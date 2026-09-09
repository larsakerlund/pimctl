// Tests for the terminal layer: the colour rule (NO_COLOR governs colour and
// nothing else), the spinner's animation, and that each TTY predicate reads the
// stream it names. A Go test has no terminal, so the spinner is driven through
// its own fields rather than through NewSpinner's detection — which is why
// SpinnerEnabled takes the answer as an argument.

package term

import (
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mattn/go-isatty"
)

// TestNoColorKeepsTheSpinner: NO_COLOR is about colour. The spinner emits none,
// and honouring it there turned every slow command into a blank terminal.
func TestNoColorKeepsTheSpinner(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if got := PaletteFor(os.Stdout).Wrap(Green, "x"); got != "x" {
		t.Errorf("NO_COLOR must still disable colour, got %q", got)
	}
	// A test has no terminal, so assert on the rule rather than the output: on
	// a terminal the spinner runs, NO_COLOR or not.
	if !SpinnerEnabled(true) {
		t.Error("NO_COLOR must not silence the spinner")
	}
	if SpinnerEnabled(false) {
		t.Error("the spinner must stay off when the destination is not a terminal")
	}
}

// syncWriter is a writer safe to read from the test goroutine while the
// spinner's own goroutine writes to it.
type syncWriter struct {
	mu sync.Mutex
	b  strings.Builder
}

// Write appends to the buffer under the lock.
func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

// String returns what has been written so far.
func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// startSpinner builds an animating spinner over w without needing a terminal.
// NewSpinner decides `enabled` from the destination, and a test has no pty; the
// behaviour under test is what the goroutine does once enabled.
func startSpinner(w io.Writer, msg string) *Spinner {
	s := &Spinner{
		w: w, msg: msg, enabled: true,
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	go s.run()
	return s
}

// waitFor polls until cond holds, failing the test rather than hanging if it
// never does. The spinner is timer-driven, so the alternative is a sleep long
// enough to be slow and short enough to be flaky.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(frameInterval / 2)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSpinnerAnimatesAndErasesItself(t *testing.T) {
	var w syncWriter
	s := startSpinner(&w, "reading active roles…")
	waitFor(t, "the first frame", func() bool { return strings.Contains(w.String(), "reading active roles…") })

	// Every frame rewrites one line: carriage return, erase, frame, message.
	// Anything else would scroll the terminal a line per 90 ms.
	if !strings.Contains(w.String(), "\r\x1b[2K") {
		t.Errorf("a frame does not rewrite its line: %q", w.String())
	}
	if strings.Contains(w.String(), "\n") {
		t.Errorf("the spinner printed a newline: %q", w.String())
	}

	s.Update("2 roles to go…")
	waitFor(t, "the updated message", func() bool { return strings.Contains(w.String(), "2 roles to go…") })

	s.Stop()
	if !strings.HasSuffix(w.String(), "\r\x1b[2K") {
		t.Errorf("Stop must leave the line erased, got %q", lastBytes(w.String()))
	}
	s.Stop() // twice is harmless.
}

func TestSpinnerPauseClearsTheLineAroundOutput(t *testing.T) {
	var w syncWriter
	s := startSpinner(&w, "activating 2 roles…")
	waitFor(t, "the first frame", func() bool { return strings.Contains(w.String(), "activating") })

	var duringPause string
	s.Pause(func() {
		duringPause = w.String()
		if _, err := w.Write([]byte("✓ ACTIVATED  Owner\n")); err != nil {
			t.Error(err)
		}
	})
	if !strings.HasSuffix(duringPause, "\r\x1b[2K") {
		t.Errorf("Pause must clear the line before running fn, got %q", lastBytes(duringPause))
	}
	s.Stop()

	// The result line survives intact: that is the whole point of pausing.
	if !strings.Contains(w.String(), "✓ ACTIVATED  Owner\n") {
		t.Errorf("the streamed line was overwritten: %q", w.String())
	}
}

func TestSpinnerIsInertWhenNotATerminal(t *testing.T) {
	var w syncWriter
	s := NewSpinner(&w, "should not draw")
	ran := false
	s.Pause(func() { ran = true })
	s.Update("still nothing")
	s.Stop()
	if !ran {
		t.Error("Pause must run fn even when the spinner is disabled")
	}
	if w.String() != "" {
		t.Errorf("a disabled spinner wrote %q", w.String())
	}

	// A real file that is not a terminal — a redirected stderr — is the same.
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // a temp file in a directory the test owns
	NewSpinner(f, "redirected").Stop()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Errorf("a redirected spinner wrote %d bytes", info.Size())
	}
}

// TestTTYHelpersReadTheStreamTheyName: each helper must ask about its own
// descriptor. A copy-paste that made StderrIsTTY read stdout would send the
// spinner to a pipe and the table to a terminal.
func TestTTYHelpersReadTheStreamTheyName(t *testing.T) {
	cases := []struct {
		name string
		got  bool
		want bool
	}{
		{"stdin", StdinIsTTY(), isatty.IsTerminal(os.Stdin.Fd()) || isatty.IsCygwinTerminal(os.Stdin.Fd())},
		{"stdout", StdoutIsTTY(), isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd())},
		{"stderr", StderrIsTTY(), isatty.IsTerminal(os.Stderr.Fd()) || isatty.IsCygwinTerminal(os.Stderr.Fd())},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%sIsTTY = %v, want %v — it is reading another stream", tc.name, tc.got, tc.want)
		}
	}
}

// lastBytes renders the tail of a spinner buffer for a failure message; the
// whole thing is a wall of escape sequences.
func lastBytes(s string) string {
	const tail = 24
	if len(s) <= tail {
		return s
	}
	return "…" + s[len(s)-tail:]
}
