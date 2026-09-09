// The --debug timing breakdown: what each phase cost, and the notes that go
// with it. It measures only what it is asked to wrap, and it never records a
// token or anything else that would be unsafe to print.

package cli

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// timings records how long each phase took, for --debug. This is the
// instrumentation behind the "why does list take 14 seconds" question: it
// separates the cost of minting a token through cloudctx/az from the ARM calls.
type timings struct {
	mu sync.Mutex // guards every field; spans are recorded from the fan-out's goroutines.
	// order is the order spans were first seen, so the report reads as the run
	// happened rather than alphabetically.
	order  []string
	spans  map[string]time.Duration // name to total time, summed over repeats.
	notes  []string                 // one-line facts with no duration, e.g. "token contoso: cache hit".
	enable bool                     // false makes every method a no-op, which is the common case.
}

// newTimings returns a recorder that is inert unless enabled. Every method
// tolerates a nil receiver and a disabled one, so callers can wrap work in
// Track without asking whether --debug was passed.
func newTimings(enabled bool) *timings {
	return &timings{spans: map[string]time.Duration{}, enable: enabled}
}

// Track times fn and records it under name.
func (t *timings) Track(name string, fn func() error) error {
	if t == nil || !t.enable {
		return fn()
	}
	start := time.Now()
	err := fn()
	t.record(name, time.Since(start))
	return err
}

// TrackVoid times fn and records it under name, for work that reports its
// failures some other way than by returning an error.
func (t *timings) TrackVoid(name string, fn func()) {
	if t == nil || !t.enable {
		fn()
		return
	}
	start := time.Now()
	fn()
	t.record(name, time.Since(start))
}

// record adds d to the span called name, remembering first-seen order so the
// report reads in the order the work happened. Repeated names accumulate: the
// per-scope fan-out reports as one line, not twenty-five. Safe for concurrent
// use, which the fan-out needs.
func (t *timings) record(name string, d time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, seen := t.spans[name]; !seen {
		t.order = append(t.order, name)
	}
	t.spans[name] += d
}

// note records a one-line fact for the --debug report, with no duration.
func (t *timings) note(msg string) {
	if t == nil || !t.enable {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.notes = append(t.notes, msg)
}

// Report writes the breakdown to w, adding an "(other)" line for whatever total
// the tracked spans do not account for. total is the whole command's wall time,
// which is why the spans can sum to more than it: they run concurrently.
// Nothing is written when --debug is off.
func (t *timings) Report(w io.Writer, total time.Duration) {
	if t == nil || !t.enable {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	fmt.Fprintln(w, "\ntiming breakdown:")
	for _, n := range t.notes {
		fmt.Fprintf(w, "  %s\n", n)
	}
	var accounted time.Duration
	for _, name := range t.order {
		d := t.spans[name]
		accounted += d
		fmt.Fprintf(w, "  %-42s %8.2fs\n", name, d.Seconds())
	}
	fmt.Fprintf(w, "  %-42s %8.2fs\n", "(other)", (total - accounted).Seconds())
	const ruleWidth = 10
	fmt.Fprintf(w, "  %-42s %8.2fs\n", strings.Repeat("─", ruleWidth)+" total", total.Seconds())
}
