// What the user sees of a run: the plan table before it, one line per role as
// each lands, and afterwards the results — a table, or a count when every row
// has already gone past on the same screen. The JSON these paths emit is
// shaped by [result] in plan.go, not here.

package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/spf13/cobra"

	"github.com/larsakerlund/pimctl/internal/armclient"
	"github.com/larsakerlund/pimctl/internal/term"
)

// printPlanTable renders the confirmation table for `up`: one numbered row per
// planned role, showing the duration that will actually be requested rather
// than the one the user asked for. Notes — a capped duration, an approval
// requirement, a role already active, a policy that could not be read — are
// printed under the table, because they do not fit a column that still leaves
// room for the role and the scope.
//
// multiContext adds the CONTEXT column, which only earns its width when the run
// spans more than one context. scopes is the labeler shared by every table in
// the run, so a scope reads the same here as in the results.
func printPlanTable(out io.Writer, plan []*planItem, multiContext bool, scopes scopeLabeler) {
	fmt.Fprintln(out, "About to activate:")
	roleW, scopeW := columnWidths(multiContext)
	w := newTabWriter(out)
	header := []string{"#", headRole, headScope, headType, "DURATION"}
	if multiContext {
		header = slices.Insert(header, 1, headContext)
	}
	fmt.Fprintln(w, strings.Join(header, "\t"))
	// Notes are printed under their row instead of in a column: "capped to the
	// policy maximum PT4H; already active" does not fit any column that leaves
	// room for the role and scope, and truncating it would hide the reason.
	notes := make([]string, len(plan))
	for i, item := range plan {
		var rowNotes []string
		switch {
		case item.PrepErr != nil:
			rowNotes = append(rowNotes, "cannot activate: "+item.PrepErr.Error())
		default:
			if item.Capped {
				rowNotes = append(
					rowNotes,
					fmt.Sprintf("capped to the policy maximum %s", item.Settings.MaximumDurationISO),
				)
			}
			if item.Settings != nil && item.Settings.ApprovalRequired {
				rowNotes = append(rowNotes, "approval required")
			}
			if item.Row.IsActive() {
				rowNotes = append(rowNotes, "already active")
			}
		}
		notes[i] = strings.Join(rowNotes, "; ")
		dur := "-"
		if item.PrepErr == nil && !item.KeepActive {
			dur = armclient.FormatISODuration(item.Duration)
		}
		fields := []string{
			strconv.Itoa(i + 1),
			armclient.TruncateMiddle(item.Row.Elig.RoleName(), roleW),
			armclient.TruncateMiddle(scopes.Label(item.Row.Elig.ScopeName(), item.Row.Elig.Properties.Scope), scopeW),
			item.Row.Elig.ScopeType(),
			dur,
		}
		if multiContext {
			fields = slices.Insert(fields, 1, item.Row.Context)
		}
		fmt.Fprintln(w, strings.Join(fields, "\t"))
	}
	w.Flush()
	for i, item := range plan {
		if notes[i] != "" {
			printNote(out, fmt.Sprintf("%d. %s — %s", i+1, item.Row.Elig.RoleName(), notes[i]))
		}
	}
	fmt.Fprintln(out)
}

// printDeactivatePlan is the same table for `down`. It has an UNTIL column
// rather than a DURATION one, and says "not listed" for a role the activation
// listing did not show, rather than implying it is definitely held.
func printDeactivatePlan(out io.Writer, rows []target, multiContext bool, scopes scopeLabeler) {
	fmt.Fprintln(out, "About to deactivate:")
	roleW, scopeW := columnWidths(multiContext)
	w := newTabWriter(out)
	header := []string{"#", headRole, headScope, headType, "UNTIL"}
	if multiContext {
		header = slices.Insert(header, 1, headContext)
	}
	fmt.Fprintln(w, strings.Join(header, "\t"))
	for i, r := range rows {
		until := "-"
		if r.EndDateTime != nil {
			until = r.EndDateTime.Local().Format(timeFormat)
		} else if !r.SeenActive {
			// The listing did not show this one; say so rather than implying it
			// is definitely held.
			until = "not listed"
		}
		fields := []string{
			strconv.Itoa(i + 1),
			armclient.TruncateMiddle(r.RoleName, roleW),
			armclient.TruncateMiddle(scopes.Label(r.ScopeName, r.Scope), scopeW),
			r.ScopeType,
			until,
		}
		if multiContext {
			fields = slices.Insert(fields, 1, r.Context)
		}
		fmt.Fprintln(w, strings.Join(fields, "\t"))
	}
	w.Flush()
	fmt.Fprintln(out)
}

