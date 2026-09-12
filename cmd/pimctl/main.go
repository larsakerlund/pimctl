// Command pimctl batch-activates the Azure PIM resource roles you are eligible
// for, using cloudctx for per-tenant credential isolation.
//
// Everything the command does lives in internal/cli; this file is only the
// process boundary, and it owns two things.
//
// The first is the interrupt. SIGINT and SIGTERM cancel the context every
// command runs under, so an activation in progress stops asking ARM for more
// rather than being killed mid-table. Because os.Exit skips deferred calls,
// that handler cannot be installed in main: it is installed in
// runWithInterrupts(), where it is torn down by its own defer, and the exit
// code is returned to main rather than taken on the spot. The body that turns
// a context, arguments and output streams into an exit code is run(), which
// takes all of them as parameters so a test can hand it an already-cancelled
// context and see the interrupt path without sending a signal.
//
// The second is the exit code, which is a contract scripts depend on:
//
//	0    every selected role reached a good terminal state, or was already
//	     there;
//	1    at least one role failed or never reached a final status in time —
//	     and every usage and setup error;
//	2    nothing failed, but at least one role is waiting on an approver, so
//	     the change has not taken effect: after `up` the role is not active,
//	     after `down` it is still active;
//	130  interrupted, with requests already sent possibly still in flight.
//
// 2 exists so a script cannot read "queued for approval" as success. A request
// that stalls below a terminal status is a plain 1, because no approver will
// resolve it.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/larsakerlund/pimctl/internal/cli"
)

// version can be stamped with `-ldflags "-X main.version=..."`. The Makefile
// and goreleaser stamp internal/cli.Version instead; either works, and an
// unstamped build falls back to the module version and VCS revision the Go
// toolchain records. Whichever is set, `pimctl version` and `pimctl --version`
// print it as `pimctl v<version> (<commit>, <go>, <os/arch>)`.
var version string

// interruptedMessage is the one line run prints to stderr when the context was
// cancelled by a signal. It names the consequence — a request ARM has already
// accepted is not withdrawn by killing the client — and the command that shows
// what actually happened.
const interruptedMessage = "pimctl: interrupted — any request already sent may still be in flight; check `pimctl status`"

// main stamps the version, runs the command through runWithInterrupts and
// exits with the code it returns. It never returns normally, which is why it
// holds no defer of its own.
func main() {
	cli.SetVersion(version)

	// os.Exit skips deferred calls, so the body lives in runWithInterrupts()
	// and the signal handler is always torn down before the process leaves.
	os.Exit(runWithInterrupts(os.Args[1:], os.Stdout, os.Stderr))
}

// runWithInterrupts installs the SIGINT/SIGTERM handler, runs the command
// under the context it cancels, and tears the handler down before returning
// the exit code. It is the only place the process listens for signals; run
// itself never does, which is what lets a test drive run with a context it
// cancelled by hand.
func runWithInterrupts(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return run(ctx, args, stdout, stderr)
}

// run executes the root command with args (the command line without the
// program name) under ctx, sends the command's output to stdout and stderr,
// prints any error to stderr as `pimctl: …`, and returns the process exit
// code.
//
// A cancelled ctx wins over whatever the command returned: a cancelled
// command's error describes the cancellation, not a failure the user needs to
// act on, so the result is [cli.ExitInterrupted] and [interruptedMessage]
// regardless of that error. Otherwise the code is [cli.ExitCode] of the error,
// or [cli.ExitOK] when there was none.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	root := cli.NewRootCmd()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)

	err := root.ExecuteContext(ctx)

	// An interrupt gets the conventional 130 and says so plainly. Any per-role
	// table has already been printed, with the interrupted roles marked ABORTED
	// or SKIPPED rather than given a status pimctl never actually observed.
	if ctx.Err() != nil {
		fmt.Fprintln(stderr, interruptedMessage)
		return cli.ExitInterrupted
	}
	if err != nil {
		fmt.Fprintf(stderr, "pimctl: %v\n", err)
		return cli.ExitCode(err)
	}
	return cli.ExitOK
}
