// Covers main.go: run's mapping from a context, an argument list and the
// command's result onto the process exit code and the `pimctl: …` stderr line.
// It drives run with a context it cancels by hand, so no signal is sent and no
// command that touches a tenant is executed; main and runWithInterrupts are the
// process boundary and stay untested here.

package main

import (
	"context"
	"strings"
	"testing"

	"github.com/larsakerlund/pimctl/internal/cli"
)

func TestRunReturns130WhenTheContextIsCancelled(t *testing.T) {
	// cobra does not consult the context before running a command, so
	// `version` runs to completion and returns nil even though ctx is already
	// cancelled. That is exactly the property under test: the interrupt
	// outranks whatever the command returned, and the stderr line says so.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var stdout, stderr strings.Builder
	code := run(ctx, []string{"version"}, &stdout, &stderr)

	if code != 130 {
		t.Errorf("exit code = %d, want 130: the documented contract", code)
	}
	if code != cli.ExitInterrupted {
		t.Errorf("exit code = %d, want cli.ExitInterrupted (%d)", code, cli.ExitInterrupted)
	}
	if want := interruptedMessage + "\n"; stderr.String() != want {
		t.Errorf("stderr = %q, want %q", stderr.String(), want)
	}
}

func TestRunReturnsUsageErrorAs1(t *testing.T) {
	var stdout, stderr strings.Builder
	code := run(context.Background(), []string{"--no-such-flag"}, &stdout, &stderr)

	if code != cli.ExitFailed {
		t.Errorf("exit code = %d, want %d", code, cli.ExitFailed)
	}
	if want := "pimctl: unknown flag: --no-such-flag\n"; stderr.String() != want {
		t.Errorf("stderr = %q, want %q", stderr.String(), want)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing: a usage error is reported on stderr only", stdout.String())
	}
}

func TestRunVersionIsZero(t *testing.T) {
	var stdout, stderr strings.Builder
	code := run(context.Background(), []string{"version"}, &stdout, &stderr)

	if code != cli.ExitOK {
		t.Errorf("exit code = %d, want %d", code, cli.ExitOK)
	}
	// An unstamped test binary reports `dev`, so only the line's shape is
	// pinned: `pimctl <version> (<commit>, <go>, <os/arch>)`.
	if out := stdout.String(); !strings.HasPrefix(out, "pimctl ") || !strings.HasSuffix(out, ")\n") {
		t.Errorf("stdout = %q, want one `pimctl <version> (…)` line", out)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want nothing on a clean run", stderr.String())
	}
}
