// Tests for the picker's behaviour under synthetic keystrokes: that typing
// filters without a mode key, that selections survive filtering, that esc
// clears before it quits, and that the header names the keys Update actually
// handles. They drive Model directly, since bubbletea's own loop needs a
// terminal — that half is covered by the expect(1) scripts under scripts/.

package picker

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func pressKey(m *Model, s string) {
	var msg tea.KeyMsg
	switch s {
	case "enter":
		msg = tea.KeyMsg{Type: tea.KeyEnter}
	case "tab":
		msg = tea.KeyMsg{Type: tea.KeyTab}
	case "esc":
		msg = tea.KeyMsg{Type: tea.KeyEsc}
	case "space":
		msg = tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
	case "backspace":
		msg = tea.KeyMsg{Type: tea.KeyBackspace}
	case "up":
		msg = tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		msg = tea.KeyMsg{Type: tea.KeyDown}
	case "ctrl+a":
		msg = tea.KeyMsg{Type: tea.KeyCtrlA}
	case "ctrl+c":
		msg = tea.KeyMsg{Type: tea.KeyCtrlC}
	case "ctrl+n":
		msg = tea.KeyMsg{Type: tea.KeyCtrlN}
	case "ctrl+p":
		msg = tea.KeyMsg{Type: tea.KeyCtrlP}
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
	m.Update(msg)
}

func typeText(m *Model, s string) {
	for _, r := range s {
		pressKey(m, string(r))
	}
}

func pickerFixture() *Model {
	items := []Item{
		{
			Label:    "Contributor @ Contoso landing zones (contoso-prod) (ManagementGroup)",
			Haystack: "contributor @ contoso landing zones (contoso-prod) (managementgroup) aa11bb22",
		},
		{
			Label:    "Cost Management Contributor @ Contoso landing zones (contoso-prod) (ManagementGroup)",
			Haystack: "cost management contributor @ contoso landing zones (contoso-prod) (managementgroup) cc33dd44",
		},
		{
			Label:    "Cost Management Contributor @ Contoso landing zones (contoso-test) (ManagementGroup)",
			Haystack: "cost management contributor @ contoso landing zones (contoso-test) (managementgroup) ee55ff66",
		},
		{Label: "Owner @ Contoso QA (Subscription)", Haystack: "owner @ contoso qa (subscription) 11223344"},
	}
	return NewModel("Eligible Azure resource roles (4)", items, 10)
}

// TestTypingFiltersDirectly is the whole point of replacing huh: no "/" first.
func TestTypingFiltersDirectly(t *testing.T) {
	m := pickerFixture()
	if len(m.shown) != 4 {
		t.Fatalf("expected all 4 rows before filtering, got %d", len(m.shown))
	}
	typeText(m, "cost")
	if len(m.shown) != 2 {
		t.Fatalf("typing 'cost' should narrow to 2 rows, got %d", len(m.shown))
	}
	if m.filter != "cost" {
		t.Errorf("filter = %q", m.filter)
	}
	// Case-insensitive.
	m = pickerFixture()
	typeText(m, "COST")
	if len(m.shown) != 2 {
		t.Fatalf("filtering must be case-insensitive, got %d rows", len(m.shown))
	}
}

func TestFilterMatchesScopeTypeAndKey(t *testing.T) {
	m := pickerFixture()
	typeText(m, "subscription")
	if len(m.shown) != 1 {
		t.Fatalf("filtering on a scope type gave %d rows, want 1", len(m.shown))
	}

	m = pickerFixture()
	typeText(m, "ee55")
	if len(m.shown) != 1 {
		t.Fatalf("filtering on a selection key gave %d rows, want 1", len(m.shown))
	}

	m = pickerFixture()
	typeText(m, "contoso-test")
	if len(m.shown) != 1 {
		t.Fatalf("filtering on a scope leaf gave %d rows, want 1", len(m.shown))
	}
}

func TestBackspaceWidensTheFilter(t *testing.T) {
	m := pickerFixture()
	typeText(m, "cost")
	pressKey(m, "backspace")
	pressKey(m, "backspace")
	pressKey(m, "backspace")
	pressKey(m, "backspace")
	if m.filter != "" {
		t.Fatalf("filter = %q, want empty", m.filter)
	}
	if len(m.shown) != 4 {
		t.Fatalf("clearing the filter should restore all rows, got %d", len(m.shown))
	}
	// Backspace on an empty filter is harmless.
	pressKey(m, "backspace")
	if m.filter != "" || len(m.shown) != 4 {
		t.Error("backspace on an empty filter should do nothing")
	}
}

func TestTabTogglesAndSelectionSurvivesFiltering(t *testing.T) {
	m := pickerFixture()
	typeText(m, "cost")
	pressKey(m, "tab") // Select the first Cost row.
	if m.selectedCount() != 1 {
		t.Fatalf("tab should have selected one row, got %d", m.selectedCount())
	}

	// Change the filter so the selected row is no longer shown.
	for range 4 {
		pressKey(m, "backspace")
	}
	typeText(m, "owner")
	if len(m.shown) != 1 {
		t.Fatalf("filter 'owner' shows %d rows", len(m.shown))
	}
	if m.selectedCount() != 1 {
		t.Fatalf("a selection must survive a filter change, got %d selected", m.selectedCount())
	}

	// Confirm: the hidden selection must still be returned.
	pressKey(m, "enter")
	if !m.confirmed {
		t.Fatal("enter should confirm")
	}
	var got []int
	for i := range m.items {
		if m.items[i].selected {
			got = append(got, i)
		}
	}
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("confirmed selection = %v, want the Cost row hidden by the filter", got)
	}
}

