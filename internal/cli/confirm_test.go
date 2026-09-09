// Covers confirm.go: the confirmation policy — never in the interactive flow,
// and unattended only for --all or a selection above the bulk threshold.

package cli

import (
	"testing"
)

// TestConfirmationOnlyForBroadUnattendedRuns pins the removal of the y/N from
// the interactive flow.
func TestConfirmationOnlyForBroadUnattendedRuns(t *testing.T) {
	cases := []struct {
		name string
		opts confirmOpts
		want bool
	}{
		{"interactive picker", confirmOpts{Interactive: true, Roles: 40}, false},
		{"unattended single role", confirmOpts{Roles: 1}, false},
		{"unattended handful", confirmOpts{Roles: confirmBulkThreshold}, false},
		{"unattended bulk", confirmOpts{Roles: confirmBulkThreshold + 1}, true},
		{"unattended --all", confirmOpts{All: true, Roles: 2}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.opts.NeedsConfirmation(); got != c.want {
				t.Errorf("NeedsConfirmation() = %v, want %v", got, c.want)
			}
		})
	}
}
