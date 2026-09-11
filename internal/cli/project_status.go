// Project status shows every requirement, including absent and unknown ones.
// It reconciles the additive local record against Azure before filtering the
// display; it never deletes unrelated recorded activations.

package cli

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/larsakerlund/pimctl/internal/armclient"
	"github.com/larsakerlund/pimctl/internal/cache"
	"github.com/larsakerlund/pimctl/internal/config"
	"github.com/larsakerlund/pimctl/internal/term"
)

// projectRequirementStatus describes exact PIM activation evidence, not proof
// that an application will accept a credential or allow every operation.
type projectRequirementStatus struct {
	Role             string     `json:"role"`             // Display name when known, otherwise the definition UUID.
	RoleDefinitionID string     `json:"roleDefinitionId"` // Required definition UUID.
	Scope            string     `json:"scope"`            // Requested activation target.
	State            string     `json:"state"`            // active, confirming, unconfirmed, unknown, or not active.
	Until            *time.Time `json:"until,omitempty"`  // Known expiry of the exact activation.
}

// projectStatusJSON extends ordinary status with explicit missing requirements.
type projectStatusJSON struct {
	Requirements      []projectRequirementStatus `json:"requirements"`      // One result per configured target.
	Roles             []activeJSON               `json:"roles"`             // Exact matching activations only.
	UnconfirmedScopes []string                   `json:"unconfirmedScopes"` // Every incomplete reconciliation scope.
}

// runProjectStatus reads target scopes and every recorded scope, preserving
// record correctness before rendering only project requirements. A fast answer
// remains provisional and is always reconciled against Azure behind the scenes.
func runProjectStatus(cmd *cobra.Command, rc *runContext, p *config.Project, fast, wait bool) error {
	s, err := projectSession(cmd, rc, p.Tenant)
	if err != nil {
		return err
	}
	scopes := projectScopes(p, s)
	local := readLocalRecord(rc)
	instant := fast || (term.StdoutIsTTY() && !rc.Opts.json() && !wait)
	if instant {
		printProjectStatus(cmd, rc, p, s, local.rows, scopes)
	}
	sp := term.NewSpinner(cmd.ErrOrStderr(), "checking project activations…")
	active, failures, slow := listActivations(rc.Ctx, rc, scopes)
	if abortedEarly(rc.Ctx) {
		sp.Stop()
		return rc.Ctx.Err()
	}
	merged := reconcileActive(rc, &local, activeResult{rows: active, unconfirmed: slow})
	sp.Stop()
	if !instant {
		printProjectStatus(cmd, rc, p, s, merged, slow)
	} else {
		reportProjectDelta(cmd, p, s, local.rows, scopes, merged, slow)
	}
	reportUnconfirmedScopes(cmd, rc, slow)
	if len(slow) > 0 {
		failures = append(failures, fmt.Errorf("project activation state is unknown at %d scope(s)", len(slow)))
	}
	if len(failures) > 0 {
		return partialFailureError(failures)
	}
	return nil
}

// projectView matches exact scope/role identities and labels absent rows as
// unknown whenever Azure did not answer. Parent activations are never presented
// as if the requested narrow activation exists.
func projectView(
	p *config.Project,
	s *session,
	rows []activeRow,
	slow []activationScope,
) ([]projectRequirementStatus, []activeRow) {
	entries := projectEntries(p, s)
	requirements := make([]projectRequirementStatus, 0, len(entries))
	var matching []activeRow
	for i, e := range entries {
		state := "not active"
		if slices.ContainsFunc(slow, func(scope activationScope) bool {
			return scope.Context == s.Token.Label() && (scope.ID == "" || strings.EqualFold(scope.ID, e.Scope))
		}) {
			state = "unknown"
		}
		view := projectRequirementStatus{
			Role:             e.RoleName,
			RoleDefinitionID: p.Roles[i].RoleDefinitionID,
			Scope:            e.Scope,
			State:            state,
		}
		for _, r := range rows {
			if rowKey(
				r.Context,
				r.Assignment.Properties.Scope,
				r.Assignment.RoleDefinitionGUID(),
			) != rowKey(
				s.Token.Label(),
				e.Scope,
				p.Roles[i].RoleDefinitionID,
			) {
				continue
			}
			view.Role, view.Until = r.Assignment.RoleName(), r.Assignment.Properties.EndDateTime
			view.State = r.State.String()
			if !r.Unconfirmed() {
				view.State = "active"
			}
			matching = append(matching, r)
			break
		}
		requirements = append(requirements, view)
	}
	return requirements, matching
}

