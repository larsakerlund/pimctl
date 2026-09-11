// Project activation resolves requirements before invoking the existing
// activation pipeline. It never changes the caller's login or starts a dev
// environment, and never substitutes wider activation targets.

package cli

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/larsakerlund/pimctl/internal/armclient"
	"github.com/larsakerlund/pimctl/internal/config"
	"github.com/larsakerlund/pimctl/internal/term"
)

// resolveActivationProject resolves discovery and target syntax without Azure
// calls. Explicit selections retain their ordinary meaning in configured repos.
func resolveActivationProject(cmd *cobra.Command, o *activateOpts) error {
	explicit := o.all || o.preset != "" || len(o.roles) > 0 || len(o.scopes) > 0 || len(o.keys) > 0 || o.at != ""
	p, err := o.projectOpts.load(cmd, true, explicit)
	if err != nil {
		return err
	}
	o.requirements = p
	if o.at != "" {
		if err := config.ValidateScope(o.at); err != nil {
			return fmt.Errorf("--at: %w", err)
		}
		if o.all {
			return errors.New("--at cannot be combined with --all; select the eligible roles to narrow")
		}
	}
	return nil
}

// runProjectActivation checks tenant ownership, resolves all targets and starts
// per-target reconciliation after printing the chosen project and account.
func runProjectActivation(cmd *cobra.Command, rc *runContext, o *activateOpts, requested time.Duration) error {
	s, err := projectSession(cmd, rc, o.requirements.Tenant)
	if err != nil {
		return err
	}
	rows, err := readProjectRows(cmd, rc, s, o.requirements)
	if err != nil {
		return err
	}
	for _, r := range rows {
		rc.names.learn(r.Elig.Properties.Scope, r.Elig.ScopeName())
		if r.sourceScope() != r.Elig.Properties.Scope {
			fmt.Fprintf(
				cmd.ErrOrStderr(),
				"%s at %s · eligible through %s\n",
				r.Elig.RoleName(),
				r.Elig.Properties.Scope,
				r.sourceScope(),
			)
		}
	}
	future := startActivationListing(rc.Ctx, rc, targetScopes(rows))
	sp := term.NewSpinner(cmd.ErrOrStderr(), "checking existing project activations…")
	active, _ := future.Wait(-1)
	local := readLocalRecord(rc)
	active.rows = reconcileActive(rc, &local, active)
	o.verified = local.verdicts
	future = &activeFuture{got: true, res: active}
	sp.Stop()
	reportUnconfirmedScopes(cmd, rc, active.unconfirmed)
	if rc.Ctx.Err() != nil {
		return rc.Ctx.Err()
	}
	return finishActivation(cmd, rc, o, requested, rows, rows, false, future, nil)
}

// buildProjectPlan preserves verified active windows and resolves policies only
// for new requests, so a repeated up neither renews access nor asks for a reason.
func buildProjectPlan(
	rc *runContext,
	rows []row,
	requested time.Duration,
	ticket string,
	verified map[string]entryVerdict,
) []*planItem {
	var pending []row
	for _, r := range rows {
		if !projectAlreadyHeld(r, verified) {
			pending = append(pending, r)
		}
	}
	fresh := buildPlan(rc.Ctx, pending, rc.Sessions, requested, ticket, rc.Refresh)
	byKey := map[string]*planItem{}
	for _, item := range fresh {
		byKey[item.Row.Key()] = item
	}
	out := make([]*planItem, 0, len(rows))
	for _, r := range rows {
		if item := byKey[r.Key()]; item != nil {
			out = append(out, item)
		} else {
			out = append(out, &planItem{Row: r, Session: sessionFor(rc.Sessions, r.Context), KeepActive: true})
		}
	}
	return out
}

// projectPlanError prevents partial project activation when preflight already
// knows a policy lookup or required ticket failed. Runtime Azure failures still
// retain the ordinary per-role results and exit codes.
func projectPlanError(plan []*planItem) error {
	var failures []error
	for _, item := range plan {
		if item.PrepErr != nil {
			failures = append(
				failures,
				fmt.Errorf("%s at %s: %w", item.Row.Elig.RoleName(), item.Row.Elig.Properties.Scope, item.PrepErr),
			)
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("project preflight failed; no activations submitted:\n%w", errors.Join(failures...))
	}
	return nil
}

// projectAlreadyHeld accepts a live listing or a freshly verified schedule,
// checking that the reported activation window still includes the present.
func projectAlreadyHeld(r row, verified map[string]entryVerdict) bool {
	if r.Active == nil || (r.ActiveState != RowConfirmed && verified[r.SelectionKey()] != verdictHeld) {
		return false
	}
	return activationWindowCurrent(r.Active.Properties, time.Now())
}

// activationWindowCurrent checks whether an ARM activation's optional start and
// expiry bounds include now, without inferring authority from the clock alone.
func activationWindowCurrent(p armclient.AssignmentProperties, now time.Time) bool {
	return (p.StartDateTime == nil || !p.StartDateTime.After(now)) && (p.EndDateTime == nil || p.EndDateTime.After(now))
}

// activationSubmissionCount excludes preserved windows from bulk confirmation.
// A no-op project up must not prompt merely because it lists many requirements.
func activationSubmissionCount(plan []*planItem) int {
	n := 0
	for _, item := range plan {
		if !item.KeepActive {
			n++
		}
	}
	return n
}
