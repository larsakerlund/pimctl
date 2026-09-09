// The `status` output: the terminal table and the JSON envelope. What goes in
// them is decided in status.go, and the shared column widths, tab writer and
// duration formatting are in render.go.

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

// printStatus writes the activation table, or the JSON envelope when -o json
// is in force. unconfirmed is the ids of the scopes Azure did not answer for;
// they are labelled for a human and carried into the JSON even when there are
// none. An encoding failure is reported on stderr rather than returned — half
// a JSON document has already reached the caller by then.
func printStatus(cmd *cobra.Command, rc *runContext, rows []activeRow, unconfirmed []string) {
	if rc.Opts.json() {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		if len(unconfirmed) == 0 {
			// An always-present field is one less branch for a caller: an empty
			// array says "everything was read", where a missing key says only
			// that this pimctl did not think to mention it.
			unconfirmed = []string{}
		}
		if err := enc.Encode(statusJSON{
			Roles:             toActiveJSON(rows),
			UnconfirmedScopes: labelScopes(rc, unconfirmed),
		}); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "could not encode the output: %v\n", err)
		}
		return
	}
	printActiveTable(cmd, rows)
}

// statusJSON is an envelope rather than a bare array, so a caller can tell an
// empty tenant from a read that could not see part of it. A parser that ignores
// unconfirmedScopes and reports "no roles" would be repeating the bug this
// field exists to prevent.
type statusJSON struct {
	Roles []activeJSON `json:"roles"` // one entry per role held, or believed held.
	// UnconfirmedScopes names the scopes whose listing did not complete. Roles
	// at these scopes may be held without appearing above. The key is always
	// present, empty array and all, so a caller need not distinguish "read
	// everything" from "did not think to say".
	UnconfirmedScopes []string `json:"unconfirmedScopes"`
}

// activeJSON is one activated role as `pimctl status -o json` emits it.
//
// Confirmed and State carry the caveat the whole tool is built around: a row is
// not proof of access unless Azure listed it. A consumer that gates something
// on holding the role should require Confirmed, and read the other two states
// as "may be held" rather than as either a yes or a no.
type activeJSON struct {
	// Key is what `pimctl down --key` takes, so a script can act on a row it
	// read here without matching on names.
	Key              string `json:"key"`
	Index            int    `json:"index"`            // the printed row number; not stable between runs.
	Context          string `json:"context"`          // which cloudctx context holds it.
	Role             string `json:"role"`             // the role's display name.
	RoleDefinitionID string `json:"roleDefinitionId"` // the full ARM id.
	ScopeName        string `json:"scopeName"`        // the scope's display name, which several scopes may share.
	// ScopeLabel is scopeName as the table prints it, disambiguated: a
	// management group always carries its own name in brackets, and any other
	// scope does when its display name is shared. Several management groups in
	// one tenant can and do share a display name, so scopeName alone cannot say
	// which place a role is held at — read this when showing a row to a person,
	// and `scope` when acting on one.
	ScopeLabel string `json:"scopeLabel"`
	ScopeType  string `json:"scopeType"` // Subscription, ManagementGroup, ResourceGroup or Resource.
	Scope      string `json:"scope"`     // the full ARM scope id.
	// AssignmentType is always "Activated" here: permanent RBAC is deliberately
	// not listed, because `status` answers what PIM is holding open.
	AssignmentType string `json:"assignmentType"`
	// Since and Until are the activation window, named as in ls, up and down.
	Since *time.Time `json:"since,omitempty"`
	Until *time.Time `json:"until,omitempty"` // nil only when Azure reported no end, which permanent RBAC would.
	// Remaining is Until minus now, pre-rendered as "7h58m" for a human. A
	// script should compute from Until instead, which does not go stale.
	Remaining string `json:"remaining"`
	// Confirmed is false for any row Azure has not listed, whether because it
	// did not answer for the scope or because it has not caught up with an
	// activation made here. Treat such a row as "may be held", not as fact.
	Confirmed bool `json:"confirmed"`
	// State says which of those it is: "confirmed", "confirming" (this machine
	// activated it moments ago and Azure's listing lags by minutes),
	// or "unconfirmed" (Azure did not answer for its scope).
	State string `json:"state"`
}

