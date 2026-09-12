// The collaborators a command tree is built with, and their production values:
// the session opener, the time budgets and the terminal probes. What each of
// them does lives with its subject — sessions in session.go, budgets in
// active.go, the isatty calls in internal/term; this file only gathers them so
// a test can substitute any one of them for a single tree.

package cli

import "github.com/larsakerlund/pimctl/internal/term"

// deps are the collaborators a command tree uses, injected at construction.
//
// It is a struct rather than a set of function parameters because every seam
// joins here instead of becoming a package variable: a package variable is
// shared by every tree a test builds, and a test that set one and forgot to
// put it back left it set for the next.
type deps struct {
	openSessions sessionOpener // defaults to [openSessionsWith]; replaced in tests.
	timeouts     timeouts      // the run's time budgets; shortened in tests.
	tty          ttyProbe      // defaults to the process's own streams; pinned in tests.
}

// defaultDeps is what [NewRootCmd] uses: the real ARM-backed opener, the
// production time budgets and the process's own streams.
func defaultDeps() deps {
	return deps{openSessions: openSessionsWith, timeouts: defaultTimeouts(), tty: defaultTTY()}
}

// ttyProbe answers whether each of the three standard streams is a terminal.
//
// The three are asked separately because they differ in practice — `pimctl ls
// | less` has a pipe on stdout and a terminal on stdin and stderr — and each
// decides something different: stdin whether a prompt can be shown, stdout
// whether a later correction can still reach the reader, stderr whether
// progress has anywhere to go. A nil field answers no, so the zero value is
// the colourless, promptless, non-streaming answer: a probe nobody configured
// must not assume a terminal.
type ttyProbe struct {
	stdin  func() bool // whether a prompt can be shown.
	stdout func() bool // whether the command's own output is a terminal.
	stderr func() bool // whether progress output has somewhere to go.
}

// defaultTTY probes the process's own file descriptors, through internal/term.
func defaultTTY() ttyProbe {
	return ttyProbe{stdin: term.StdinIsTTY, stdout: term.StdoutIsTTY, stderr: term.StderrIsTTY}
}

// stdinIsTTY reports whether an interactive prompt can be shown; no when the
// probe was never configured.
func (p ttyProbe) stdinIsTTY() bool { return p.stdin != nil && p.stdin() }

// stdoutIsTTY reports whether the command's own output is a terminal, which is
// what decides if a later correction can still reach the reader; no when the
// probe was never configured.
func (p ttyProbe) stdoutIsTTY() bool { return p.stdout != nil && p.stdout() }

// stderrIsTTY reports whether progress output has somewhere to go; no when the
// probe was never configured.
func (p ttyProbe) stderrIsTTY() bool { return p.stderr != nil && p.stderr() }