// onEachResult composes the always-on record write with the terminal-only
// progress line.
//
// The record write must not be gated on anything: an earlier version wrote the
// record after the results table, in a call site that a later refactor
// replaced — so the writes became dead code and every `status` after an `up`
// reported the new roles as not existing until ARM caught up. Hanging it off
// the same per-role callback as the progress line keeps it on the path that
// cannot be refactored away without noticing.
func onEachResult(stream func(result)) func(result) {
	return func(r result) {
		recordResult(r)
		if stream != nil {
			stream(r)
		}
	}
}

// streamProgress returns a per-role reporter for a terminal, and nil otherwise.
// Streaming to a pipe would interleave with the table that follows and give a
// parser two representations of the same run.
//
// Each line is printed with the spinner paused, so a completed role does not
// land in the middle of the spinner's own line.
func streamProgress(
	cmd *cobra.Command,
	g *globalOpts,
	roles int,
	sp *term.Spinner,
	scopes scopeLabeler,
) func(result) {
	if g.json() || !term.StderrIsTTY() {
		return nil
	}
	w := cmd.ErrOrStderr()
	pal := term.PaletteFor(w)
	var remaining atomic.Int64
	remaining.Store(int64(roles))
	return func(r result) {
		left := remaining.Add(-1)
		sp.Pause(func() { streamResult(w, pal, scopes, r) })
		if left > 0 {
			sp.Update(fmt.Sprintf("%s to go…", roleCount(int(left))))
		}
	}
}

// streamResult prints one role's outcome the moment it lands.
//
// A batch of five roles takes as long as its slowest, and printing nothing
// until all five finish makes a working command look hung. Streaming turns the
// wait into visible progress.
//
// The scope is named with the same labeler the tables use, so one place reads
// the same everywhere: a subscription that is "Contoso QA" in the table three
// lines below must not be "33333333" here.
func streamResult(w io.Writer, pal term.Palette, scopes scopeLabeler, r result) {
	detail := r.Detail
	if detail != "" {
		detail = " — " + detail
	}
	until := ""
	if r.Until != nil {
		until = " until " + r.Until.Local().Format(timeFormat)
	}
	fmt.Fprintf(w, "%s  %s @ %s%s%s\n",
		renderOutcome(pal, r.Outcome), r.Role, scopes.Label(r.ScopeName, r.Scope), until, detail)
}

// streamedTo reports whether the reader has already seen every row go past on
// this same screen.
//
// Both halves matter: stream is nil when nothing was streamed, and stdout being
// redirected means the streamed lines went to a terminal while the table is
// going to a file, which still wants the full table in it.
func streamedTo(stream func(result)) bool {
	return stream != nil && term.StdoutIsTTY()
}

// withScopeLabels fills each result's ScopeLabel from the run's labeler.
//
// It is done here rather than where the result is built because the labeler is
// derived from every eligible role, not just the selection — the same reason
// `ls` and `status` label their rows at the point of printing. A result built
// in isolation cannot know whether its scope's display name is ambiguous.
func withScopeLabels(results []result, scopes scopeLabeler) []result {
	out := make([]result, len(results))
	for i, r := range results {
		r.ScopeLabel = scopes.Label(r.ScopeName, r.Scope)
		out[i] = r
	}
	return out
}

// printResults prints the results table, or the JSON array under -o json, or
// nothing but a count when every row has already been streamed past.
//
// The JSON branch encodes the results as they stand apart from the scope label,
// so what it emits is [result]'s public shape and nothing this function decides;
// the table is free to truncate and re-order, the JSON is not.
func printResults(
	cmd *cobra.Command,
	g *globalOpts,
	results []result,
	multiContext, streamed bool,
	scopes scopeLabeler,
) error {
	out := cmd.OutOrStdout()
	if g.json() {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(withScopeLabels(results, scopes))
	}
	// Every row has already gone past on the same screen, one line at a time,
	// with more detail than the table carries. Printing the table again says
	// nothing new — and on a deactivation it says it worse, with an "UNTIL -"
	// column that means nothing. A count is what is left to add.
	if streamed {
		printResultSummary(out, results)
		printRecoveries(out, results)
		return nil
	}
	roleW, scopeW := columnWidths(multiContext)
	pal := term.PaletteFor(out)
	w := newTabWriter(out)
	// RESULT goes last because it is the coloured cell: text/tabwriter counts
	// ANSI escape bytes as width, so a coloured cell in the middle would
	// misalign every column after it.
	header := []string{"#", headRole, headScope, headType, "UNTIL", "RESULT"}
	if multiContext {
		header = slices.Insert(header, 1, headContext)
	}
	fmt.Fprintln(w, strings.Join(header, "\t"))
	for i, r := range results {
		until := "-"
		if r.Until != nil {
			until = r.Until.Local().Format(timeFormat)
		}
		fields := []string{
			strconv.Itoa(i + 1),
			armclient.TruncateMiddle(r.Role, roleW),
			armclient.TruncateMiddle(scopes.Label(r.ScopeName, r.Scope), scopeW),
			r.ScopeType,
			until,
			renderOutcome(pal, r.Outcome),
		}
		if multiContext {
			fields = slices.Insert(fields, 1, r.Context)
		}
		fmt.Fprintln(w, strings.Join(fields, "\t"))
	}
	w.Flush()
	// Details carry the ARM error code and the recovery advice; they are the
	// most important text on screen and must never be truncated.
	for i, r := range results {
		if r.Detail != "" {
			printNote(out, fmt.Sprintf("%d. %s — %s", i+1, r.Role, r.Detail))
		}
	}
	printRecoveries(out, results)
	return nil
}

