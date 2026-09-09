// The shared shape of pimctl's output: table geometry, the headings and glyphs
// every command uses, and the one way it prints an instant. What goes in the
// tables is each command's own business — report.go, list.go, statusview.go —
// and nothing here writes to a stream it was not handed.

package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/larsakerlund/pimctl/internal/term"
)

// planWriter picks where the human-readable confirmation table goes. Under
// `-o json` it must not land on stdout, or the JSON that follows is unparseable.
func planWriter(cmd *cobra.Command, g *globalOpts) io.Writer {
	if g.json() {
		return cmd.ErrOrStderr()
	}
	return cmd.OutOrStdout()
}

// timeFormat is the one way pimctl prints an instant. A bare "15:04" saves
// five characters and costs the reader the question "today?", which for a role
// that expires overnight is the question that matters.
const timeFormat = "2006-01-02 15:04"

// formatRemaining renders the time left on an activation, e.g. "58m" or
// "3h12m". A past or missing end time yields "-".
func formatRemaining(end *time.Time, now time.Time) string {
	if end == nil {
		return "-"
	}
	d := end.Sub(now)
	if d <= 0 {
		return "expired"
	}
	d = d.Round(time.Minute)
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	if h > 0 {
		return fmt.Sprintf("%dh%02dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

// tableWidth is the console budget every table has to fit inside.
const tableWidth = 120

// outputJSON is the value of -o/--output that switches every command over to
// machine-readable output.
const outputJSON = "json"

// The table headings shared by every command's output.
const (
	headRole    = "ROLE"    // the role's display name.
	headContext = "CONTEXT" // printed only when a run spans more than one context.
	headScope   = "SCOPE"   // the scope label, disambiguated where display names collide.
	headType    = "TYPE"    // Subscription, ManagementGroup, ResourceGroup or Resource.
)

// columnWidths returns the role and scope truncation widths that keep a table
// inside tableWidth. Dropping the context column when there is only one context
// buys most of the room back.
func columnWidths(multiContext bool) (role, scope int) {
	if multiContext {
		return 26, 24 //nolint:mnd // the two column budgets that fit tableWidth with a context column
	}
	return 30, 30 //nolint:mnd // ... and without one
}

// The tabwriter geometry every table shares: no minimum cell width, four-space
// tab stops and two spaces of padding between columns.
const (
	tabMinWidth = 0 // no minimum cell width; the columns are already budgeted.
	tabWidth    = 4 // tab stop, which only matters for a cell containing a tab.
	tabPadding  = 2 // spaces between columns: enough to read, cheap in width.
)

// newTabWriter returns a tabwriter with the geometry above, so every table in
// pimctl lines its columns up the same way. The caller must Flush it.
func newTabWriter(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, tabMinWidth, tabWidth, tabPadding, ' ', 0)
}

// printNote writes a wrapped continuation line under a table row. Notes and
// error details are the part of a result a user most needs to read in full, so
// they get their own lines rather than being truncated into a column.
func printNote(w io.Writer, note string) {
	if note == "" {
		return
	}
	const indent = "     "
	const width = tableWidth - len(indent) - 2
	for _, line := range wrapText(note, width) {
		fmt.Fprintf(w, "%s%s\n", indent, line)
	}
}

// wrapText breaks s into lines of at most width runes, on word boundaries where
// it can.
func wrapText(s string, width int) []string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}
	var (
		lines []string
		cur   []rune
	)
	for _, word := range words {
		w := []rune(word)
		switch {
		case len(cur) == 0:
			cur = w
		case len(cur)+1+len(w) <= width:
			cur = append(append(cur, ' '), w...)
		default:
			lines = append(lines, string(cur))
			cur = w
		}
		// A single word longer than the budget still has to be broken.
		for len(cur) > width {
			lines = append(lines, string(cur[:width]))
			cur = cur[width:]
		}
	}
	if len(cur) > 0 {
		lines = append(lines, string(cur))
	}
	return lines
}

// outcomeGlyph pairs each result with a marker that survives losing colour:
// the glyph carries the meaning, the colour only reinforces it.
func outcomeGlyph(o outcome) string {
	switch o {
	case OutcomeActivated, OutcomeDeactivated:
		return "✓"
	case OutcomeAlreadyActive:
		return "•"
	case OutcomePending, OutcomeSubmitted:
		return "⧗"
	case OutcomeWaiting, OutcomeAborted, OutcomeSkipped:
		return "…"
	case OutcomeFailed:
		return "✗"
	}
	return " "
}

// renderOutcome renders a result cell, coloured when the destination allows.
func renderOutcome(pal term.Palette, o outcome) string {
	text := outcomeGlyph(o) + " " + string(o)
	switch o {
	case OutcomeActivated, OutcomeDeactivated:
		return pal.Wrap(term.Green, text)
	case OutcomePending, OutcomeSubmitted, OutcomeWaiting:
		return pal.Wrap(term.Yellow, text)
	case OutcomeFailed, OutcomeAborted, OutcomeSkipped:
		return pal.Wrap(term.Red, text)
	case OutcomeAlreadyActive:
		return pal.Wrap(term.Dim, text)
	}
	return text
}
