// `pimctl list` / `ls`: the eligible-role table and its JSON. Reading the
// eligibilities is eligible.go, and the Azure-side answer for the ACTIVE
// column comes from active.go.

package cli

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/larsakerlund/pimctl/internal/armclient"
)

// newListCmd builds `pimctl list` (alias `ls`).
//
// The eligibility listing is cached and cheap; the activation listing is the
// per-scope fan-out that costs seconds, so the ACTIVE column is filled from
// this machine's own record and the command says on stderr that it did.
// --with-active waits for Azure's answer instead.
func newListCmd(opts *globalOpts, d deps) *cobra.Command {
	var withActive bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the Azure resource roles you are eligible for",
		Example: `  pimctl list
  pimctl ls --refresh
  pimctl list -o json | jq -r '.[] | select(.role=="Owner") | .key'`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateFlags(opts); err != nil {
				return err
			}
			rc, err := prepare(cmd, opts, d, nil)
			if err != nil {
				return err
			}
			defer rc.finish(cmd)
			rows, listErrs, future := readEligibilities(rc.Ctx, cmd, rc)
			if abortedEarly(cmd.Context()) {
				return cmd.Context().Err()
			}
			failures := slices.Concat(rc.Failures, listErrs)

			// The eligibility list is already in hand; the activation listing
			// is a per-scope fan-out that takes seconds. Do not hold the table
			// hostage to it — fill ACTIVE from this machine's own record, which
			// is free, and wait for Azure only when the caller asks.
			res, ok := future.TryGet()
			if withActive {
				res, ok = future.Wait(-1)
			}
			if ok {
				failures = append(failures, res.errs...)
				rows = applyActive(rows, res.rows)
				reportUnconfirmedScopes(cmd, rc, res.unconfirmed)
			} else {
				rows = applyLocalRecord(rows, rc)
			}
			reportContextFailures(cmd.ErrOrStderr(), failures)
			if !ok {
				fmt.Fprintln(
					cmd.ErrOrStderr(),
					"ACTIVE shows this machine's own record; run `pimctl status`, or `pimctl ls --with-active`, for Azure's answer",
				)
			}

			if opts.json() {
				if err := printRowsJSON(cmd, rows); err != nil {
					return err
				}
			} else {
				printRowsTable(cmd, rows)
			}
			if len(failures) > 0 {
				return partialFailureError(failures)
			}
			return nil
		},
	}
	cmd.Flags().
		BoolVar(&withActive, "with-active", false, "wait for the (slow) activation listing so the ACTIVE column is filled in")
	return cmd
}

// rowJSON is one eligible role as `pimctl list -o json` emits it. The field
// names are a public contract — a caller's jq expression is written against
// them — and are spelled the same as in status, up and down wherever they mean
// the same thing.
type rowJSON struct {
	// Key is stable across runs; Index is not, so scripts should use Key.
	Key              string `json:"key"`
	Index            int    `json:"index"`            // the printed row number, for reading alongside the table.
	Context          string `json:"context"`          // which cloudctx context the eligibility was read from.
	Role             string `json:"role"`             // the role's display name.
	RoleDefinitionID string `json:"roleDefinitionId"` // the full ARM id; only its trailing GUID is the role's identity.
	ScopeName        string `json:"scopeName"`        // the scope's display name, which several scopes may share.
	// ScopeLabel is scopeName as the tables print it, disambiguated: a
	// management group always carries its own name in brackets, and any other
	// scope does when its display name is shared. Several management groups in
	// one tenant can and do share a display name, so scopeName alone cannot say
	// which place a row is about — read this when showing a row to a person,
	// and `scope` when acting on one.
	ScopeLabel string `json:"scopeLabel"`
	ScopeType  string `json:"scopeType"` // Subscription, ManagementGroup, ResourceGroup or Resource.
	Scope      string `json:"scope"`     // the full ARM scope id: unambiguous, and what to act on.
	// MemberType is "Direct" or "Group", which is how a row says the
	// eligibility was inherited rather than granted to you by name.
	MemberType    string `json:"memberType"`
	PrincipalName string `json:"principalName"` // the group the eligibility came through, when it came through one.
	// RoleEligibilityScheduleID is what an activation must send back verbatim
	// as linkedRoleEligibilityScheduleId.
	RoleEligibilityScheduleID string `json:"roleEligibilityScheduleId"`
	// EligibleUntil is when the eligibility itself lapses — a different thing
	// from Until, which is when an activation of it ends.
	EligibleUntil *time.Time `json:"eligibleUntil,omitempty"`
	// AlsoVia lists the other member types the same role and scope was eligible
	// through, when duplicates were folded into one row.
	AlsoVia []string `json:"alsoEligibleVia,omitempty"`
	// Active comes from this machine's own record unless --with-active was
	// given, so it can be stale for a role activated elsewhere.
	Active bool `json:"active"`
	// Until is the end of the current activation, named the same here as in
	// status, up and down so one jq expression works across all four.
	Until *time.Time `json:"until,omitempty"`
}