// resultOrder is the order outcomes are summarised in: what went right first,
// then what needs the reader to do something.
var resultOrder = []outcome{
	OutcomeActivated, OutcomeDeactivated, OutcomeAlreadyActive, OutcomeNotActive,
	OutcomeSubmitted, OutcomePending, OutcomeWaiting, OutcomeFailed,
	OutcomeAborted, OutcomeSkipped,
}

// printResultSummary renders the one-line count that replaces the table when
// every row has already been streamed: "2 deactivated", "3 activated, 1 failed".
func printResultSummary(out io.Writer, results []result) {
	counts := map[outcome]int{}
	for _, r := range results {
		counts[r.Outcome]++
	}
	parts := make([]string, 0, len(counts))
	for _, o := range resultOrder {
		if n := counts[o]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, strings.ToLower(string(o))))
		}
	}
	// An outcome not in the list above is still worth printing; being missed by
	// a summary is how a new outcome silently disappears.
	for o, n := range counts {
		if !slices.Contains(resultOrder, o) {
			parts = append(parts, fmt.Sprintf("%d %s", n, strings.ToLower(string(o))))
		}
	}
	if len(parts) == 0 {
		return
	}
	fmt.Fprintln(out, strings.Join(parts, ", "))
}

// printRecoveries prints the runnable re-authentication command for any role
// that failed a claims challenge. It survives the summary path: it is advice
// the streamed lines do not carry, and it is the only way out of that failure.
func printRecoveries(out io.Writer, results []result) {
	for _, r := range results {
		if r.Recovery != "" {
			fmt.Fprintf(
				out,
				"\nConditional Access wants a stepped-up token for %s. Re-authenticate with:\n  %s\n",
				r.Role,
				r.Recovery,
			)
		}
	}
}

// summariseFailures is the one-line message printed alongside a non-zero exit.
func summariseFailures(results []result) string {
	// The same labeler the tables use: a scope that reads "Contoso landing zones
	// (contoso-prod)" in the results must not become a bare "contoso-prod" in the line
	// explaining why the command failed.
	refs := make([]scopeRef, 0, len(results))
	for _, r := range results {
		refs = append(refs, scopeRef{Name: r.ScopeName, ID: r.Scope})
	}
	scopes := newScopeLabeler(refs)

	var failed, unfinished, pending []string
	for _, r := range results {
		label := r.Role + " @ " + scopes.Label(r.ScopeName, r.Scope)
		switch {
		case r.IsFailure():
			failed = append(failed, label)
		case r.IsUnfinished():
			unfinished = append(unfinished, label)
		case r.IsPending():
			pending = append(pending, label)
		}
	}
	var parts []string
	if len(failed) > 0 {
		parts = append(parts, fmt.Sprintf("%d role(s) failed: %s", len(failed), strings.Join(failed, ", ")))
	}
	if len(unfinished) > 0 {
		parts = append(
			parts,
			fmt.Sprintf(
				"%d role(s) did not reach a final status in time: %s",
				len(unfinished),
				strings.Join(unfinished, ", "),
			),
		)
	}
	if len(parts) > 0 {
		return strings.Join(parts, "; ")
	}
	return fmt.Sprintf("%d role(s) are waiting for approval: %s", len(pending), strings.Join(pending, ", "))
}

// roleCount renders "1 role" / "3 roles".
func roleCount(n int) string {
	if n == 1 {
		return "1 role"
	}
	return strconv.Itoa(n) + " roles"
}
