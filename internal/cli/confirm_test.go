// Covers confirm.go: the confirmation policy — never in the interactive flow,
// and unattended only for --all or a selection above the bulk threshold.

package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
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

// TestConfirmPlanAsksOnlyForAllOrMoreThanTenRoles pins the documented rule
// through confirmPlanWith itself: an unattended --all, or more than ten roles, is
// put to the user; anything smaller, anything with -y, and anything picked by
// hand goes ahead with the plan printed and no question asked. Whether a
// question was asked is observed the one way a unit test can: with stdin off a
// terminal, asking fails with errNoConfirmTTY and prints no plan, while not
// asking returns yes and prints it.
func TestConfirmPlanAsksOnlyForAllOrMoreThanTenRoles(t *testing.T) {
	cases := []struct {
		name string
		opts confirmOpts
		asks bool
	}{
		{"one role", confirmOpts{Roles: 1}, false},
		{"exactly ten roles", confirmOpts{Roles: confirmBulkThreshold}, false},
		{"eleven roles", confirmOpts{Roles: confirmBulkThreshold + 1}, true},
		{"--all with two roles", confirmOpts{All: true, Roles: 2}, true},
		{"--all with -y", confirmOpts{All: true, Roles: 2, Yes: true}, false},
		{"eleven roles with -y", confirmOpts{Roles: confirmBulkThreshold + 1, Yes: true}, false},
		{"eleven roles from the picker", confirmOpts{Roles: confirmBulkThreshold + 1, Interactive: true}, false},
		{"--all from the picker", confirmOpts{All: true, Roles: 40, Interactive: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := NewRootCmd()
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(io.Discard)
			printed := 0
			printPlan := func(w io.Writer) { printed++; fmt.Fprintln(w, "About to activate:") }

			ok, err := confirmPlanWith(cmd, &globalOpts{output: "table"}, noTTY(), tc.opts, printPlan)
			if tc.asks {
				if !errors.Is(err, errNoConfirmTTY) {
					t.Fatalf("a run that must ask cannot ask without a terminal, got ok=%v err=%v", ok, err)
				}
				if ok || printed != 0 {
					t.Errorf("nothing may be printed or approved when the question cannot be put: ok=%v printed=%d",
						ok, printed)
				}
				return
			}
			if err != nil || !ok {
				t.Fatalf("a run that need not ask must go ahead, got ok=%v err=%v", ok, err)
			}
			if printed != 1 || !strings.Contains(out.String(), "About to activate:") {
				t.Errorf("the plan must still be printed to stdout: printed=%d out=%q", printed, out.String())
			}
		})
	}
}

// TestConfirmPlanKeepsJSONOutputClean: a run that goes ahead without asking
// prints its plan only to a table run, because under -o json the plan would
// land inside the document a caller is parsing.
func TestConfirmPlanKeepsJSONOutputClean(t *testing.T) {
	cmd := NewRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	printed := false
	ok, err := confirmPlanWith(
		cmd, &globalOpts{output: outputJSON}, ttyOn(true, true, true), confirmOpts{Roles: 1},
		func(io.Writer) { printed = true },
	)
	if err != nil || !ok {
		t.Fatalf("got ok=%v err=%v", ok, err)
	}
	if printed || out.Len() != 0 {
		t.Errorf("the plan must not be printed under -o json: printed=%v out=%q", printed, out.String())
	}
}