func TestSpaceTogglesOnlyWhenTheFilterIsEmpty(t *testing.T) {
	m := pickerFixture()
	pressKey(m, "space")
	if m.selectedCount() != 1 {
		t.Fatalf("space with an empty filter should toggle, got %d selected", m.selectedCount())
	}

	m = pickerFixture()
	typeText(m, "cost")
	before := m.selectedCount()
	pressKey(m, "space")
	if m.selectedCount() != before {
		t.Error("space while filtering must type a space, not toggle")
	}
	if m.filter != "cost " {
		t.Errorf("filter = %q, want a trailing space", m.filter)
	}
}

func TestCtrlATogglesEveryMatchingRow(t *testing.T) {
	m := pickerFixture()
	typeText(m, "cost")
	pressKey(m, "ctrl+a")
	if m.selectedCount() != 2 {
		t.Fatalf("ctrl+a should select the 2 matching rows, got %d", m.selectedCount())
	}
	// Rows outside the filter are untouched.
	if m.items[0].selected || m.items[3].selected {
		t.Error("ctrl+a must not select rows hidden by the filter")
	}
	// Again clears them.
	pressKey(m, "ctrl+a")
	if m.selectedCount() != 0 {
		t.Fatalf("ctrl+a again should clear the matching rows, got %d", m.selectedCount())
	}
}

func TestCursorMovementKeys(t *testing.T) {
	for _, keys := range [][2]string{{"down", "up"}, {"ctrl+n", "ctrl+p"}} {
		m := pickerFixture()
		pressKey(m, keys[0])
		if m.cursor != 1 {
			t.Errorf("%s should move down, cursor = %d", keys[0], m.cursor)
		}
		pressKey(m, keys[1])
		if m.cursor != 0 {
			t.Errorf("%s should move up, cursor = %d", keys[1], m.cursor)
		}
		// Cannot run off either end.
		pressKey(m, keys[1])
		if m.cursor != 0 {
			t.Errorf("cursor went above the first row: %d", m.cursor)
		}
		for range 10 {
			pressKey(m, keys[0])
		}
		if m.cursor != len(m.shown)-1 {
			t.Errorf("cursor went past the last row: %d of %d", m.cursor, len(m.shown))
		}
	}
}

// TestEscClearsThenQuits: a mistyped filter must never throw away a selection.
func TestEscClearsThenQuits(t *testing.T) {
	m := pickerFixture()
	typeText(m, "cost")
	pressKey(m, "esc")
	if m.filter != "" {
		t.Fatalf("the first esc should clear the filter, got %q", m.filter)
	}
	if m.aborted {
		t.Fatal("the first esc must not quit")
	}
	pressKey(m, "esc")
	if !m.aborted {
		t.Fatal("esc on an empty filter should quit")
	}
}

