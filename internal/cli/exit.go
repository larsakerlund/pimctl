// The exit-code contract: the four codes, the error that carries one out to
// main, and the single place a run's results are mapped onto one. Which outcome
// a role ended in is decided in plan.go, and printed in report.go; this file
// only turns the set of them into a process status.

package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

const (
	// ExitOK means every selected role reached a good terminal state.
	ExitOK = 0
	// ExitFailed means at least one selected role failed. It is also the code
	// for every usage and setup error, which is why the documented wording is
	// wider than "a role failed".
	ExitFailed = 1
	// ExitPending means nothing failed but at least one request is waiting on
	// an approver, so the change has not taken effect. After `up` that means
	// the role is not active; after `down` it means the role is still active —
	// the same code, the opposite access, which is why both are spelled out
	// wherever it is documented.
	ExitPending = 2
	// ExitInterrupted is the conventional code for a run killed by SIGINT.
	ExitInterrupted = 130
)

// exitCodeError carries an explicit process exit code out to main, for the
// cases where the run did not fail in the ordinary sense: a selection queued
// for approval ([ExitPending]), or a picker the user walked away from
// ([ExitInterrupted]). Any other error means [ExitFailed], so this type is only
// ever constructed when the code is not 1.
type exitCodeError struct {
	code int    // one of the Exit* constants; never [ExitOK].
	msg  string // what main prints; already phrased for a person.
}

// Error returns the message, which is what main prints. The code travels
// separately, through [ExitCode].
func (e *exitCodeError) Error() string { return e.msg }

// ExitCode extracts the process exit code an error should produce, unwrapping
// to find an [exitCodeError]. Anything else is [ExitFailed]: usage errors,
// setup errors and ARM failures are all the same thing to a script.
func ExitCode(err error) int {
	var ec *exitCodeError
	if errors.As(err, &ec) {
		return ec.code
	}
	return ExitFailed
}

// partialFailureError is the non-zero exit produced when some contexts failed
// but the run still printed useful output for the rest.
func partialFailureError(failures []error) error {
	return &exitCodeError{
		code: ExitFailed,
		msg:  fmt.Sprintf("%d query failure(s); the output above is incomplete", len(failures)),
	}
}

// reportRun prints the per-role results and maps the run onto pimctl's exit
// code contract. Nothing here may return early: the requests have been sent, so
// the user must always see what happened to every role.
func reportRun(
	cmd *cobra.Command,
	g *globalOpts,
	results []result,
	failures []error,
	multi, streamed bool,
	scopes scopeLabeler,
) error {
	if err := printResults(cmd, g, results, multi, streamed, scopes); err != nil {
		return err
	}
	if len(failures) > 0 {
		// A context we could not reach may well have held more of the roles the
		// user asked for, so the run is incomplete regardless of the results.
		return partialFailureError(failures)
	}
	if code := exitCodeFor(results); code != ExitOK {
		return &exitCodeError{code: code, msg: summariseFailures(results)}
	}
	return nil
}
