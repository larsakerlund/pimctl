// Reading what Azure says is activated right now: the per-scope fan-out, the
// future the commands poll it through, and the row type it produces. Merging
// that answer with this machine's own record of what it activated is
// reconcile.go's job; giving a role back is deactivate.go's.

package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/larsakerlund/pimctl/internal/armclient"
	"github.com/larsakerlund/pimctl/internal/cache"
)

// activeRow is one live activation, with the session that can deactivate it.
type activeRow struct {
	// Context is the token label the row was read under, and Session is the one
	// that can act on it. Both are needed: --all-contexts merges rows from
	// several tenants into one table.
	Context    string
	Session    *session             // as above.
	Assignment armclient.Assignment // ARM's own row, or the record's rendering of one.
	// State says how far the row can be trusted, so a fast or a lagging answer
	// is never mistaken for an authoritative one.
	State rowState
}

// rowState is where a row's claim comes from.
type rowState int

const (
	// RowConfirmed means Azure listed this role as activated.
	RowConfirmed rowState = iota
	// RowConfirming means this machine activated it and Azure has not caught
	// up. Azure's listing lags a fresh activation by minutes, so its absence
	// there is not evidence of anything yet; the role's own schedule request is.
	RowConfirming
	// RowUnconfirmed means the row comes from this machine's record because
	// Azure did not answer for its scope at all.
	RowUnconfirmed
)

// Unconfirmed reports whether Azure has confirmed the row.
func (r activeRow) Unconfirmed() bool { return r.State != RowConfirmed }

// String names the state for JSON output.
func (s rowState) String() string {
	switch s {
	case RowConfirming:
		return "confirming"
	case RowUnconfirmed:
		return "unconfirmed"
	case RowConfirmed:
		return "confirmed"
	default:
		return "confirmed"
	}
}

// marker is the state's suffix in the table, empty when Azure confirmed it.
func (s rowState) marker() string {
	switch s {
	case RowConfirming:
		return "~"
	case RowUnconfirmed:
		return "?"
	case RowConfirmed:
		return ""
	default:
		return ""
	}
}

// activationScope identifies a scope within the session that discovered it.
// An empty ID asks for that session's tenant-wide listing when it has no
// eligible scopes. Context is a session label, never discarded during merging.
type activationScope struct {
	Context string // session label owning this scope.
	ID      string // full ARM scope id, empty for a tenant-wide fallback.
}

// key identifies a scope without conflating equal management-group names in
// different contexts. ARM ids are case-insensitive; context labels are preserved.
func (s activationScope) key() string { return s.Context + "|" + strings.ToLower(s.ID) }

// sortScopes orders scope references by context and then ARM id.
func sortScopes(scopes []activationScope) {
	slices.SortFunc(scopes, func(a, b activationScope) int { return strings.Compare(a.key(), b.key()) })
}

// activeResult is the outcome of the background activation listing.
type activeResult struct {
	rows []activeRow // every activation the read did see.
	errs []error     // contexts or scopes that failed outright, as opposed to timing out.
	// unconfirmed holds the scope ids whose call missed the soft deadline. Their
	// state is unknown, not empty — the difference matters, so they are named
	// rather than silently under-reported.
	unconfirmed []activationScope
}

// timeouts are a run's time budgets, gathered in one value on [runContext].
//
// They were package variables, which meant a test shortening one had to
// remember to put it back, and two tests could not have wanted different values
// at the same time. Every field is a measurement or a judgement about ARM, and
// the comments say which.
type timeouts struct {
	// scopeSoftDeadline bounds one per-scope activation listing.
	//
	// Roughly one fan-out in three has a single random scope take 12-15s, which
	// turned a 2.4s median into a p90 of 16.8s and a worst case of 25s. The slow
	// scope is different every time, so it cannot be predicted or hedged around.
	// Cutting each call short and naming what was missed keeps the command
	// responsive; --wait trades it for waitScope and reads properly.
	scopeSoftDeadline time.Duration
	// waitScope is the per-scope bound for a thorough read: generous enough for
	// the 12-15s outliers, still bounded so one wedged call cannot hang the CLI.
	waitScope time.Duration
	// verify bounds the whole request-confirmation step, which runs after the
	// listing on a path a human is watching: it may add a little, not a lot.
	verify time.Duration
	// activeBackfill bounds the wait for the activation listing when an
	// already-active result still needs its end time. By that point the PUTs
	// have made a full ARM round-trip, so it has usually landed already.
	activeBackfill time.Duration
	// poll bounds how long a request is polled for a terminal status before it
	// is reported as "still <status>" — a failure, not an approval.
	poll time.Duration
}

