// Covers exit.go: the mapping from a run's results onto the four exit codes,
// and the extraction of a code back out of an error. The contract is documented,
// so scripts depend on it.

package cli

import (
	"testing"
)

func TestExitCodeFor(t *testing.T) {
	ok := []result{{Outcome: OutcomeActivated}, {Outcome: OutcomeAlreadyActive}, {Outcome: OutcomeDeactivated}}
	if got := exitCodeFor(ok); got != ExitOK {
		t.Errorf("all-good exit code = %d, want 0", got)
	}
	pending := append([]result{{Outcome: OutcomePending}}, ok...)
	if got := exitCodeFor(pending); got != ExitPending {
		t.Errorf("pending exit code = %d, want 2", got)
	}
	failed := append([]result{{Outcome: OutcomeFailed}}, pending...)
	if got := exitCodeFor(failed); got != ExitFailed {
		t.Errorf("failed exit code = %d, want 1 (failure outranks pending)", got)
	}
	// A poll timeout is a failure, not "pending approval": the access is not
	// held and no approver is going to resolve it.
	if got := exitCodeFor([]result{{Outcome: OutcomeWaiting}}); got != ExitFailed {
		t.Errorf("timed-out exit code = %d, want 1 (exit 2 is only for pending approval)", got)
	}
	if got := exitCodeFor([]result{{Outcome: OutcomeWaiting}, {Outcome: OutcomePending}}); got != ExitFailed {
		t.Errorf("a timeout alongside a pending approval must exit 1, got %d", got)
	}
	if got := exitCodeFor([]result{{Outcome: OutcomePending}, {Outcome: OutcomeActivated}}); got != ExitPending {
		t.Errorf("pending approval alone must exit 2, got %d", got)
	}
}

func TestExitCodeExtraction(t *testing.T) {
	if got := ExitCode(&exitCodeError{code: ExitPending, msg: "pending"}); got != ExitPending {
		t.Errorf("ExitCode = %d", got)
	}
	if got := ExitCode(plainError{}); got != ExitFailed {
		t.Errorf("a plain error should exit 1, got %d", got)
	}
}

type plainError struct{}

func (plainError) Error() string { return "boom" }