// printProjectStatus renders all requirements and exact matching activations.
// Broader ancestors are reported separately without asserting effective access.
func printProjectStatus(
	cmd *cobra.Command,
	rc *runContext,
	p *config.Project,
	s *session,
	rows []activeRow,
	slow []activationScope,
) {
	requirements, matching := projectView(p, s, rows, slow)
	reportBroaderActivations(cmd, p, s, rows)
	if rc.Opts.json() {
		unconfirmed := labelScopes(rc, slow)
		if unconfirmed == nil {
			unconfirmed = []string{}
		}
		if err := encodeJSON(
			cmd.OutOrStdout(),
			projectStatusJSON{
				Requirements:      requirements,
				Roles:             toActiveJSON(matching),
				UnconfirmedScopes: unconfirmed,
			},
		); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "could not encode project status: %v\n", err)
		}
		return
	}
	w := newTabWriter(cmd.OutOrStdout())
	fmt.Fprintln(w, "ROLE\tTARGET SCOPE\tSTATE\tREMAINING")
	for _, r := range requirements {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Role, r.Scope, r.State, formatRemaining(r.Until, time.Now()))
	}
	w.Flush()
}

// reportBroaderActivations names known parent activations without asserting
// condition coverage. Cached scoped eligibilities provide management-group
// ancestry evidence; their names are never treated as proof.
func reportBroaderActivations(cmd *cobra.Command, p *config.Project, s *session, rows []activeRow) {
	for _, r := range rows {
		for _, need := range p.Roles {
			if r.Context == s.Token.Label() &&
				strings.EqualFold(r.Assignment.RoleDefinitionGUID(), need.RoleDefinitionID) &&
				knownProjectAncestor(s, need, r.Assignment.Properties.Scope) {
				fmt.Fprintf(
					cmd.ErrOrStderr(),
					"Broader activation also reported: %s at %s (%s); project down leaves it alone.\n",
					r.Assignment.RoleName(),
					r.Assignment.Properties.Scope,
					r.State.String(),
				)
				break
			}
		}
	}
}

// reportProjectDelta reconciles a fast preview with one line per changed
// requirement. It keeps stdout JSON singular and avoids a second terminal table.
func reportProjectDelta(
	cmd *cobra.Command,
	p *config.Project,
	s *session,
	before []activeRow,
	beforeSlow []activationScope,
	after []activeRow,
	afterSlow []activationScope,
) {
	old, _ := projectView(p, s, before, beforeSlow)
	fresh, _ := projectView(p, s, after, afterSlow)
	for i, r := range fresh {
		if r.State == old[i].State && equalProjectExpiry(r.Until, old[i].Until) {
			continue
		}
		fmt.Fprintf(
			cmd.ErrOrStderr(),
			"Project status: %s at %s · %s · %s remaining\n",
			r.Role,
			r.Scope,
			r.State,
			formatRemaining(r.Until, time.Now()),
		)
	}
	reportBroaderActivations(cmd, p, s, after)
}

// equalProjectExpiry compares optional expiry timestamps without pointer identity.
func equalProjectExpiry(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// knownProjectAncestor recognises lexical resource parents and cached ARM
// ancestry evidence. A cache miss means unknown, not proof of no broader access.
func knownProjectAncestor(s *session, need config.ProjectRole, parent string) bool {
	if strings.EqualFold(parent, need.Scope) {
		return false
	}
	if strings.HasPrefix(strings.ToLower(need.Scope), strings.ToLower(parent)+"/") {
		return true
	}
	roles, ok := cache.ReadScoped(s.owner(), need.Scope)
	if !ok {
		return false
	}
	return slices.ContainsFunc(
		roles,
		func(e armclient.Eligibility) bool { return strings.EqualFold(e.Properties.Scope, parent) },
	)
}
