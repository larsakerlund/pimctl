// The glue between the commands and the interactive widgets: the role and
// activation pickers, the justification prompt and the y/N confirm. Both draw
// inline and erase themselves, so what survives in scrollback is the one
// summary line [reportSelection] leaves. The picker itself is internal/picker;
// whether a run is allowed to prompt at all is confirm.go.

package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/huh"

	"github.com/larsakerlund/pimctl/internal/picker"
	"github.com/larsakerlund/pimctl/internal/term"
)

// multiSelectHeight is how many rows the picker shows at once. With well over a
// hundred eligible roles the list is always filtered rather than scrolled, so a
// tall viewport matters less than leaving the filter line visible.
const multiSelectHeight = 18

// runPicker shows the picker and turns a cancelled selection into pimctl's
// interrupted exit code, which is what walking away from the picker means to
// the process that called it.
func runPicker(title string, items []picker.Item, height int) ([]int, error) {
	picked, err := picker.Run(title, items, height)
	if errors.Is(err, picker.ErrCancelled) {
		return nil, &exitCodeError{code: ExitInterrupted, msg: err.Error()}
	}
	return picked, err
}

// selectInteractive shows a type-to-filter multi-select of every eligible role.
// Already-active roles stay in the list but are dimmed, because re-activating
// one to extend it is legitimate.
func selectInteractive(rows []row, multiContext bool, scopes scopeLabeler) ([]row, error) {
	labels := itemLabels(rows, multiContext, scopes)
	items := make([]picker.Item, 0, len(rows))
	for i, r := range rows {
		items = append(items, picker.Item{
			Label:    labels[i],
			Haystack: pickerHaystack(r, labels[i]),
			Active:   r.IsActive(),
		})
	}
	picked, err := runPicker(fmt.Sprintf("Eligible Azure resource roles (%d)", len(rows)), items, multiSelectHeight)
	if err != nil {
		return nil, err
	}
	out := make([]row, 0, len(picked))
	for _, i := range picked {
		out = append(out, rows[i])
	}
	reportSelection(len(out))
	return out, nil
}

// reportSelection leaves the one line the erased picker owes to scrollback.
func reportSelection(n int) {
	if !term.StderrIsTTY() {
		return
	}
	pal := term.PaletteFor(os.Stderr)
	fmt.Fprintf(os.Stderr, "%s Selected %s\n", pal.Wrap(term.Green, "✓"), roleCount(n))
}

// itemLabel renders one eligible role as a single line for the interactive
// multi-select. Filtering matches against this whole line, so it carries the
// role, the scope and the scope type; the role name leads because that is what
// a filter is usually typed against.
func itemLabel(r row, multiContext bool, scopes scopeLabeler) string {
	var b strings.Builder
	if multiContext {
		fmt.Fprintf(&b, "[%s] ", r.Context)
	}
	b.WriteString(r.Elig.RoleName())
	b.WriteString(" @ ")
	b.WriteString(scopes.Label(r.Elig.ScopeName(), r.Elig.Properties.Scope))
	fmt.Fprintf(&b, " (%s)", r.Elig.ScopeType())
	if r.IsActive() {
		b.WriteString("  • ACTIVE")
		if until := r.ActiveUntil(); until != nil {
			fmt.Fprintf(&b, " until %s", until.Local().Format(timeFormat))
		}
	}
	return b.String()
}

// itemLabels renders every row for the picker, guaranteeing the labels are
// unique.
//
// This matters for correctness, not looks: huh identifies a MultiSelect option
// by its label string, and its toggle handler flips *every* option whose label
// matches the one under the cursor. Two rows sharing a label would therefore
// toggle together — one keypress silently selecting a scope the user never
// pointed at. Scope disambiguation makes collisions unlikely; the numeric
// suffix makes them impossible.
func itemLabels(rows []row, multiContext bool, scopes scopeLabeler) []string {
	out := make([]string, len(rows))
	seen := map[string]int{}
	for i, r := range rows {
		label := itemLabel(r, multiContext, scopes)
		seen[label]++
		if n := seen[label]; n > 1 {
			label = fmt.Sprintf("%s #%d", label, n)
		}
		out[i] = label
	}
	return out
}

// pickerHaystack is everything a filter should match: the rendered label plus
// the scope type and the selection key, so "managementgroup" or a key prefix
// both narrow the list.
func pickerHaystack(r row, label string) string {
	return strings.ToLower(strings.Join([]string{
		label,
		r.Elig.RoleName(),
		r.Elig.ScopeName(),
		r.Elig.Properties.Scope,
		r.Elig.ScopeType(),
		r.SelectionKey(),
		r.Context,
	}, " "))
}

// selectInteractiveActive shows a multi-select of the roles currently activated.
func selectInteractiveActive(rows []activeRow, multiContext bool, scopes scopeLabeler) ([]activeRow, error) {
	items := make([]picker.Item, 0, len(rows))
	seen := map[string]int{}
	for _, r := range rows {
		label := r.Assignment.RoleName() + " @ " +
			scopes.Label(r.Assignment.ScopeName(), r.Assignment.Properties.Scope) +
			fmt.Sprintf(" (%s)", r.Assignment.ScopeType())
		if multiContext {
			label = "[" + r.Context + "] " + label
		}
		if e := r.Assignment.Properties.EndDateTime; e != nil {
			label += "  • until " + e.Local().Format(timeFormat)
		}
		seen[label]++
		if n := seen[label]; n > 1 {
			label = fmt.Sprintf("%s #%d", label, n)
		}
		items = append(items, picker.Item{
			Label: label,
			Haystack: strings.ToLower(strings.Join([]string{
				label, r.Assignment.RoleName(), r.Assignment.ScopeName(),
				r.Assignment.Properties.Scope, r.Assignment.ScopeType(), r.Context,
			}, " ")),
			Active: true,
		})
	}
	picked, err := runPicker(fmt.Sprintf("Activated roles (%d)", len(rows)), items, multiSelectHeight)
	if err != nil {
		return nil, err
	}
	reportSelection(len(picked))
	out := make([]activeRow, 0, len(picked))
	for _, i := range picked {
		out = append(out, rows[i])
	}
	return out, nil
}

// promptJustification asks for the justification, prefilled with the last one
// used so repeat activations are a single keypress.
func promptJustification(prefill, subtitle string) (string, error) {
	value := prefill
	description := "sent to PIM with every activation in this run"
	if subtitle != "" {
		description = "justification " + subtitle
	}
	field := huh.NewInput().
		Title("Justification").
		Description(description).
		Value(&value)
	if err := field.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return "", errors.New("cancelled")
		}
		return "", err
	}
	return value, nil
}

// confirm asks a yes/no question, defaulting to no.
func confirm(title string) (bool, error) {
	ok := false
	field := huh.NewConfirm().Title(title).Affirmative("Yes").Negative("No").Value(&ok)
	if err := field.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return false, nil
		}
		return false, err
	}
	return ok, nil
}
