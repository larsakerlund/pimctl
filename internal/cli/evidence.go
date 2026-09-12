// What a named deactivation knows about held roles while its requests are in
// flight: this machine's record at once, and the activation listing when it
// has landed. Deciding which roles to give up is deactivate.go; the listing
// itself is active.go, and the record it is merged with is localrecord.go.

package cli

import (
	"context"
	"sync"
	"time"
)

// activeEvidence is the held-role evidence a named `down` consults lazily.
//
// A named selection — a preset, --project, --role, --scope or --key — carries
// its own scopes and role ids, so its requests can go out on the strength of
// the eligibility rows and the local record alone. The activation listing is
// the expensive call (a per-scope fan-out of seconds, up to the soft deadline),
// and it changes only two things for such a run: whether a "no such
// assignment" answer reads as NOT ACTIVE or as a failure to give up a role
// that is held, and whether a filter that matched nothing else can still find
// an activation only the listing knows. So the listing starts in the
// background and is waited for only at those two points.
//
// The value is shared by the goroutines that submit the requests, so every
// method is safe for concurrent use; the merge with the record happens once,
// under mu, the first time the listing is taken.
type activeEvidence struct {
	rc     *runContext   // the run, for its sessions, budgets and the verification calls.
	future *activeFuture // the listing in flight; never nil once started.
	// started and budget bound how long a request's answer may wait on the
	// listing: budget is the listing's own deadline, measured from started.
	started time.Time
	budget  time.Duration // see listingBudget.

	mu     sync.Mutex   // guards everything below; the merge runs under it.
	local  localRecord  // this machine's record, whose verdicts are filled on merge.
	landed bool         // whether res and rows reflect the listing.
	res    activeResult // the listing as it landed; the zero value until then.
	// rows is what is currently believed held: the record's rows alone until
	// the listing lands, and the record merged with the listing afterwards,
	// exactly as readActivations merges them for a bare `down`.
	rows []activeRow
}

// startActiveEvidence reads the local record and starts the activation listing
// in the background, over scopes when given and over the eligible and recorded
// scopes otherwise; see [startActivationListing]. The goroutine is registered
// with rc, so [runContext.finish] tears it down.
func startActiveEvidence(rc *runContext, scopes []activationScope) *activeEvidence {
	local := readLocalRecord(rc)
	return &activeEvidence{
		rc:      rc,
		future:  startActivationListing(rc.Ctx, rc, scopes),
		started: time.Now(),
		budget:  listingBudget(rc.Timeouts, rc.Wait),
		local:   local,
		rows:    deactivationCandidates(local, nil),
	}
}

// listingBudget is how long the listing may take before a deactivation stops
// waiting for it: one wave of the per-scope fan-out, each call cut at the
// soft deadline (or the --wait bound when wait is set), plus the step that
// confirms recorded activations against their own requests. Past that sum the
// listing has landed unless it is on the tenant-wide fallback, whose 12-20 s
// a request that ARM has already answered does not sit out.
func listingBudget(tm timeouts, wait bool) time.Duration {
	scope := tm.scopeSoftDeadline
	if wait {
		scope = tm.waitScope
	}
	return scope + tm.verify
}

// remaining is what is left of the listing's own budget, never negative.
func (e *activeEvidence) remaining() time.Duration {
	return e.remainingOf(e.budget)
}

// remainingOf is what is left of budget, measured from the moment the listing
// started and never negative. A caller that may wait for only part of the
// listing's budget — the widening pass, which has the per-scope soft deadline
// and not the confirmation step on top of it — names its own.
func (e *activeEvidence) remainingOf(budget time.Duration) time.Duration {
	return max(0, budget-time.Since(e.started))
}

// candidates returns the rows currently believed held: the record's before
// the listing lands, the merge afterwards. It never blocks, but does take the
// listing if it has landed since the last look.
func (e *activeEvidence) candidates() []activeRow {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.landed {
		if res, ok := e.future.TryGet(); ok {
			e.merge(res)
		}
	}
	return e.rows
}

// wait blocks up to timeout for the listing and returns the merged rows and
// whether the listing is in them. A negative timeout waits indefinitely; zero
// only takes a listing that has already landed. The record's rows come back
// either way, so a caller that times out still has what this machine knows.
// It blocks other callers for as long as it waits.
func (e *activeEvidence) wait(timeout time.Duration) ([]activeRow, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.landed {
		return e.rows, true
	}
	var (
		res activeResult
		ok  bool
	)
	if timeout == 0 {
		// Wait(0) would arm a timer and race it; TryGet is the honest zero.
		res, ok = e.future.TryGet()
	} else {
		res, ok = e.future.Wait(timeout)
	}
	if ok {
		e.merge(res)
	}
	return e.rows, ok
}

// merge folds the landed listing into the record the way readActivations
// does for a bare `down`: each recorded activation the listing omits is
// checked against its own schedule request, and the candidates are the
// listing's rows minus this machine's tombstones plus every recorded role not
// found to be gone. It makes the confirmation calls, bounded by the verify
// budget. The caller holds e.mu.
func (e *activeEvidence) merge(res activeResult) {
	e.res, e.landed = res, true
	e.local.verdicts = verifyConfirming(e.rc.Ctx, e.rc, res.rows, res.unconfirmed)
	e.rows = deactivationCandidates(e.local, res.rows)
}

// held reports whether the listing has landed and, merged with the record,
// shows t held. It is false while the listing is in flight, whatever the
// record says: a recorded activation the listing has not caught up with is
// this machine's own claim, and merge asks its schedule request before
// counting it. It never blocks; a caller that needs the listing calls wait.
func (e *activeEvidence) held(t target) bool {
	rows, landed := e.wait(0)
	if !landed {
		return false
	}
	for _, r := range rows {
		if recordKey(r.Context, r.Assignment.Properties.Scope, r.Assignment.Properties.RoleDefinitionID) == t.key() {
			return true
		}
	}
	return false
}

// gaps returns what the listing could not read — the scopes that failed and
// the ones that missed the deadline — or nil while it has not landed, when
// nothing has been checked and so nothing has been found wanting. It takes a
// listing that has landed since the last look, without blocking.
func (e *activeEvidence) gaps() (errs []error, unconfirmed []activationScope) {
	e.candidates()
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.landed {
		return nil, nil
	}
	return e.res.errs, e.res.unconfirmed
}

// deactivate submits one named target and reads ARM's answer against the
// evidence. The request goes out at once. Only a "no such assignment" answer
// for a target nothing had yet shown held waits for the listing, for what is
// left of its budget: if the listing then shows the role held, the answer
// means the assignment has not finished propagating, so the request is sent
// once more with that evidence attached and [deactivateOne] reads the second
// answer as it reads one for a listed role — a failure if ARM still denies
// the assignment, DEACTIVATED if it has propagated in between. A listing that
// does not show the role, or does not land in time, leaves NOT ACTIVE
// standing: ARM has said plainly that the role is not held, and nothing
// contradicts it.
func (e *activeEvidence) deactivate(ctx context.Context, t target, noWait bool, pollTimeout time.Duration) result {
	if !t.SeenActive && e.held(t) {
		// The listing landed while earlier requests were in flight.
		t.SeenActive = true
	}
	res := deactivateOne(ctx, t, noWait, pollTimeout)
	if res.Outcome != OutcomeNotActive || t.SeenActive {
		return res
	}
	if _, ok := e.wait(e.remaining()); !ok || !e.held(t) {
		return res
	}
	t.SeenActive = true
	return deactivateOne(ctx, t, noWait, pollTimeout)
}