// toRowJSON renders the rows in listing order, numbering Index from 1 so a
// JSON row and a printed row can be matched by eye.
func toRowJSON(rows []row) []rowJSON {
	// Built from every row in the answer, exactly as the table does, so one
	// scope reads the same way in both.
	scopes := scopeLabelerForRows(rows)
	out := make([]rowJSON, 0, len(rows))
	for i, r := range rows {
		out = append(out, rowJSON{
			Index:                     i + 1,
			Context:                   r.Context,
			Role:                      r.Elig.RoleName(),
			RoleDefinitionID:          r.Elig.Properties.RoleDefinitionID,
			ScopeName:                 r.Elig.ScopeName(),
			ScopeLabel:                scopes.Label(r.Elig.ScopeName(), r.Elig.Properties.Scope),
			ScopeType:                 r.Elig.ScopeType(),
			Scope:                     r.Elig.Properties.Scope,
			MemberType:                r.Elig.Properties.MemberType,
			PrincipalName:             r.Elig.Properties.ExpandedProperties.Principal.DisplayName,
			RoleEligibilityScheduleID: r.Elig.Properties.RoleEligibilityScheduleID,
			EligibleUntil:             r.Elig.Properties.EndDateTime,
			Key:                       r.SelectionKey(),
			AlsoVia:                   r.AlsoVia,
			Active:                    r.IsActive(),
			Until:                     r.ActiveUntil(),
		})
	}
	return out
}

// printRowsJSON writes the rows to stdout as an indented JSON array, and
// returns the encoder's error.
//
// A bare array rather than an envelope, unlike statusJSON: eligibility is read
// per context in one call, so there is no partial answer to describe. A context
// that could not be read is a named failure on stderr and a non-zero exit, not
// a silently short array.
func printRowsJSON(cmd *cobra.Command, rows []row) error {
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(toRowJSON(rows))
}

// printRowsTable prints the eligible roles as the terminal table, with the
// scope column disambiguated after truncation rather than before.
func printRowsTable(cmd *cobra.Command, rows []row) {
	out := cmd.OutOrStdout()
	if len(rows) == 0 {
		fmt.Fprintln(out, "No eligible Azure resource roles.")
		return
	}
	multi := multipleContexts(rows)
	roleW, scopeW := columnWidths(multi)
	scopes := scopeLabelerForRows(rows)
	// Truncation can manufacture ambiguity that the labeler did not: two
	// distinct subscriptions whose names differ only past the cut render as the
	// same string. Disambiguate after truncating, not before.
	scopeCells := truncatedScopeCells(rows, scopes, scopeW)

	w := newTabWriter(out)
	header := []string{"#", "KEY", headRole, headScope, headType, "ACTIVE"}
	if multi {
		header = slices.Insert(header, 1, headContext)
	}
	fmt.Fprintln(w, strings.Join(header, "\t"))
	for i, r := range rows {
		active := ""
		if r.IsActive() {
			// "until <date> <time>" without a leading "yes": the column is
			// headed ACTIVE and a value in it already means yes. The four
			// characters mattered — with the full date they pushed the widest
			// real row to 122 columns, two over the budget.
			active = "yes"
			if until := r.ActiveUntil(); until != nil {
				active = "until " + until.Local().Format(timeFormat)
			}
		}
		fields := []string{
			strconv.Itoa(i + 1),
			r.SelectionKey(),
			armclient.TruncateMiddle(r.Elig.RoleName(), roleW),
			scopeCells[i],
			r.Elig.ScopeType(),
		}
		fields = append(fields, active)
		if multi {
			fields = slices.Insert(fields, 1, r.Context)
		}
		fmt.Fprintln(w, strings.Join(fields, "\t"))
	}
	w.Flush()
	fmt.Fprintf(out, "\n%d eligible role(s).\n", len(rows))
}

// applyLocalRecord marks rows this machine believes it activated, so the ACTIVE
// column is useful immediately. Azure remains the authority: `status` and
// `--with-active` reconcile.
func applyLocalRecord(rows []row, rc *runContext) []row {
	local := localActiveRows(rc)
	if len(local) == 0 {
		return rows
	}
	return applyActive(rows, local)
}
