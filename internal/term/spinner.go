// The one-line progress indicator pimctl shows while the network is slow: its
// state, its goroutine and the escape sequences that erase it. Whether the
// stream is a terminal at all, and the colour it may carry, are decided in
// term.go — the spinner emits no colour of its own.

package term

import (
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mattn/go-isatty"
)

// Spinner writes a single status line to stderr while slow work runs. It is a
// no-op when stderr is not a terminal, so piped and CI output stays clean.
//
// A Spinner is created started by [NewSpinner] and must be stopped exactly once
// with [Spinner.Stop]; the zero value is not usable, because its channels are
// nil. [Spinner.Update] and [Spinner.Stop] may be called from any goroutine
// while the animation runs in its own.
type Spinner struct {
	w       io.Writer // where frames go, normally stderr.
	enabled bool      // false when w is not a terminal; every method is then a no-op.
	// paused is atomic because Pause is called from the goroutine printing
	// results while run() is reading it a frame at a time.
	paused atomic.Bool
	stop   chan struct{} // closed by Stop to ask run() to finish.
	done   chan struct{} // closed by run() once the line is cleared.
	mu     sync.Mutex    // guards msg, which Update rewrites mid-animation.
	msg    string        // the text after the frame, e.g. "reading active roles…".
}

// frames is the braille cycle the animation walks. Braille rather than ASCII
// because the dots turn in place: every frame is one cell wide, so the line
// never reflows, and the sequence reads as motion rather than as flicker.
var frames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// SpinnerEnabled decides whether a Spinner animates, given whether its
// destination is a terminal.
//
// It takes the answer rather than working it out so the rule can be tested off
// a terminal, and the rule is the whole point: being on a terminal is the only
// condition. NO_COLOR is not one — it governs colour, and a spinner is motion,
// not colour.
func SpinnerEnabled(isTTY bool) bool { return isTTY }

// NewSpinner returns a started Spinner writing msg to w, animating only when w
// is a terminal. It starts a goroutine, so call [Spinner.Stop] exactly once —
// deferring it at the call site is the usual shape.
func NewSpinner(w io.Writer, msg string) *Spinner {
	s := &Spinner{w: w, msg: msg, stop: make(chan struct{}), done: make(chan struct{})}
	// NO_COLOR deliberately plays no part here. The Spinner emits no colour — a
	// carriage return, an erase, a braille frame — and NO_COLOR is about
	// colour, not motion. Colour is handled where it belongs, in PaletteFor.
	f, ok := w.(*os.File)
	s.enabled = SpinnerEnabled(ok && (isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())))
	if !s.enabled {
		close(s.done)
		return s
	}
	go s.run()
	return s
}

// frameInterval is fast enough to look alive and slow enough not to
// flicker over ssh.
const frameInterval = 90 * time.Millisecond

// run animates the line until Stop closes s.stop, then erases it. It is the
// Spinner's own goroutine and is started only when the destination is a
// terminal.
func (s *Spinner) run() {
	defer close(s.done)
	t := time.NewTicker(frameInterval)
	defer t.Stop()
	i := 0
	for {
		select {
		case <-s.stop:
			s.clear()
			return
		case <-t.C:
			if s.paused.Load() {
				continue
			}
			s.mu.Lock()
			msg := s.msg
			s.mu.Unlock()
			fmt.Fprintf(s.w, "\r\x1b[2K%s %s", frames[i%len(frames)], msg)
			i++
		}
	}
}

// clear returns the cursor to column one and erases the whole line, so what is
// printed next starts on clean ground rather than on top of a half-drawn frame.
func (s *Spinner) clear() { fmt.Fprint(s.w, "\r\x1b[2K") }

// Pause stops the animation, runs fn synchronously with the line cleared, and
// resumes. When the Spinner is disabled it simply runs fn. A
// result line printed while the Spinner is mid-frame would otherwise land on
// top of it.
func (s *Spinner) Pause(fn func()) {
	if !s.enabled {
		fn()
		return
	}
	s.paused.Store(true)
	s.clear()
	fn()
	s.paused.Store(false)
}

// Update changes the message shown from the next frame onwards. It is safe to
// call from another goroutine while the Spinner is animating, and does nothing
// visible when it is not.
func (s *Spinner) Update(msg string) {
	s.mu.Lock()
	s.msg = msg
	s.mu.Unlock()
}

// Stop halts the Spinner and clears its line, blocking until the goroutine has
// erased it so the caller's next write cannot race the last frame. Calling it
// more than once is harmless; not calling it leaves the goroutine running and
// the frame on screen.
func (s *Spinner) Stop() {
	if !s.enabled {
		return
	}
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	<-s.done
}
