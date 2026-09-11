// Resolve exact activation targets through the caller's scope-aware Azure
// eligibilities. The source schedule stays intact; these helpers never widen
// a failed target or infer management-group ancestry from string prefixes.

package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/larsakerlund/pimctl/internal/armclient"
	"github.com/larsakerlund/pimctl/internal/cache"
	"github.com/larsakerlund/pimctl/internal/config"
	"github.com/larsakerlund/pimctl/internal/term"
)

// scopedEligibilities reads complete target evidence from the account-owned
// cache or ARM. A failed request never becomes an empty or cached success.
func scopedEligibilities(ctx context.Context, s *session, scope string, refresh bool) ([]armclient.Eligibility, error) {
	if !refresh {
		if roles, ok := cache.ReadScoped(s.owner(), scope); ok {
			return roles, nil
		}
	}
	var roles []armclient.Eligibility
	err := retryOn401(s, func() error {
		var readErr error
		roles, readErr = s.Client.ListEligibilitiesAtScope(ctx, scope, s.Token.PrincipalID)
		return readErr
	})
	if err != nil {
		return nil, fmt.Errorf("could not verify eligible access at %s: %w", scope, err)
	}
	cache.WriteScoped(s.owner(), scope, roles)
	return roles, nil
}

// eligibleNow excludes expired and future schedules before source selection.
func eligibleNow(e armclient.Eligibility, now time.Time) bool {
	p := e.Properties
	return (p.StartDateTime == nil || !p.StartDateTime.After(now)) &&
		(p.EndDateTime == nil || p.EndDateTime.After(now)) && !armclient.IsFailureStatus(p.Status)
}

// targetedRow keeps the source scope and schedule while assigning target
// identity to output, active-state matching and activation records.
func targetedRow(s *session, e armclient.Eligibility, scope string) row {
	r := row{Context: s.Token.Label(), Elig: e, EligibilityScope: e.Properties.Scope}
	r.Elig.Properties.Scope = scope
	if !strings.EqualFold(scope, e.Properties.Scope) {
		r.Elig.Properties.ExpandedProperties.Scope = armclient.Named{
			ID: scope, DisplayName: scopeLeaf(scope), Type: armclient.NormalizeScopeType("", scope),
		}
	}
	return r
}

// chooseScopedEligibility selects an unambiguous source for one requirement.
// An explicit schedule pins --at to the eligibility the user selected. Exact
// scope sources take precedence; competing inherited or conditional grants
// are diagnosed rather than arbitrarily choosing a policy or constraint.
func chooseScopedEligibility(
	roles []armclient.Eligibility,
	roleID, targetScope, schedule string,
) (armclient.Eligibility, error) {
	var candidates, exact []armclient.Eligibility
	seen := map[string]bool{}
	for _, e := range roles {
		if !strings.EqualFold(e.RoleDefinitionGUID(), armclient.RoleDefinitionGUID(roleID)) ||
			!eligibleNow(e, time.Now()) {
			continue
		}
		if schedule != "" && !strings.EqualFold(schedule, e.Properties.RoleEligibilityScheduleID) {
			continue
		}
		id := strings.ToLower(e.Properties.RoleEligibilityScheduleID)
		if seen[id] {
			continue
		}
		seen[id] = true
		candidates = append(candidates, e)
		if strings.EqualFold(e.Properties.Scope, targetScope) {
			exact = append(exact, e)
		}
	}
	if len(exact) > 0 {
		candidates = exact
	}
	candidates = equivalentSources(candidates)
	switch len(candidates) {
	case 0:
		return armclient.Eligibility{}, fmt.Errorf(
			"no matching eligible role %s found at or above %s",
			roleID,
			targetScope,
		)
	case 1:
		return candidates[0], nil
	default:
		return armclient.Eligibility{}, fmt.Errorf(
			"role %s at %s has %d eligible source schedules; use ordinary up --key KEY --at SCOPE for the preferred source shown by pimctl ls",
			roleID,
			targetScope,
			len(candidates),
		)
	}
}

