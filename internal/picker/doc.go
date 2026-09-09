// Package picker is the type-to-filter multi-select pimctl shows when a command
// that acts on roles was given no selection flags.
//
// The interaction is fzf's, not a form's: the list is on screen from the start,
// a printable character goes straight into the filter, tab toggles the row
// under the cursor, ctrl+a toggles every row the filter currently matches, and
// enter confirms. Esc clears the filter and only quits once it is already
// empty, so a mistyped search cannot throw away a selection. Rows selected
// while one filter was active stay selected under the next one.
//
// The exported surface is five names. [Item] is one row: the label to draw and
// the pre-lowercased haystack the filter matches against. [Run] is what
// commands call — it draws the picker on stderr, inline rather than on the
// alternate screen, and returns the indices selected in the original order, or
// [ErrCancelled] if the user left without confirming. [NewModel] returns the
// [Model] behind [Run] without running it, which is how the tests drive
// keystrokes and read frames without a terminal.
//
// Nothing here knows what a role or a scope is. The package takes strings and
// returns indices into the slice it was given, so it has no dependency on
// internal/cli's types and cannot drift as they change; building the labels and
// the haystacks, and mapping the indices back to eligibilities, is the caller's
// job.
//
// It is a small hand-written bubbletea model rather than a huh MultiSelect or a
// bubbles/list. Both of those put filtering behind a mode — huh reserves "/" to
// enter it, and inside it space types a space and enter applies the filter
// instead of confirming — which is three surprises in a row and the reason the
// picker read as broken. The interaction is small enough that owning it
// outright is less code than bending either library.
//
// The Go tests here cover the model: they feed it tea.KeyMsg values and assert
// on the string [Model.View] returns. What they cannot cover is the terminal
// itself, because bubbletea and huh both need a real one — so the picker is
// also driven end to end through a pty by the expect(1) scripts under
// scripts/, behind the `tuiprobe` build tag, which is where the huh
// misbehaviour above was caught in the first place.
package picker
