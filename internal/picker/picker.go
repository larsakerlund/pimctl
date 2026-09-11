// The picker's whole implementation: the model, the key handling, the view and
// the Run wrapper that drives bubbletea. The package comment, and the reasoning
// for not using huh or bubbles/list, are in doc.go.

package picker

import (
	"errors"
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/larsakerlund/pimctl/internal/term"
)

// ErrCancelled is returned by [Run] when the user leaves the picker without
// confirming, with esc on an empty filter or with ctrl+c. It is not a failure:
// callers report it as "cancelled" and exit without touching Azure, which is
// why it is distinguishable from a real bubbletea error.
var ErrCancelled = errors.New("selection cancelled")

// Item is one row offered to the user. Label and Haystack are separate on
// purpose: the filter matches text the row never shows — a scope id, a role
// GUID — so typing part of a subscription id finds a row labelled with its
// display name. The zero value is a blank row that every filter matches.
type Item struct {
	// Label is what is drawn, already truncated and padded by the caller.
	Label string
	// Haystack is everything the filter matches against, pre-lowercased so the
	// match is a plain strings.Contains on every keystroke.
	Haystack string
	// selected is the toggle state, unexported so the only way to learn it is
	// the index slice Run returns.
	selected bool
	// Active marks a role the user already holds; such rows are dimmed rather
	// than hidden, because re-activating one is a legitimate way to extend it.
	Active bool
}

// Model is the picker's state: the items, the filter, and what is selected. It
// is a bubbletea model, and is exported so a test can drive it with synthetic
// keystrokes and assert that the help text and the header the picker really
// draws say the same thing.
//
// The zero value is not usable — build one with [NewModel], which computes the
// initial filtered set. A Model is owned by bubbletea's single update
// goroutine; nothing here is safe for concurrent use.
type Model struct {
	single bool   // Enter chooses only the highlighted item, for scope navigation.
	items  []Item // every row, in the order given; selection lives on the item.
	// shown indexes items, in display order, after filtering. Filtering never
	// touches items, so a row selected under one filter is still selected
	// under the next.
	shown []int
	// cursor indexes shown, not items.
	cursor int
	filter string // what has been typed, lower-cased and matched against each item's haystack.
	// top is the first visible row of the window, an index into shown.
	top int
	// height is how many rows are drawn at once; a non-positive value is
	// clamped to a default in clampWindow.
	height int
	title  string // drawn above the list, with the count and the key hints.

	// confirmed and aborted are how the model tells Run why it quit, and also
	// make View draw nothing on the final frame.
	confirmed bool // enter was pressed.
	aborted   bool // esc on an empty filter, or ctrl+c.
}

// NewModel builds a picker over items, titled title, showing height rows at a
// time; a non-positive height falls back to ten. It takes ownership of items
// and mutates their selection state in place. Use it directly only in a test —
// production callers want [Run], which also drives the terminal.
func NewModel(title string, items []Item, height int) *Model {
	m := &Model{items: items, height: height, title: title}
	m.refilter()
	return m
}

// refilter recomputes the visible set for the current filter, keeping the
// cursor on the same item where that item still matches and putting it on the
// first row where it does not. Matching is a case-insensitive substring test
// against [Item.Haystack]; there is no fuzzy matching, because a substring is
// what an operator typing part of a role name expects.
func (m *Model) refilter() {
	current := -1
	if m.cursor >= 0 && m.cursor < len(m.shown) {
		current = m.shown[m.cursor]
	}
	needle := strings.ToLower(m.filter)
	m.shown = m.shown[:0]
	for i := range m.items {
		if needle == "" || strings.Contains(m.items[i].Haystack, needle) {
			m.shown = append(m.shown, i)
		}
	}
	m.cursor = 0
	for i, idx := range m.shown {
		if idx == current {
			m.cursor = i
			break
		}
	}
	m.clampWindow()
}

