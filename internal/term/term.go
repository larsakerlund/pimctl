// Am I talking to a person, and may I use colour? The three TTY predicates and
// the palette live here; the spinner that uses them is in spinner.go, and the
// package comment is in doc.go.

package term

import (
	"io"
	"os"

	"github.com/mattn/go-isatty"
)

// StdinIsTTY reports whether an interactive prompt can be shown.
func StdinIsTTY() bool {
	return isatty.IsTerminal(os.Stdin.Fd()) || isatty.IsCygwinTerminal(os.Stdin.Fd())
}

// StdoutIsTTY reports whether the command's own output is a terminal, which is
// what decides if a later correction can still reach the reader.
func StdoutIsTTY() bool {
	return isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd())
}

// StderrIsTTY reports whether progress output has somewhere to go.
func StderrIsTTY() bool {
	return isatty.IsTerminal(os.Stderr.Fd()) || isatty.IsCygwinTerminal(os.Stderr.Fd())
}

// Palette decides whether a string may be coloured. Colour is off unless the
// destination is a terminal and NO_COLOR is unset, per https://no-color.org.
//
// A Palette is immutable and safe to copy or share between goroutines. The zero
// value is the colourless palette, which is the right default: a value that was
// never built by [PaletteFor] must not assume a terminal.
type Palette struct {
	enabled bool // false unless the destination is a terminal and NO_COLOR is unset.
}

// PaletteFor returns the palette appropriate to one destination.
//
// It is asked per writer rather than per process because the answer differs
// between streams: `pimctl ls | less` has a pipe on stdout and a terminal on
// stderr, and the spinner and warnings there may still be coloured. Anything
// that is not an *os.File — a bytes.Buffer in a test, say — gets the
// colourless palette.
func PaletteFor(w io.Writer) Palette {
	if os.Getenv("NO_COLOR") != "" {
		return Palette{}
	}
	f, ok := w.(*os.File)
	if !ok {
		return Palette{}
	}
	return Palette{enabled: isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())}
}

// The SGR sequences pimctl uses, kept to the eight basic colours and two
// attributes so they render the same on every terminal worth supporting. Each
// carries meaning that survives losing it: the colour reinforces a glyph or a
// word, never replaces one, so a NO_COLOR run or a log file loses decoration
// and not information.
const (
	Reset  = "\x1b[0m"  // ends every sequence below; nothing is ever left set.
	Green  = "\x1b[32m" // a good outcome: activated, deactivated, already active.
	Yellow = "\x1b[33m" // something unfinished: pending approval, still pending.
	Red    = "\x1b[31m" // a failure.
	Dim    = "\x1b[2m"  // secondary text, such as a picker row already active.
	Bold   = "\x1b[1m"  // a heading.
)

// Wrap returns s wrapped in the given SGR sequence and [Reset], or s unchanged
// when colour is off for this palette or there is nothing to colour. code is
// one of the sequences declared above; nothing validates it, because the only
// callers are inside pimctl.
func (p Palette) Wrap(code, s string) string {
	if !p.enabled || s == "" {
		return s
	}
	return code + s + Reset
}