// toActiveJSON renders the rows in table order, numbering Index from 1 so a
// JSON row and a printed row can be matched by eye. Remaining is measured
// against the clock at the moment of the call.
func toActiveJSON(rows []activeRow) []activeJSON {
	now := time.Now()
	// Built from every row in the answer, exactly as the table does, so one
	// scope reads the same way in both.
	scopes := scopeLabelerForActive(rows)
	out := make([]activeJSON, 0, len(rows))
	for i, r := range rows {
		out = append(out, activeJSON{
			Key:              activeSelectionKey(r),
			Index:            i + 1,
			Context:          r.Context,
			Role:             r.Assignment.RoleName(),
			RoleDefinitionID: r.Assignment.Properties.RoleDefinitionID,
			ScopeName:        r.Assignment.ScopeName(),
			ScopeLabel:       scopes.Label(r.Assignment.ScopeName(), r.Assignment.Properties.Scope),
			ScopeType:        r.Assignment.ScopeType(),
			Scope:            r.Assignment.Properties.Scope,
			AssignmentType:   r.Assignment.Properties.AssignmentType,
			Since:            r.Assignment.Properties.StartDateTime,
			Until:            r.Assignment.Properties.EndDateTime,
			Remaining:        formatRemaining(r.Assignment.Properties.EndDateTime, now),
			Confirmed:        !r.Unconfirmed(),
			State:            r.State.String(),
		})
	}
	return out
}

// printActiveTable prints the activation table, a legend for whichever
// unconfirmed markers appear in it, and the count.
//
// No rows prints "No roles are currently activated." That sentence is only the
// whole truth when every scope was read; the scopes that were not are named
// separately by reportUnconfirmedScopes, so the two must be read together.
func printActiveTable(cmd *cobra.Command, rows []activeRow) {
	out := cmd.OutOrStdout()
	if len(rows) == 0 {
		fmt.Fprintln(out, "No roles are currently activated.")
		return
	}
	now := time.Now()
	multi := multipleActiveContexts(rows)
	roleW, scopeW := columnWidths(multi)
	scopes := scopeLabelerForActive(rows)

	w := newTabWriter(out)
	header := []string{"#", headRole, headScope, headType, "UNTIL", "REMAINING"}
	if multi {
		header = slices.Insert(header, 1, headContext)
	}
	fmt.Fprintln(w, strings.Join(header, "\t"))
	for i, r := range rows {
		until := "-"
		if e := r.Assignment.Properties.EndDateTime; e != nil {
			until = e.Local().Format(timeFormat)
		}
		remaining := formatRemaining(r.Assignment.Properties.EndDateTime, now)
		if m := r.State.marker(); m != "" {
			remaining += " " + m
		}
		fields := []string{
			strconv.Itoa(i + 1),
			armclient.TruncateMiddle(r.Assignment.RoleName(), roleW),
			armclient.TruncateMiddle(scopes.Label(r.Assignment.ScopeName(), r.Assignment.Properties.Scope), scopeW),
			r.Assignment.ScopeType(),
			until,
			remaining,
		}
		if multi {
			fields = slices.Insert(fields, 1, r.Context)
		}
		fmt.Fprintln(w, strings.Join(fields, "\t"))
	}
	w.Flush()
	fmt.Fprintf(out, "\n%d activated role(s).\n", len(rows))
	if slices.ContainsFunc(rows, func(r activeRow) bool { return r.State == RowUnconfirmed }) {
		fmt.Fprintln(out, "? = from this machine's own record; Azure did not answer for that scope.")
	}
	if slices.ContainsFunc(rows, func(r activeRow) bool { return r.State == RowConfirming }) {
		fmt.Fprintln(out, "~ = activated from this machine just now; Azure's listing has yet to catch up.")
	}
}