// clampWindow puts the cursor back inside the filtered set and scrolls the
// window the least amount that brings the cursor into view. Every navigation
// and filter change ends here, so no other code has to reason about the
// boundaries.
func (m *Model) clampWindow() {
	if m.height <= 0 {
		m.height = 10
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= len(m.shown) {
		m.cursor = max(0, len(m.shown)-1)
	}
	if m.cursor < m.top {
		m.top = m.cursor
	}
	if m.cursor >= m.top+m.height {
		m.top = m.cursor - m.height + 1
	}
	if m.top < 0 {
		m.top = 0
	}
}

// selectedCount counts selected items across the whole list, not just the
// visible ones, so the header keeps reporting selections a filter is hiding.
func (m *Model) selectedCount() int {
	n := 0
	for i := range m.items {
		if m.items[i].selected {
			n++
		}
	}
	return n
}

// toggleCursor flips the selection of the row under the cursor, and does
// nothing when the filter matches nothing.
func (m *Model) toggleCursor() {
	if m.cursor < 0 || m.cursor >= len(m.shown) {
		return
	}
	i := m.shown[m.cursor]
	m.items[i].selected = !m.items[i].selected
}

// toggleAllShown selects every currently matching row, or clears them if they
// are all already selected.
func (m *Model) toggleAllShown() {
	allSelected := len(m.shown) > 0
	for _, i := range m.shown {
		if !m.items[i].selected {
			allSelected = false
			break
		}
	}
	for _, i := range m.shown {
		m.items[i].selected = !allSelected
	}
}

// Init is bubbletea's start hook; the picker has nothing to do at start-up.
func (*Model) Init() tea.Cmd { return nil }

// Update applies one keypress and returns the model, plus tea.Quit on the keys
// that end the picker. Everything that is not a key — resizes, ticks — is
// ignored, since the view is redrawn from scratch each frame anyway. It
// satisfies tea.Model.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	if cmd, handled := m.handleExitKey(key); handled {
		return m, cmd
	}
	m.handleEditKey(key)
	return m, nil
}

// handleExitKey deals with the keys that can end the picker, and reports
// whether it consumed the key.
func (m *Model) handleExitKey(key tea.KeyMsg) (cmd tea.Cmd, handled bool) {
	switch key.String() {
	case "ctrl+c":
		m.aborted = true
		return tea.Quit, true
	case "esc":
		// Esc clears the filter first; only an already-empty filter quits, so a
		// mistyped search never throws away the whole selection.
		if m.filter != "" {
			m.filter = ""
			m.refilter()
			return nil, true
		}
		m.aborted = true
		return tea.Quit, true
	case "enter":
		if m.single {
			if len(m.shown) == 0 {
				return nil, true
			}
			for i := range m.items {
				m.items[i].selected = i == m.shown[m.cursor]
			}
		}
		m.confirmed = true
		return tea.Quit, true
	}
	return nil, false
}

// handleEditKey applies the selection, navigation and filter keys.
func (m *Model) handleEditKey(key tea.KeyMsg) {
	if m.single && (key.String() == "tab" || key.String() == "ctrl+a") {
		return
	}
	switch key.String() {
	case "tab":
		m.toggleCursor()
	case " ":
		// Space toggles only while the filter is empty; once you are typing it
		// is a character like any other.
		if m.canToggleOnSpace() {
			m.toggleCursor()
			return
		}
		m.filter += " "
		m.refilter()
	case "ctrl+a":
		m.toggleAllShown()
	case "up", "ctrl+p":
		m.cursor--
		m.clampWindow()
	case "down", "ctrl+n":
		m.cursor++
		m.clampWindow()
	case "backspace":
		if m.filter != "" {
			r := []rune(m.filter)
			m.filter = string(r[:len(r)-1])
			m.refilter()
		}
	default:
		if key.Type == tea.KeyRunes && len(key.Runes) > 0 {
			m.filter += string(key.Runes)
			m.refilter()
		}
	}
}

