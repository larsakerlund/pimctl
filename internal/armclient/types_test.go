// Covers the status predicates in types.go: which statuses end a poll, which
// count as a failure for which request type, and which are worded as a
// revocation rather than a refusal. The wire types themselves are exercised
// through client_test.go, and the ISO-8601 duration helpers have their own
// tests there too.

package armclient

import "testing"

// TestExpiredAndRevokedAndCanceledEndThePoll pins the statuses that used to
// run the full poll budget: ARM has closed the request, so nothing further can
// happen to it and waiting only delays a settled answer.
func TestExpiredAndRevokedAndCanceledEndThePoll(t *testing.T) {
	for _, s := range []string{StatusExpired, StatusRevokedAndCanceled} {
		if !IsTerminalStatus(s) {
			t.Errorf("%s must be terminal — polling has to stop", s)
		}
		if !IsFailureStatus(s) {
			t.Errorf("%s must count as a failure", s)
		}
		if IsSuccessStatus(s) || IsPendingApprovalStatus(s) {
			t.Errorf("%s must be neither success nor pending", s)
		}
		if !IsRevocationStatus(s) {
			t.Errorf("%s should be worded as a revocation, not a refusal", s)
		}
	}
}

// TestFailureStatusForIsRequestTypeAware: Revoked is the success of a
// deactivation and the failure of an activation, and only the type-aware
// predicate may say so.
func TestFailureStatusForIsRequestTypeAware(t *testing.T) {
	if !IsFailureStatusFor(RequestTypeSelfActivate, StatusRevoked) {
		t.Error("an activation that ended Revoked has granted nothing and must read as a failure")
	}
	if IsFailureStatusFor(RequestTypeSelfDeactivate, StatusRevoked) {
		t.Error("Revoked is the successful end of a deactivation, not a failure")
	}
	if !IsTerminalStatus(StatusRevoked) {
		t.Error("Revoked must end the poll for both request types")
	}
	for _, s := range []string{"Failed", "Denied", "AdminDenied", StatusCanceled, StatusExpired} {
		for _, rt := range []string{RequestTypeSelfActivate, RequestTypeSelfDeactivate} {
			if !IsFailureStatusFor(rt, s) {
				t.Errorf("%s should be a failure for %s", s, rt)
			}
		}
	}
	for _, s := range []string{StatusProvisioned, StatusGranted, "PendingApproval", "Accepted"} {
		if IsFailureStatusFor(RequestTypeSelfActivate, s) {
			t.Errorf("%s is not a failure", s)
		}
	}
	if !IsRevocationStatus(StatusRevoked) || IsRevocationStatus("Failed") || IsRevocationStatus("Denied") {
		t.Error("only revoked, revoked-and-cancelled and expired requests are revocations")
	}
}