func TestCtrlCAlwaysQuits(t *testing.T) {
	m := pickerFixture()
	typeText(m, "cost")
	pressKey(m, "ctrl+c")
	if !m.aborted || m.confirmed {
		t.Fatal("ctrl+c must abort even with a filter Active")
	}
}

func TestPickerViewShowsCountsAndMarks(t *testing.T) {
	m := pickerFixture()
	typeText(m, "cost")
	pressKey(m, "tab")
	view := m.View()
	if !strings.Contains(view, "1 selected") {
		t.Errorf("the selected count is missing:\n%s", view)
	}
	if !strings.Contains(view, "2 of 4 shown") {
		t.Errorf("the shown/total count is wrong:\n%s", view)
	}
	if !strings.Contains(view, "✓") {
		t.Errorf("a selected row should carry a tick:\n%s", view)
	}
}

func TestPickerHandlesNoMatches(t *testing.T) {
	m := pickerFixture()
	typeText(m, "zzzznothing")
	if len(m.shown) != 0 {
		t.Fatalf("expected no matches, got %d", len(m.shown))
	}
	view := m.View()
	if !strings.Contains(view, "nothing matches") {
		t.Errorf("an empty result needs an explanation:\n%s", view)
	}
	// Toggling with nothing shown must not panic or select anything.
	pressKey(m, "tab")
	pressKey(m, "ctrl+a")
	if m.selectedCount() != 0 {
		t.Error("nothing should be selectable when nothing matches")
	}
	// And backspacing out recovers.
	for range 11 {
		pressKey(m, "backspace")
	}
	if len(m.shown) != 4 {
		t.Fatalf("recovered %d rows, want 4", len(m.shown))
	}
}

// TestCursorStaysOnTheSameItemAcrossFiltering keeps the highlight meaningful
// while the user types.
func TestCursorStaysOnTheSameItemAcrossFiltering(t *testing.T) {
	m := pickerFixture()
	pressKey(m, "down")
	pressKey(m, "down") // Third item: Cost @ contoso-test.
	want := m.shown[m.cursor]
	typeText(m, "cost")
	if m.shown[m.cursor] != want {
		t.Errorf("cursor jumped to item %d, want it to stay on %d", m.shown[m.cursor], want)
	}
}

// TestPickerHeaderDescribesTheRealKeys guards the header against drifting from
// what the picker actually binds.
func TestPickerHeaderDescribesTheRealKeys(t *testing.T) {
	m := NewModel("Eligible roles (3)", []Item{
		{Label: "a", Haystack: "a"}, {Label: "b", Haystack: "b"}, {Label: "c", Haystack: "c"},
	}, 10)
	view := m.View()
	for _, want := range []string{"type to filter", "tab toggle", "ctrl+a all", "enter confirm", "0 selected", "3 of 3 shown"} {
		if !strings.Contains(view, want) {
			t.Errorf("the header omits %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "/ filter") {
		t.Error("filtering no longer needs a leading '/'; the header must not say so")
	}
}

func TestSingleChoiceUsesHighlightedFilteredItem(t *testing.T) {
	m := pickerFixture()
	m.single = true
	typeText(m, "cost")
	pressKey(m, "down")
	pressKey(m, "ctrl+a")
	pressKey(m, "tab")
	if m.selectedCount() != 0 {
		t.Fatal("single picker permits multiselection")
	}
	pressKey(m, "enter")
	if !m.confirmed || m.selectedCount() != 1 || !m.items[2].selected {
		t.Fatal("did not choose highlighted filtered item")
	}
	if m.View() != "" {
		t.Fatal("did not erase frame")
	}
}

func TestSingleChoiceNoMatchDoesNotFinish(t *testing.T) {
	m := pickerFixture()
	m.single = true
	typeText(m, "no-such-scope")
	pressKey(m, "enter")
	if m.confirmed {
		t.Fatal("accepted empty match")
	}
	pressKey(m, "esc")
	if m.aborted || len(m.shown) != 4 {
		t.Fatal("escape did not clear filter")
	}
	pressKey(m, "esc")
	if !m.aborted {
		t.Fatal("escape did not cancel")
	}
}
