// Tests for the picker's behaviour under synthetic keystrokes: that typing
// filters without a mode key, that selections survive filtering, that esc
// clears before it quits, and that the header names the keys Update actually
// handles. They drive Model directly, since bubbletea's own loop needs a
// terminal — that half is covered by the expect(1) scripts under scripts/.

package picker

import (
	"strings"
	"testing"
	"unicode/utf8"

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

// resizeTo delivers the terminal size bubbletea would report after a resize.
func resizeTo(m *Model, rows int) {
	m.Update(tea.WindowSizeMsg{Width: 120, Height: rows})
}

// listRows counts the item rows in a multi-select frame: the lines that carry
// a checkbox, which excludes the title, header, filter line and footer.
func listRows(view string) int {
	n := 0
	for line := range strings.SplitSeq(view, "\n") {
		if strings.Contains(line, "[ ] ") || strings.Contains(line, "[✓] ") {
			n++
		}
	}
	return n
}

// manyItems builds n distinct rows so a window test has something to scroll.
func manyItems(n int) []Item {
	items := make([]Item, n)
	for i := range items {
		label := "role-" + strings.Repeat("x", i%3) + string(rune('a'+i%26))
		items[i] = Item{Label: label, Haystack: strings.ToLower(label)}
	}
	return items
}

// TestWindowFollowsCursorAtHeightOne is the smallest window there is: every
// move must scroll, and the one drawn row must be the cursor row.
func TestWindowFollowsCursorAtHeightOne(t *testing.T) {
	m := NewModel("one", manyItems(5), 1)
	for want := range 5 {
		if m.cursor != want || m.top != want {
			t.Fatalf("cursor=%d top=%d, want both %d", m.cursor, m.top, want)
		}
		view := m.View()
		if got := listRows(view); got != 1 {
			t.Fatalf("a height-one window drew %d rows:\n%s", got, view)
		}
		if !strings.Contains(view, m.items[m.shown[m.cursor]].Label) {
			t.Fatalf("the drawn row is not the cursor row:\n%s", view)
		}
		if !strings.Contains(view, "… 4 more") {
			t.Fatalf("the footer should always say 4 more with one row drawn:\n%s", view)
		}
		pressKey(m, "down")
	}
	for range 10 {
		pressKey(m, "up")
	}
	if m.cursor != 0 || m.top != 0 {
		t.Fatalf("after scrolling back up cursor=%d top=%d, want 0 and 0", m.cursor, m.top)
	}
}

// TestResizeShrinksThenGrowsTheWindow: a terminal too short for the requested
// height must not push the header off the top, and growing it back must
// restore the full window rather than a short tail.
func TestResizeShrinksThenGrowsTheWindow(t *testing.T) {
	m := NewModel("resize", manyItems(20), 10)
	if got := listRows(m.View()); got != 10 {
		t.Fatalf("before any resize the window should draw the requested 10 rows, got %d", got)
	}
	for range 19 {
		pressKey(m, "down")
	}
	if m.cursor != 19 || m.top != 10 {
		t.Fatalf("cursor=%d top=%d before shrink, want 19 and 10", m.cursor, m.top)
	}

	// Six terminal lines leave two for rows once the title, header, filter
	// line and footer are drawn.
	resizeTo(m, 6)
	view := m.View()
	if got := listRows(view); got != 2 {
		t.Fatalf("a 6-line terminal should draw 2 rows, got %d:\n%s", got, view)
	}
	if m.top != 18 || m.cursor != 19 {
		t.Fatalf("after shrinking cursor=%d top=%d, want 19 and 18", m.cursor, m.top)
	}
	if !strings.Contains(view, "… 18 more") {
		t.Fatalf("the footer should count against the drawn rows:\n%s", view)
	}
	if n := strings.Count(view, "\n"); n != 6 {
		t.Fatalf("the frame must fit the terminal: %d lines, want 6:\n%s", n, view)
	}

	// Shorter than the chrome alone: still one row, never zero or negative.
	resizeTo(m, 2)
	if got := listRows(m.View()); got != 1 {
		t.Fatalf("a terminal shorter than the chrome should still draw 1 row, got %d", got)
	}
	pressKey(m, "up")
	if m.top != m.cursor {
		t.Fatalf("with one row drawn the window must follow the cursor: cursor=%d top=%d", m.cursor, m.top)
	}

	// Growing back restores the requested height, and no more.
	resizeTo(m, 40)
	view = m.View()
	if got := listRows(view); got != 10 {
		t.Fatalf("a 40-line terminal should draw the requested 10 rows again, got %d:\n%s", got, view)
	}
	if m.top != 10 {
		t.Fatalf("after growing top=%d, want 10 so the window is full", m.top)
	}
	if !strings.Contains(view, m.items[m.shown[m.cursor]].Label) {
		t.Fatalf("the cursor row left the window after growing:\n%s", view)
	}
}

// TestResizeIsIgnoredBeforeAnyReport keeps the default height when bubbletea
// has not reported a size, and after a report of zero.
func TestResizeIsIgnoredBeforeAnyReport(t *testing.T) {
	m := NewModel("zero", manyItems(20), 0)
	if got := listRows(m.View()); got != 10 {
		t.Fatalf("a non-positive height should fall back to 10 rows, got %d", got)
	}
	resizeTo(m, 0)
	if got := listRows(m.View()); got != 10 {
		t.Fatalf("a zero-height report should leave the window at 10 rows, got %d", got)
	}
}

// TestFilterMatchesUnicodeRoleNames: role and scope names in a Swedish tenant
// are not ASCII, and the filter must lower-case and match them the same way.
func TestFilterMatchesUnicodeRoleNames(t *testing.T) {
	items := []Item{
		{Label: "Ägare @ Löpande (Subscription)", Haystack: "ägare @ löpande (subscription) 11aa"},
		{
			Label:    "🔑 Key Vault Administrator @ Contoso (ResourceGroup)",
			Haystack: "🔑 key vault administrator @ contoso (resourcegroup) 22bb",
		},
		{Label: "Owner @ Contoso QA (Subscription)", Haystack: "owner @ contoso qa (subscription) 33cc"},
	}
	m := NewModel("unicode", items, 10)
	typeText(m, "äg")
	if len(m.shown) != 1 || m.shown[0] != 0 {
		t.Fatalf("filtering on 'äg' shows %v, want only the Ägare row", m.shown)
	}
	pressKey(m, "esc")
	typeText(m, "ÄG")
	if len(m.shown) != 1 || m.shown[0] != 0 {
		t.Fatalf("filtering on 'ÄG' must lower-case beyond ASCII, shows %v", m.shown)
	}
	pressKey(m, "backspace")
	if m.filter != "Ä" {
		t.Fatalf("backspace must remove one rune, not one byte: filter = %q", m.filter)
	}
	pressKey(m, "esc")
	typeText(m, "löp")
	if len(m.shown) != 1 || m.shown[0] != 0 {
		t.Fatalf("filtering on 'löp' shows %v, want only the Löpande row", m.shown)
	}
	pressKey(m, "esc")
	typeText(m, "🔑")
	if len(m.shown) != 1 || m.shown[0] != 1 {
		t.Fatalf("filtering on an emoji shows %v, want only the Key Vault row", m.shown)
	}

	// Rendering draws every label whole: the picker never slices a label, so
	// no rune is cut and the frame stays valid UTF-8.
	pressKey(m, "esc")
	view := m.View()
	if !utf8.ValidString(view) {
		t.Fatalf("the frame is not valid UTF-8:\n%q", view)
	}
	for _, it := range items {
		if !strings.Contains(view, it.Label) {
			t.Errorf("label %q is not drawn intact:\n%s", it.Label, view)
		}
	}
	typeText(m, "ö")
	if got := m.View(); !strings.Contains(got, "> ö") {
		t.Errorf("the filter line does not echo the typed rune:\n%s", got)
	}
}

// assertStillEmpty fails the test if key left an empty picker anywhere but
// idle: still running, cursor and window at zero, nothing shown.
func assertStillEmpty(t *testing.T, m *Model, single bool, key string) {
	t.Helper()
	if m.confirmed || m.aborted {
		t.Fatalf("single=%v: %q ended the picker", single, key)
	}
	if m.cursor != 0 || m.top != 0 || len(m.shown) != 0 {
		t.Fatalf("single=%v: after %q cursor=%d top=%d shown=%v", single, key, m.cursor, m.top, m.shown)
	}
}

// TestEmptyModelSurvivesEveryKey: a picker over nothing — every role filtered
// out by the caller, say — must take any key without panicking, quit on the
// exit keys, and select nothing on enter.
func TestEmptyModelSurvivesEveryKey(t *testing.T) {
	keys := []string{"tab", "space", "ctrl+a", "up", "down", "ctrl+n", "ctrl+p", "backspace", "x", "ö"}
	for _, single := range []bool{false, true} {
		m := NewModel("empty", nil, 5)
		m.single = single
		for _, k := range keys {
			pressKey(m, k)
			assertStillEmpty(t, m, single, k)
		}
		resizeTo(m, 3)
		if view := m.View(); !strings.Contains(view, "nothing matches") || !strings.Contains(view, "0 of 0 shown") {
			t.Fatalf("single=%v: an empty list needs the no-match line and a zero count:\n%s", single, view)
		}
		if m.selectedCount() != 0 {
			t.Fatalf("single=%v: something got selected on an empty list", single)
		}

		// Esc clears the typed filter first, then quits.
		pressKey(m, "esc")
		if m.aborted || m.filter != "" {
			t.Fatalf(
				"single=%v: the first esc should clear the filter, aborted=%v filter=%q",
				single,
				m.aborted,
				m.filter,
			)
		}
		pressKey(m, "esc")
		if !m.aborted {
			t.Fatalf("single=%v: esc on an empty filter should quit", single)
		}
		if m.View() != "" {
			t.Fatalf("single=%v: the final frame should be empty", single)
		}
	}

	m := NewModel("empty", nil, 5)
	pressKey(m, "ctrl+c")
	if !m.aborted {
		t.Fatal("ctrl+c should abort an empty picker")
	}

	// Enter on an empty multi-select confirms an empty selection: Run then
	// returns nothing, which is the caller's decision to interpret.
	m = NewModel("empty", nil, 5)
	pressKey(m, "enter")
	if !m.confirmed || m.selectedCount() != 0 {
		t.Fatalf("enter on an empty multi-select: confirmed=%v selected=%d", m.confirmed, m.selectedCount())
	}

	// The single picker has nothing to choose, so enter is a no-op there.
	m = NewModel("empty", nil, 5)
	m.single = true
	pressKey(m, "enter")
	if m.confirmed || m.aborted {
		t.Fatal("enter on an empty single picker must neither confirm nor abort")
	}
}