// View draws the title, the counts-and-keys header, the filter line and the
// visible window of rows, returning the empty string once the picker has
// ended. It satisfies tea.Model and is called by bubbletea after every
// [Model.Update].
func (m *Model) View() string {
	// Erase on exit: bubbletea clears the frame it last drew, so returning an
	// empty view at quit leaves the terminal exactly as the picker found it.
	// Scrollback then holds one "Selected N roles" line and the results, not a
	// dead snapshot of a list nobody can interact with any more.
	if m.confirmed || m.aborted {
		return ""
	}
	pal := term.PaletteFor(os.Stderr)
	var b strings.Builder

	fmt.Fprintf(&b, "%s\n", pal.Wrap(term.Bold, m.title))
	if m.single {
		fmt.Fprintf(&b, "%d of %d shown · type to filter, arrows move, enter choose\n", len(m.shown), len(m.items))
	} else {
		fmt.Fprintf(&b, "%d selected · %d of %d shown · type to filter, tab toggle, ctrl+a all, enter confirm\n",
			m.selectedCount(), len(m.shown), len(m.items))
	}

	filter := m.filter
	if filter == "" {
		filter = pal.Wrap(term.Dim, "(type to filter)")
	}
	fmt.Fprintf(&b, "> %s\n", filter)

	if len(m.shown) == 0 {
		fmt.Fprintf(&b, "  %s\n", pal.Wrap(term.Dim, "nothing matches — backspace to widen, esc to clear"))
		return b.String()
	}

	end := min(m.top+m.height, len(m.shown))
	for pos := m.top; pos < end; pos++ {
		i := m.shown[pos]
		cursor := "  "
		if pos == m.cursor {
			cursor = pal.Wrap(term.Bold, "> ")
		}
		mark := " "
		if m.items[i].selected {
			mark = pal.Wrap(term.Green, "✓")
		}
		label := m.items[i].Label
		if m.items[i].Active {
			label = pal.Wrap(term.Dim, label)
		}
		if m.single {
			fmt.Fprintf(&b, "%s%s\n", cursor, label)
		} else {
			fmt.Fprintf(&b, "%s[%s] %s\n", cursor, mark, label)
		}
	}
	if len(m.shown) > m.height {
		fmt.Fprintf(&b, "  %s\n", pal.Wrap(term.Dim,
			fmt.Sprintf("… %d more, ↑/↓ to scroll", len(m.shown)-m.height)))
	}
	return b.String()
}

// Run shows the picker and returns the indices into items that the user
// selected, in the original order. Rows selected while a filter was active
// count even if that filter no longer shows them.
//
// It blocks until the user confirms or leaves, and takes over the terminal's
// input while it does. It returns [ErrCancelled] when the user left without
// confirming, and bubbletea's own error if the program could not run at all —
// which is what happens when there is no terminal, so callers check that
// before reaching here. A confirmed but empty selection is no error: it returns
// a nil slice and the caller decides what nothing means.
func Run(title string, items []Item, height int) ([]int, error) {
	m := NewModel(title, items, height)
	return runModel(m)
}

// RunSingle draws the same inline picker but chooses the highlighted item on
// Enter. It blocks until selection or cancellation and returns Run's errors.
func RunSingle(title string, items []Item, height int) (int, error) {
	m := NewModel(title, items, height)
	m.single = true
	selected, err := runModel(m)
	if err != nil {
		return 0, err
	}
	if len(selected) != 1 {
		return 0, errors.New("no item selected")
	}
	return selected[0], nil
}

// runModel drives an inline picker and extracts its confirmed selection.
func runModel(m *Model) ([]int, error) {
	// Render to stderr so stdout stays a clean data channel, and inline rather
	// than on the alternate screen so the picker sits under the prompt like fzf
	// instead of taking over the terminal.
	p := tea.NewProgram(m, tea.WithOutput(os.Stderr))
	final, err := p.Run()
	if err != nil {
		return nil, err
	}
	fm, ok := final.(*Model)
	if !ok {
		return nil, errors.New("picker returned an unexpected model")
	}
	if fm.aborted || !fm.confirmed {
		return nil, ErrCancelled
	}
	var out []int
	for i := range fm.items {
		if fm.items[i].selected {
			out = append(out, i)
		}
	}
	return out, nil
}

// canToggleOnSpace preserves the multi-picker shortcut without selecting rows
// in the single-choice scope browser, where space is always filter text.
func (m *Model) canToggleOnSpace() bool { return m.filter == "" && !m.single }
