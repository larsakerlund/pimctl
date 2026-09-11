// `pimctl status`: choosing between the instant answer from this machine's
// record and Azure's authoritative one, and reporting where the two differ.
// The table and the JSON envelope are in statusview.go.

package cli

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/larsakerlund/pimctl/internal/term"
)

// newStatusCmd builds `pimctl status`.
//
// --fast and --wait ask for opposite things and are rejected together. Neither
// is the default: a terminal gets this machine's record straight away and a
// correction a couple of seconds later, while piped output and -o json block
// for Azure, because a reader that has already consumed the output cannot see
// a correction printed after it.
func newStatusCmd(opts *globalOpts, d deps) *cobra.Command {
	var fast, wait bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the roles you currently have activated",
		Long: `Show the roles you currently hold.

Reading activation state from Azure means one request per scope you are
eligible at, which takes a couple of seconds. On a terminal pimctl prints what
this machine activated straight away, marked as unconfirmed, then reconciles
against Azure and reports the difference — including anything activated
elsewhere, such as in the portal. Rows it has not confirmed are marked "?".

Azure's per-scope listing lags its own writes by minutes (4.5 observed), so what
pimctl did here outranks that listing until the role's own schedule request says
otherwise: a fresh activation is shown marked "~" while its request reports the
role provisioned, and a role given up here stays off the table while Azure still
reports it. A 30-minute ceiling bounds both, for an activation ARM cannot be
asked about. Nothing is ever dropped silently — a role that goes is named.

Piped output and -o json wait for Azure, because a caller cannot see a later
correction. --fast opts into the local answer there too, and --wait forces the
blocking read anywhere.`,
		Example: `  pimctl status
  pimctl status --wait                 # block for the authoritative answer
  pimctl status -o json | jq '.roles[].remaining'`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateFlags(opts); err != nil {
				return err
			}
			if fast && wait {
				return errors.New("--fast and --wait ask for opposite things")
			}
			rc, err := prepare(cmd, opts, d, nil)
			if err != nil {
				return err
			}
			// A caller that cannot see a later correction gets the thorough
			// read: the snappiness budget is for the interactive path.
			rc.Wait = wait || (!fast && (!term.StdoutIsTTY() || opts.json()))
			defer rc.finish(cmd)

			_, listErrs := runStatus(cmd, rc, fast, wait)
			if abortedEarly(cmd.Context()) {
				return cmd.Context().Err()
			}
			failures := slices.Concat(rc.Failures, listErrs)
			reportContextFailures(cmd.ErrOrStderr(), failures)
			if len(failures) > 0 {
				return partialFailureError(failures)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&fast, "fast", false,
		"print this machine's own record without waiting for Azure, even when not on a terminal")
	cmd.Flags().BoolVar(&wait, "wait", false,
		"wait for Azure's authoritative answer before printing anything")
	return cmd
}

// runStatus decides between the instant local answer and the authoritative one,
// and does the reconciliation reporting.
func runStatus(cmd *cobra.Command, rc *runContext, fast, wait bool) ([]activeRow, []error) {
	// Anything not going to a terminal has one chance to be right: a reader
	// cannot see a correction printed a second later. So piped output and JSON
	// wait for Azure unless --fast says otherwise.
	interactive := term.StdoutIsTTY() && !rc.Opts.json()
	blocking := wait || !interactive && !fast

	if blocking {
		// Read the record before reconciliation rewrites it: what this machine
		// believed is the only thing a correction can be measured against.
		local := readLocalRecord(rc)
		sp := term.NewSpinner(cmd.ErrOrStderr(), "reading active roles…")
		active, listErrs, slow := listActivations(rc.Ctx, rc, nil)
		sp.Stop()
		local.verdicts = verifyConfirming(rc.Ctx, rc, active, slow)
		merged := mergeActive(local, active, slow)
		for _, s := range rc.Sessions {
			reconcileRecord(s.owner(), active, slow, local.verdicts)
		}
		printStatus(cmd, rc, merged, slow)
		reportUnconfirmedScopes(cmd, rc, slow)
		reportRecordDrops(cmd, local, active, slow)
		return merged, listErrs
	}

	local := readLocalRecord(rc)
	printStatus(cmd, rc, local.rows, nil)

	// Reconcile in the background and report only the difference, so the fast
	// answer is never silently wrong.
	future := startActivationListing(rc.Ctx, rc, nil)
	res, ok := future.Wait(-1)
	if !ok {
		return local.rows, res.errs
	}
	local.verdicts = verifyConfirming(rc.Ctx, rc, res.rows, res.unconfirmed)
	merged := mergeActive(local, res.rows, res.unconfirmed)
	for _, s := range rc.Sessions {
		reconcileRecord(s.owner(), res.rows, res.unconfirmed, local.verdicts)
	}
	reportStatusDelta(cmd, rc, local, res.rows, res.unconfirmed)
	return merged, res.errs
}

