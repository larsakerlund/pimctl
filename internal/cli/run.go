// The spine every command runs on: resolving contexts and opening sessions,
// the run-scoped state they share, and the two things that must be said out
// loud afterwards — that a run was cut short, and which scopes went unread.
// The commands themselves, and the ARM calls, are elsewhere.

package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/larsakerlund/pimctl/internal/term"
)

// runContext bundles what every command needs once contexts are resolved.
//
// It is built by [prepare] and torn down by [runContext.finish], which every
// command defers: finish is what cancels Ctx and waits for the background
// listing, so a runContext that is never finished leaks a goroutine and a
// request in flight. It is not safe for concurrent use beyond the parts
// documented as such — names and bg.
type runContext struct {
	// Timeouts are the run's time budgets, carried here rather than kept in
	// package variables a test has to mutate and put back.
	Timeouts timeouts
	// Opts is the run's flag state, carried here rather than read from a
	// package variable: the background listing outlives the command's own
	// goroutine, and a test that set a flag once left it set for the next one.
	Opts     *globalOpts
	Res      resolution // which contexts were chosen, and how.
	Sessions []*session // one per context that opened, in the order they were named.
	// Failures are the contexts that could not be opened. They are warnings, not
	// a reason to stop, but a run that has any is incomplete and must exit
	// non-zero — see [partialFailureError].
	Failures []error
	Timings  *timings // the --debug recorder; inert, never nil, when --debug is off.
	// Started is when prepare was entered, so the --debug total covers context
	// resolution and token acquisition rather than just the ARM calls.
	Started time.Time
	// Flags the background listing needs, captured here rather than read from
	// the package-level `global` at use time: the fan-out runs on a goroutine
	// that can outlive the command, and reading mutable package state from
	// there is unsound even when it happens to be written only once.
	AllScopes bool
	Refresh   bool // --refresh: ignore the caches for this run.

	// Wait removes the per-scope soft deadline, for callers that have asked for
	// the authoritative answer however long it takes.
	Wait bool

	// Ctx is the context every ARM call must use. It is cancelled by finish, so
	// a background listing the command stopped waiting for is torn down rather
	// than left running — a request still in flight when the test server closes
	// stalls it, and in production it is work nobody reads.
	//nolint:containedctx // request-scoped by construction: one command
	// invocation, carrying the cancel that tears down the background listing.
	Ctx    context.Context
	cancel context.CancelFunc // called by finish, which is what tears the background work down.
	// bg counts the background listings finish must wait for.
	bg sync.WaitGroup

	// names remembers each scope's display name, so a message about a scope can
	// name it the way the tables do.
	names scopeNames
}

// scopeNames maps scope ids to display names for the run.
//
// The fan-out knows only scope ids, so a message about a slow or unreadable
// scope has to be named the way the tables name it — "Contoso landing zones
// (contoso-prod)", not a bare "contoso-prod" — and the fan-out sees only ids. Learning
// the names as the eligibility listing is read costs nothing and keeps one
// vocabulary.
type scopeNames struct {
	mu   sync.RWMutex      // guards both fields; the fan-out reads while the listing writes.
	byID map[string]string // scope id to display name, as far as this run has seen.
	// labeler is built from byID on first use and dropped whenever a new name
	// is learned. Rebuilding it per call walked the whole map for every scope
	// labelled, which is quadratic in a report that names them all.
	labeler *scopeLabeler
}