// The production time budgets, each measured or reasoned about where the field
// it fills is documented.
const (
	defaultScopeSoftDeadline = 3 * time.Second   // one per-scope listing.
	defaultWaitScope         = 60 * time.Second  // one per-scope listing under --wait.
	defaultVerify            = 5 * time.Second   // the whole request-confirmation step.
	defaultActiveBackfill    = 3 * time.Second   // waiting for an already-active role's end time.
	defaultPoll              = 120 * time.Second // polling one request to a terminal status.
)

// defaultTimeouts are the production budgets. A test builds its own value
// instead of reaching for a package variable.
func defaultTimeouts() timeouts {
	return timeouts{
		scopeSoftDeadline: defaultScopeSoftDeadline,
		waitScope:         defaultWaitScope,
		verify:            defaultVerify,
		activeBackfill:    defaultActiveBackfill,
		poll:              defaultPoll,
	}
}

// activeFuture is the activation listing, fetched in the background, and read
// at most once from its channel: the first [activeFuture.Wait] or
// [activeFuture.TryGet] to receive latches the result so every later caller
// gets the same answer. The zero value is not usable; a nil future is, and
// always reports "not arrived".
//
// Reading activation state is the expensive half of pimctl however it is done:
// ARM's roleAssignmentScheduleInstances?$filter=asTarget() takes 12–19 seconds
// on this tenant — reproducibly, and independently of pimctl — and the per-scope
// fan-out that replaced it still costs seconds. Eligibility comes back in about
// two. Blocking the picker on the slow call made every `up` a quarter-minute of
// silence, so the two are decoupled: eligibility drives the UI, activation state
// arrives when it arrives and is used if it is there.
type activeFuture struct {
	ch  chan activeResult // buffered, size 1, so the producer never blocks.
	res activeResult      // the received result, once got is set.
	got bool              // whether ch has been read; a channel can only give it up once.
	mu  sync.Mutex        // guards res and got, which several waiters may reach at once.
}

// Wait blocks up to timeout. A negative timeout waits indefinitely. ok reports
// whether the listing arrived in time; a timed-out future stays usable.
func (f *activeFuture) Wait(timeout time.Duration) (res activeResult, ok bool) {
	if f == nil {
		return activeResult{}, false
	}
	f.mu.Lock()
	if f.got {
		defer f.mu.Unlock()
		return f.res, true
	}
	f.mu.Unlock()

	var timer <-chan time.Time
	if timeout >= 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timer = t.C
	}
	select {
	case got := <-f.ch:
		f.mu.Lock()
		f.res, f.got = got, true
		f.mu.Unlock()
		return got, true
	case <-timer:
		return activeResult{}, false
	}
}

// TryGet returns the listing only if it has already arrived. It never blocks —
// Wait(0) would arm a timer and race it, which is not the same thing.
func (f *activeFuture) TryGet() (activeResult, bool) {
	if f == nil {
		return activeResult{}, false
	}
	f.mu.Lock()
	if f.got {
		defer f.mu.Unlock()
		return f.res, true
	}
	f.mu.Unlock()
	select {
	case got := <-f.ch:
		f.mu.Lock()
		f.res, f.got = got, true
		f.mu.Unlock()
		return got, true
	default:
		return activeResult{}, false
	}
}

// startActivationListing kicks off [listActivations] without waiting for it,
// and returns the future to poll. The goroutine is registered with rc, so
// [runContext.finish] tears it down; scopes nil means "work them out from the
// eligibility listing".
func startActivationListing(ctx context.Context, rc *runContext, scopes []activationScope) *activeFuture {
	f := &activeFuture{ch: make(chan activeResult, 1)}
	rc.bg.Go(func() {
		rows, errs, unconfirmed := listActivations(ctx, rc, scopes)
		f.ch <- activeResult{rows: rows, errs: errs, unconfirmed: unconfirmed}
	})
	return f
}

// maxScopeFanOut caps how many per-scope listings run at once.
//
// The fan-out's wall time is ceil(scopes/concurrency) x slowest call, so any
// limit below the scope count costs a whole extra wave: measured on a 25-scope
// tenant, 16 took 3.95s in two waves and 25 took 2.44s in one. The cap exists
// only to stop an unusually large tenant opening hundreds of concurrent
// requests — back-to-back bursts do provoke transient ARM errors, which the
// Retry-After path handles but which are worth not inviting.
const maxScopeFanOut = 32

// scopeFanOutFor sizes the fan-out to the work, so the common tenant runs in a
// single wave.
func scopeFanOutFor(scopes int) int {
	if scopes < maxScopeFanOut {
		return max(scopes, 1)
	}
	return maxScopeFanOut
}