// readProjectRows resolves every required role before returning any selection.
// Missing eligibility and failed verification both prevent all submissions.
func readProjectRows(cmd *cobra.Command, rc *runContext, s *session, p *config.Project) ([]row, error) {
	sp := term.NewSpinner(cmd.ErrOrStderr(), "resolving project access…")
	defer sp.Stop()
	out := make([]row, 0, len(p.Roles))
	var failures []error
	byScope := map[string][]armclient.Eligibility{}
	for _, need := range p.Roles {
		roles, ok := byScope[strings.ToLower(need.Scope)]
		if !ok {
			var err error
			roles, err = scopedEligibilities(rc.Ctx, s, need.Scope, rc.Refresh)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			byScope[strings.ToLower(need.Scope)] = roles
		}
		e, err := chooseScopedEligibility(roles, need.RoleDefinitionID, need.Scope, "")
		if err != nil {
			failures = append(failures, err)
			continue
		}
		out = append(out, targetedRow(s, e, need.Scope))
	}
	if len(failures) > 0 {
		return nil, fmt.Errorf(
			"project access could not be resolved; no activations submitted:\n%w",
			errors.Join(failures...),
		)
	}
	return out, nil
}

// narrowRows verifies that each selected source can activate at scope. It
// returns no partial selection when a target is outside a source's ancestry.
func narrowRows(cmd *cobra.Command, rc *runContext, rows []row, scope string) ([]row, error) {
	sp := term.NewSpinner(cmd.ErrOrStderr(), "verifying activation scope…")
	defer sp.Stop()
	out := make([]row, 0, len(rows))
	for _, r := range rows {
		s := sessionFor(rc.Sessions, r.Context)
		roles, err := scopedEligibilities(rc.Ctx, s, scope, rc.Refresh)
		if err != nil {
			return nil, err
		}
		e, err := chooseScopedEligibility(
			roles,
			r.Elig.RoleDefinitionGUID(),
			scope,
			r.Elig.Properties.RoleEligibilityScheduleID,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"cannot activate %s at %s; no activations submitted: %w",
				r.Elig.RoleName(),
				scope,
				err,
			)
		}
		out = append(out, targetedRow(s, e, scope))
	}
	return dedupeRows(out), nil
}

// targetScopes gathers actual activation targets for background reconciliation.
func targetScopes(rows []row) []activationScope {
	var scopes []activationScope
	seen := map[string]bool{}
	for _, r := range rows {
		s := activationScope{Context: r.Context, ID: r.Elig.Properties.Scope}
		if !seen[s.key()] {
			scopes = append(scopes, s)
			seen[s.key()] = true
		}
	}
	return scopes
}

// equivalentSources folds schedules with identical scope and conditions because
// they share a role policy. Direct membership wins, then the stable schedule ID;
// different constraints or granting scopes remain explicitly ambiguous.
func equivalentSources(roles []armclient.Eligibility) []armclient.Eligibility {
	byGrant := map[string]armclient.Eligibility{}
	for _, e := range roles {
		p := e.Properties
		key := strings.ToLower(p.Scope) + "|" + p.ConditionVersion + "|" + p.Condition
		old, exists := byGrant[key]
		if !exists || preferSource(e, old) {
			byGrant[key] = e
		}
	}
	out := make([]armclient.Eligibility, 0, len(byGrant))
	for _, e := range byGrant {
		out = append(out, e)
	}
	return out
}

// preferSource deterministically chooses among equivalent eligibility schedules.
func preferSource(a, b armclient.Eligibility) bool {
	aDirect, bDirect := a.Properties.MemberType == "Direct", b.Properties.MemberType == "Direct"
	if aDirect != bDirect {
		return aDirect
	}
	return a.Properties.RoleEligibilityScheduleID < b.Properties.RoleEligibilityScheduleID
}
