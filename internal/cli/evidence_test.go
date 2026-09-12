// Tests for evidence.go: what a named deactivation believes held before and
// after the background listing lands, and how long it may wait for it. The
// command-level behaviour built on it is tested in deactivate_test.go.

package cli

import (
	"context"
	"testing"
	"time"
)

func TestListingBudgetIsTheListingsOwnDeadline(t *testing.T) {
	tm := timeouts{scopeSoftDeadline: 3 * time.Second, waitScope: 60 * time.Second, verify: 5 * time.Second}
	if got, want := listingBudget(tm, false), 8*time.Second; got != want {
		t.Errorf("budget = %v, want %v", got, want)
	}
	if got, want := listingBudget(tm, true), 65*time.Second; got != want {
		t.Errorf("budget under --wait = %v, want %v", got, want)
	}
}

func TestActiveEvidenceHeldOnlyOnceTheListingLands(t *testing.T) {
	rc := &runContext{Timeouts: defaultTimeouts(), Ctx: context.Background()}
	future := &activeFuture{ch: make(chan activeResult, 1)}
	ev := &activeEvidence{rc: rc, future: future, started: time.Now(), budget: time.Minute}

	a := mkActivated("Reader", testProjectRole, testProjectSubscription, "Dev", time.Now().Add(time.Hour))
	a.ID = "/1"
	tgt := targetFromActive(activeRow{Context: "contoso", Assignment: a})

	if ev.held(tgt) {
		t.Fatal("held before the listing landed")
	}
	if rows, ok := ev.wait(0); ok || len(rows) != 0 {
		t.Fatalf("wait(0) = %d rows, %t; want nothing, false", len(rows), ok)
	}
	if errs, unconfirmed := ev.gaps(); errs != nil || unconfirmed != nil {
		t.Fatalf("gaps reported before the listing landed: %v %v", errs, unconfirmed)
	}

	unread := activationScope{Context: "contoso", ID: testProjectSubscription}
	future.ch <- activeResult{
		rows:        []activeRow{{Context: "contoso", Assignment: a}},
		unconfirmed: []activationScope{unread},
	}
	if rows, ok := ev.wait(time.Second); !ok || len(rows) != 1 {
		t.Fatalf("wait after landing = %d rows, %t; want 1, true", len(rows), ok)
	}
	if !ev.held(tgt) {
		t.Error("the listed role is not held")
	}
	other := tgt
	other.Scope += "/resourceGroups/other"
	if ev.held(other) {
		t.Error("a role the listing did not show is held")
	}
	if _, unconfirmed := ev.gaps(); len(unconfirmed) != 1 || unconfirmed[0] != unread {
		t.Errorf("gaps = %v, want the unread scope", unconfirmed)
	}
}

func TestActiveEvidenceRemainingNeverGoesNegative(t *testing.T) {
	ev := &activeEvidence{started: time.Now().Add(-time.Hour), budget: time.Second}
	if got := ev.remaining(); got != 0 {
		t.Errorf("remaining = %v, want 0 once the budget is spent", got)
	}
	ev = &activeEvidence{started: time.Now(), budget: time.Hour}
	if got := ev.remaining(); got <= 0 || got > time.Hour {
		t.Errorf("remaining = %v, want what is left of an hour", got)
	}
	if got := ev.remainingOf(time.Minute); got <= 0 || got > time.Minute {
		t.Errorf("remainingOf a minute = %v, want what is left of it", got)
	}
	if got := ev.remainingOf(0); got != 0 {
		t.Errorf("remainingOf nothing = %v, want 0", got)
	}
}