// scopeTally accumulates the fan-out's results and its timing figures. Every
// field is guarded by mu: the per-scope calls all write into one tally.
type scopeTally struct {
	mu    sync.Mutex        // guards every field below.
	rows  []activeRow       // activations found, before deduplication across nested scopes.
	errs  []error           // scopes that answered with an error rather than late.
	slow  []activationScope // scope ids that missed the deadline: unknown, not empty.
	calls int               // per-scope calls made, for the --debug line.
	maxD  time.Duration     // the slowest call, which is what sets the wall time.
	sumD  time.Duration     // every call's duration added up, to show what concurrency bought.
}

// record folds one per-scope call's duration into the fan-out's figures, for
// the --debug line. The caller holds t.mu.
func (t *scopeTally) record(d time.Duration) {
	t.calls++
	t.sumD += d
	if d > t.maxD {
		t.maxD = d
	}
}

// listActivationsAtScope reads one scope's activations, bounded by its own deadline so a
// single slow call cannot hold the whole answer hostage.
func listActivationsAtScope(ctx context.Context, rc *runContext, s *session, scope string, tally *scopeTally) {
	deadline := rc.Timeouts.scopeSoftDeadline
	if rc.Wait {
		// --wait asks for the authoritative answer, so the deadline becomes the
		// command's overall budget rather than the snappiness budget.
		deadline = rc.Timeouts.waitScope
	}
	callCtx, cancelCall := context.WithTimeout(ctx, deadline)
	defer cancelCall()

	callStart := time.Now()
	var assignments []armclient.Assignment
	err := retryOn401(s, func() error {
		var e error
		assignments, e = s.Client.ListAssignmentsAtScope(callCtx, scope)
		return e
	})
	d := time.Since(callStart)

	tally.mu.Lock()
	defer tally.mu.Unlock()
	tally.record(d)
	if err != nil {
		// A scope that timed out, or that ARM is still throttling after the
		// retries, is *unknown* — not empty. Naming it keeps the difference
		// visible instead of quietly reporting fewer roles than are held.
		if (callCtx.Err() != nil && ctx.Err() == nil) || isThrottled(err) {
			// The scope id, not a label: this list is matched against record
			// entries as well as printed, and labelling it here once made every
			// row at an unread scope silently vanish. Presentation happens at
			// the point of printing.
			tally.slow = append(tally.slow, activationScope{Context: s.Token.Label(), ID: scope})
			return
		}
		// One unreadable scope must not sink the rest: the user may simply have
		// lost access to it since the cache was written.
		tally.errs = append(tally.errs,
			fmt.Errorf("listing active roles at %s in %s: %w", rc.names.label(scope), s.Token.Label(), err))
		return
	}
	for _, a := range assignments {
		if a.IsActivated() {
			tally.rows = append(tally.rows, activeRow{Context: s.Token.Label(), Session: s, Assignment: a})
		}
	}
}

// listActivations lists the user's live activations.
//
// By default it fans out over the distinct scopes the user is eligible at,
// rather than making ARM's tenant-wide asTarget() call. That call takes 11-21
// seconds and silently drops rows: three runs against an unchanged tenant
// returned 126, 131 and 132 instances with no nextLink. --all-scopes restores
// the old behaviour.
//
// scopes nil means "work them out from the eligibility listing", which is
// cached and therefore usually free.
func listActivations(
	ctx context.Context,
	rc *runContext,
	scopes []activationScope,
) (rows []activeRow, errs []error, unconfirmed []activationScope) {
	if rc.AllScopes {
		r, e := tenantWideActivations(ctx, rc)
		return r, e, nil
	}
	var scopeErrs []error
	if scopes == nil {
		// The fan-out needs to know where to look. Eligibility is the cheap
		// listing — well under a second even cold — so read it rather than
		// falling back to the tenant-wide call that is known to drop rows.
		scopes, scopeErrs = eligibleScopes(ctx, rc)
	}
	if len(scopes) == 0 {
		if len(scopeErrs) > 0 {
			// The eligibility read failed, so "no scopes" is not a fact about
			// the tenant. Falling back here would answer with a listing known
			// to be lossy instead of saying what went wrong.
			return nil, scopeErrs, nil
		}
		// Eligible for nothing anywhere: the tenant-wide call is the only way
		// left to notice an activation at a scope the listing cannot suggest.
		r, e := tenantWideActivations(ctx, rc)
		return r, e, nil
	}

	tally := &scopeTally{}
	sem := make(chan struct{}, scopeFanOutFor(len(scopes)))
	var wg sync.WaitGroup
	start := time.Now()
	for _, scope := range scopes {
		sess := sessionFor(rc.Sessions, scope.Context)
		if sess == nil {
			continue
		}
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			listActivationsAtScope(ctx, rc, sess, scope.ID, tally)
		})
	}
	wg.Wait()

	if rc.Timings != nil {
		rc.Timings.record(
			fmt.Sprintf("ARM active fan-out (%d scopes, max %.2fs, summed %.2fs)",
				tally.calls, tally.maxD.Seconds(), tally.sumD.Seconds()),
			time.Since(start),
		)
	}

	out := dedupeActivations(tally.rows)
	sortActivations(out)
	sortScopes(tally.slow)
	return out, slices.Concat(scopeErrs, tally.errs), slices.Compact(tally.slow)
}

