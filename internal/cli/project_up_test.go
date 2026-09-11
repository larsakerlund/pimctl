// Project activation tests cover confirmation of actual submissions. Existing
// windows are preserved without treating a long requirements file as new work.

package cli

import "testing"

func TestPreservedProjectDoesNotRequireBulkConfirmation(t *testing.T) {
	plan := make([]*planItem, confirmBulkThreshold+1)
	for i := range plan {
		plan[i] = &planItem{KeepActive: true}
	}
	opts := confirmOpts{Roles: activationSubmissionCount(plan)}
	if opts.NeedsConfirmation() {
		t.Fatal("a fully held project prompted for a no-op")
	}
	plan[0].KeepActive = false
	opts.Roles = activationSubmissionCount(plan)
	if opts.Roles != 1 || opts.NeedsConfirmation() {
		t.Fatal("preserved roles counted toward new submissions")
	}
	for _, item := range plan {
		item.KeepActive = false
	}
	opts.Roles = activationSubmissionCount(plan)
	if !opts.NeedsConfirmation() {
		t.Fatal("a real bulk activation skipped confirmation")
	}
}
