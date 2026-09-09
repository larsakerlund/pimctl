// Command pimctl batch-activates the Azure PIM resource roles you are eligible
// for, using cloudctx for per-tenant credential isolation.
//
// Everything the command does lives in internal/cli; this file is only the
// process boundary, and it owns two things.
//
// The first is the interrupt. SIGINT and SIGTERM cancel the context every
// command runs under, so an activation in progress stops asking ARM for more
// rather than being killed mid-table. Because os.Exit skips deferred calls,
// that handler cannot be installed in main: the body is in run(), where the
// handler is torn down by its own defer and the exit code is returned rather
// than taken on the spot.
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
	"os"
	"os/signal"
	"syscall"

	"github.com/larsakerlund/pimctl/internal/cli"
)

// version can be stamped with `-ldflags "-X main.version=..."`. The Makefile
// stamps internal/cli.Version instead; either works, and an unstamped build
// falls back to the VCS revision recorded by the Go toolchain.
var version string

// main stamps the version, runs the command through run and exits with the code
// it returns. It never returns normally, which is why it holds no defer of its
// own.
func main() {
	cli.SetVersion(version)

	// os.Exit skips deferred calls, so the body lives in run() and the signal
	// handler is always torn down before the process leaves.
	os.Exit(run())
}

// run executes the root command under a context cancelled by SIGINT or
// SIGTERM, prints any error to stderr as `pimctl: …`, and returns the process
// exit code.
//
// It exists so the signal handler it installs is torn down before os.Exit,
// which runs no deferred calls. An interrupt wins over whatever error the command
// returned: a cancelled command's error describes the cancellation, not a
// failure the user needs to act on.
func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := cli.NewRootCmd().ExecuteContext(ctx)

	// An interrupt gets the conventional 130 and says so plainly. Any per-role
	// table has already been printed, with the interrupted roles marked ABORTED
	// or SKIPPED rather than given a status pimctl never actually observed.
	if ctx.Err() != nil {
		fmt.Fprintln(
			os.Stderr,
			"pimctl: interrupted — any request already sent may still be in flight; check `pimctl status`",
		)
		return cli.ExitInterrupted
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "pimctl: %v\n", err)
		return cli.ExitCode(err)
	}
	return cli.ExitOK
}