// dedupeActivations folds duplicates by ARM instance id: the same activation can be
// returned by more than one scope query when scopes nest.
func dedupeActivations(rows []activeRow) []activeRow {
	seen := map[string]bool{}
	out := make([]activeRow, 0, len(rows))
	for _, r := range rows {
		id := r.Context + "|" + strings.ToLower(r.Assignment.ID)
		if r.Assignment.ID != "" && seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, r)
	}
	return out
}

// sortActivations orders activations for stable display.
func sortActivations(out []activeRow) {
	slices.SortStableFunc(out, func(a, b activeRow) int {
		return cmp.Or(
			cmp.Compare(a.Context, b.Context),
			cmp.Compare(a.Assignment.RoleName(), b.Assignment.RoleName()),
			cmp.Compare(a.Assignment.ScopeName(), b.Assignment.ScopeName()),
		)
	})
}

// eligibleScopes returns the distinct scopes the user is eligible at, from the
// cache when it is warm and from ARM when it is not.
func eligibleScopes(ctx context.Context, rc *runContext) (scopes []activationScope, errs []error) {
	add := func(label string, elig []armclient.Eligibility) {
		for _, e := range elig {
			rc.names.learn(e.Properties.Scope, e.ScopeName())
		}
		scopes = append(scopes, scopesFor(label, elig)...)
	}
	for _, s := range rc.Sessions {
		label := s.Token.Label()
		if !rc.Refresh {
			if cached, _, ok := cache.Read(s.owner()); ok {
				add(label, cached)
				continue
			}
		}
		// A cold cache is not a reason to give up on the fan-out. Reading the
		// eligibilities costs well under a second, and it is the difference
		// between a per-scope read and the tenant-wide call that is slow and
		// drops rows: `cache clear` followed by a piped `status` took two
		// minutes and exited 1 because this returned nothing here.
		var elig []armclient.Eligibility
		err := rc.Timings.Track("ARM roleEligibilityScheduleInstances ("+label+")", func() error {
			return retryOn401(s, func() error {
				var e error
				elig, e = s.Client.ListEligibilities(ctx)
				return e
			})
		})
		if err != nil {
			// Swallowing this is what made the fallback silent — and a 401 on a
			// freshly minted token, which retryOn401 exists to absorb, was not
			// even retried. The caller decides what to do; it must not read
			// this as "eligible for nothing".
			errs = append(errs, fmt.Errorf("listing eligible roles in %s: %w", label, err))
			continue
		}
		cache.Write(s.owner(), elig)
		add(label, elig)
	}
	sortScopes(scopes)
	return scopes, errs
}

// tenantWideActivations makes ARM's single roleAssignmentScheduleInstances call
// per session instead of the per-scope fan-out. It is what --all-scopes asks
// for, and the only way left to notice an activation when the eligibility
// listing cannot suggest a scope to look at.
func tenantWideActivations(ctx context.Context, rc *runContext) ([]activeRow, []error) {
	var out []activeRow
	var errs []error
	rc.Timings.TrackVoid("ARM roleAssignmentScheduleInstances (tenant-wide)", func() {
		out, errs = listTenantWide(ctx, rc.Sessions)
	})
	return out, errs
}

// listTenantWide is the call itself, once per session. As with
// readEligibilities, a failing context does not discard the others' results.
func listTenantWide(ctx context.Context, sessions []*session) ([]activeRow, []error) {
	var (
		mu   sync.Mutex
		out  []activeRow
		errs []error
		wg   sync.WaitGroup
	)
	for _, s := range sessions {
		wg.Add(1)
		go func(s *session) {
			defer wg.Done()
			active, err := s.Client.ListActivated(ctx)
			if err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("listing active roles in %s: %w", s.Token.Label(), err))
				mu.Unlock()
				return
			}
			mu.Lock()
			for _, a := range active {
				out = append(out, activeRow{Context: s.Token.Label(), Session: s, Assignment: a})
			}
			mu.Unlock()
		}(s)
	}
	wg.Wait()
	sortActivations(out)
	return out, errs
}

// isThrottled reports whether ARM refused to answer rather than answering
// "nothing". The client already retries these honouring Retry-After; if it is
// still throttled after that, the scope is unread, not empty.
func isThrottled(err error) bool {
	var apiErr *armclient.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.StatusCode {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}