// reportStatusDelta prints what Azure disagreed with, and nothing when it
// agrees — the common case should be silent.
//
// Only scopes Azure answered, and roles this machine did not just change, can
// disagree with anything: a role still propagating is not a role gained or lost,
// and saying so would undo the whole point of confirming it separately.
func reportStatusDelta(
	cmd *cobra.Command,
	rc *runContext,
	local localRecord,
	active []activeRow,
	unconfirmed []activationScope,
) {
	added, dropped := statusDelta(local, active, unconfirmed)
	armCount := 0
	for _, r := range active {
		if !local.denies(r) {
			armCount++
		}
	}

	errw := cmd.ErrOrStderr()
	reportUnconfirmedScopes(cmd, rc, unconfirmed)
	switch {
	case len(added) == 0 && len(dropped) == 0:
		fmt.Fprintf(errw, "confirmed %d against Azure\n", armCount)
	default:
		fmt.Fprintf(errw, "confirmed %d against Azure", armCount-len(added))
		if len(added) > 0 {
			fmt.Fprintf(errw, ", +%d activated elsewhere: %s", len(added), strings.Join(added, ", "))
		}
		fmt.Fprintln(errw)
		if len(dropped) > 0 {
			fmt.Fprintln(errw, lostRolesLine(dropped))
		}
		fmt.Fprintln(errw, "run `pimctl status` again for the corrected table")
	}
}

// reportRecordDrops says what the authoritative table no longer contains.
//
// The blocking path prints the right answer, but a role vanishing between two
// runs is exactly the thing a user needs told rather than left to notice: it may
// be an expiry, a colleague's deactivation, or an activation that never took.
// Nothing is said when nothing was lost — the table speaks for itself.
func reportRecordDrops(cmd *cobra.Command, local localRecord, active []activeRow, unconfirmed []activationScope) {
	if _, dropped := statusDelta(local, active, unconfirmed); len(dropped) > 0 {
		fmt.Fprintln(cmd.ErrOrStderr(), lostRolesLine(dropped))
	}
}

// statusDelta compares what this machine believed with what Azure reported.
func statusDelta(local localRecord, active []activeRow, unconfirmed []activationScope) (added, dropped []string) {
	scopes := scopeLabelerForActive(slices.Concat(local.rows, active))
	localKeys := map[string]activeRow{}
	for _, r := range local.rows {
		localKeys[activeSelectionKey(r)] = r
	}
	armKeys := map[string]activeRow{}
	for _, r := range active {
		armKeys[activeSelectionKey(r)] = r
	}

	for k, r := range armKeys {
		if local.denies(r) {
			continue // given up here; ARM's listing has not caught up.
		}
		if _, ok := localKeys[k]; !ok {
			added = append(added, describeRow(scopes, r))
		}
	}
	// A role is only "no longer held" if the record no longer stands: Azure
	// answered for its scope, and either it had already listed the role or the
	// role's own request says it is gone. Anything else would report a loss
	// that never happened — but a real loss must never go unsaid, which is why
	// this runs on the blocking path too.
	unknown := unreadScopes(unconfirmed)
	for k, r := range localKeys {
		if _, ok := armKeys[k]; ok {
			continue
		}
		if local.stands(r, scopeIsUnread(unknown, r.Context, r.Assignment.Properties.Scope)) {
			continue
		}
		dropped = append(dropped, describeRow(scopes, r))
	}
	slices.Sort(added)
	slices.Sort(dropped)
	return added, dropped
}

// describeRow names a role and its scope for a one-line report, the same way
// the tables do.
func describeRow(scopes scopeLabeler, r activeRow) string {
	return r.Assignment.RoleName() + " @ " +
		scopes.Label(r.Assignment.ScopeName(), r.Assignment.Properties.Scope)
}

// lostRolesLine is the one wording for "a role you held is gone", used by both
// the fast and the blocking path. Two wordings for one event meant anything
// watching for the sentence — a script, an agent, a person scanning back
// through a terminal — could only recognise half the cases.
func lostRolesLine(dropped []string) string {
	return fmt.Sprintf("%d role(s) this machine had activated are no longer held: %s",
		len(dropped), strings.Join(dropped, ", "))
}