// learn records one scope's display name. It is called from the eligibility
// read, which runs one goroutine per session, so it takes the write lock; an
// empty id or name is ignored rather than stored as a blank label.
func (n *scopeNames) learn(id, name string) {
	if id == "" || name == "" {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.byID == nil {
		n.byID = map[string]string{}
	}
	n.byID[id] = name
	// One more name can make a previously unique display name ambiguous, so
	// the built labeler is no longer valid.
	n.labeler = nil
}

// label renders one scope id, falling back to its leaf when no name was seen.
// It builds the labeler on first use and keeps it until a new name is learned,
// so labelling N scopes costs one walk of the map rather than N.
func (n *scopeNames) label(id string) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.labeler == nil {
		refs := make([]scopeRef, 0, len(n.byID))
		for scopeID, name := range n.byID {
			refs = append(refs, scopeRef{Name: name, ID: scopeID})
		}
		built := newScopeLabeler(refs)
		n.labeler = &built
	}
	return n.labeler.Label(n.byID[id], id)
}

// prepare resolves contexts, tells the user which one is being used, and opens
// a session per context. presetContexts lets a named preset supply the contexts
// when none were given on the command line.
func prepare(cmd *cobra.Command, opts *globalOpts, d deps, presetContexts []string) (*runContext, error) {
	started := time.Now()
	res, err := resolveContexts(opts.contexts, opts.allContexts, opts.bareAz, presetContexts, os.Getenv)
	if err != nil {
		return nil, err
	}
	// Only worth saying when the user did not type it: -c is self-evident.
	if res.Source != SourceFlag {
		if d := res.Describe(); d != "" {
			fmt.Fprintln(cmd.ErrOrStderr(), d)
		}
	}
	t := newTimings(opts.debug)
	sp := term.NewSpinner(cmd.ErrOrStderr(), "authenticating…")
	sessions, failures, err := d.openSessions(res, t, opts.refresh)
	sp.Stop()
	if err != nil {
		return nil, err
	}
	// The shared az login is announced only once a token exists, because the
	// tenant and user come from its claims.
	if res.Bare && len(sessions) > 0 {
		line := describeAzLogin(sessions[0].Token.TenantID, sessions[0].Token.UserPrincipalName)
		fmt.Fprintln(cmd.ErrOrStderr(), line)
		t.note(line)
	}
	ctx, cancel := context.WithCancel(cmd.Context())
	return &runContext{
		Opts: opts, Timeouts: d.timeouts,
		Res: res, Sessions: sessions, Failures: failures, Timings: t, Started: started,
		AllScopes: opts.allScopes, Refresh: opts.refresh,
		Ctx: ctx, cancel: cancel,
	}, nil
}

// finish cancels any background listing, waits for it to unwind, and prints the
// timing breakdown when --debug is set. Every command defers it.
func (rc *runContext) finish(cmd *cobra.Command) {
	rc.cancel()
	rc.bg.Wait()
	rc.Timings.Report(cmd.ErrOrStderr(), time.Since(rc.Started))
}

// abortedEarly reports whether the run was interrupted before it could produce
// meaningful output. Commands check this before printing an empty result: after
// a Ctrl-C an empty listing means "we never got the data", not "there is
// nothing", and saying "No eligible Azure resource roles." on stdout would be a
// false statement about the tenant.
func abortedEarly(ctx context.Context) bool { return ctx.Err() != nil }

// reportUnconfirmedScopes names scopes whose listing failed or was cut short. Naming
// them is the whole point: "N roles active" from an incomplete read would be a
// quiet under-report, which for an access tool is worse than a slow answer.
func reportUnconfirmedScopes(cmd *cobra.Command, rc *runContext, scopes []activationScope) {
	if len(scopes) == 0 {
		return
	}
	fmt.Fprintf(
		cmd.ErrOrStderr(),
		"%d scope(s) unconfirmed (ARM did not answer): %s — their last known state is kept; `pimctl status --wait` reads them properly\n",
		len(scopes),
		strings.Join(labelScopes(rc, scopes), ", "),
	)
}

// labelScopes renders scope ids for a human, the way the tables do.
func labelScopes(rc *runContext, ids []activationScope) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		label := rc.names.label(id.ID)
		if id.ID == "" {
			label = "all scopes"
		}
		if len(rc.Sessions) > 1 {
			label = id.Context + ": " + label
		}
		out = append(out, label)
	}
	return out
}
