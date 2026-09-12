// Opening one ARM session per resolved context, and keeping its token usable
// once it is open. Which contexts to open is decided in context.go, and the
// tokens themselves are minted by internal/azauth; this file only pairs one
// with a client and absorbs the 401 that a revoked cached token produces.

package cli

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/larsakerlund/pimctl/internal/armclient"
	"github.com/larsakerlund/pimctl/internal/azauth"
	"github.com/larsakerlund/pimctl/internal/store"
)

// session is one context's token plus the ARM client bound to it.
//
// Everything a command does against a tenant goes through a session, so a token
// for one tenant can never reach another's client. Token is an immutable
// identity snapshot; retryOn401 updates only the client's bearer credential
// after checking that its tenant and principal still match. The zero value is
// not usable — sessions only come from openSessionsWith.
type session struct {
	Context     string            // the cloudctx context name; empty for the shared az login.
	Token       *azauth.Token     // immutable initial token and identity claims.
	Client      *armclient.Client // bound to that identity, with a refreshable bearer token.
	refreshOnce sync.Once         // coordinates one refresh across all concurrent requests.
	refreshErr  error             // written by refreshOnce and read only after it completes.
}

// owner returns the immutable account identity for persisted state. A missing
// session has no owner and must not write or reuse account-owned data.
func (s *session) owner() store.Owner {
	if s == nil || s.Token == nil {
		return store.Owner{}
	}
	return s.Token.Owner()
}

// labelOf names a context for messages; the ambient az login has no name.
func labelOf(name string) string {
	if name == "" {
		return "(bare az)"
	}
	return name
}

// sessionOpener mints the sessions a run acts through. It is a field on [deps]
// rather than a package variable so a test can substitute a fake ARM backend
// for one command tree instead of for the process.
type sessionOpener func(res resolution, t *timings, refresh bool) ([]*session, []error, error)

// deps are the collaborators a command tree uses, injected at construction.
//
// There is one of them today. It is a struct rather than a bare function
// parameter because the next seam should join it here instead of becoming a
// second package variable, which is what these were before.
type deps struct {
	openSessions sessionOpener // defaults to [openSessionsWith]; replaced in tests.
	timeouts     timeouts      // the run's time budgets; shortened in tests.
}

// defaultDeps is what [NewRootCmd] uses: the real ARM-backed opener and the
// production time budgets.
func defaultDeps() deps {
	return deps{openSessions: openSessionsWith, timeouts: defaultTimeouts()}
}

// openSessionsWith mints one ARM token per context.
//
// A context that cannot produce a token does not abort the others: its error is
// collected and returned alongside the sessions that did open, so
// `--all-contexts` still does useful work when one tenant's login has expired.
// The command reports the failures and exits non-zero, so a partial run can
// never be mistaken for a complete one. Only a total failure — no session at
// all — is fatal.
//
// Several contexts open concurrently: each one pays ~0.65 s for the `cloudctx
// show` spawn and, with no cached token, ~1.2 s for the mint, and one after
// another that is N times the wait for `--all-contexts`. Opening them at once is safe
// because a context that is not logged in fails fast — `az account
// get-access-token` reports it rather than starting a device-code flow — so
// no two of them can compete for the terminal. The sessions, the failures and
// the --debug lines still come out in the order the contexts were named, not
// the order the mints finished, and a single context is opened on the calling
// goroutine.
//
// refresh is passed in rather than read from the flags so a caller can force a
// fresh mint. It records a token span per context in t, and notes whether the
// token came from the cache — never the token itself.
func openSessionsWith(res resolution, t *timings, refresh bool) ([]*session, []error, error) {
	names := res.Names
	if res.Bare {
		names = []string{""}
	}
	slots := make([]openedSession, len(names))
	if len(names) == 1 {
		slots[0] = openSession(names[0], refresh)
	} else {
		var wg sync.WaitGroup
		for i, name := range names {
			wg.Go(func() { slots[i] = openSession(name, refresh) })
		}
		wg.Wait()
	}
	var (
		sessions = make([]*session, 0, len(names))
		failures []error
	)
	for i, name := range names {
		slot := slots[i]
		if t != nil && t.enable {
			t.record("token ("+labelOf(name)+")", slot.took)
		}
		if slot.err != nil {
			failures = append(failures, slot.err)
			continue
		}
		// Only whether it was a hit — never the token.
		state := "miss (minted via cloudctx/az)"
		if slot.session.Token.FromCache {
			state = "cache hit"
		}
		t.note("token " + labelOf(name) + ": " + state)
		sessions = append(sessions, slot.session)
	}
	if len(sessions) == 0 {
		if len(failures) == 0 {
			// Nothing failed because nothing was tried. Wrapping an empty
			// errors.Join here rendered as "%!w(<nil>)", which tells a reader
			// nothing at all.
			return nil, nil, errors.New("no context to act on")
		}
		return nil, failures, fmt.Errorf("no context could be used:\n%w", errors.Join(failures...))
	}
	return sessions, failures, nil
}

// openedSession is what opening one context produced, parked in that context's
// slot until every context has answered so the results can be read out in the
// order the contexts were named rather than the order the mints finished.
type openedSession struct {
	session *session      // the open session; nil when the mint failed.
	err     error         // why the mint failed; nil when it did not.
	took    time.Duration // how long the mint took, for the --debug breakdown.
}

// openSession mints one context's token and binds an ARM client to it. It
// spawns cloudctx/az through [azauth.AcquireCached] unless a usable cached
// token exists, and measures the wait either way so the caller can attribute
// it in the --debug breakdown once every context has answered. The error is
// [azauth.AcquireCached]'s, which names the context and carries az's stderr.
func openSession(name string, refresh bool) openedSession {
	start := time.Now()
	tok, err := azauth.AcquireCached(name, azauth.DefaultRunner, refresh)
	took := time.Since(start)
	if err != nil {
		return openedSession{err: err, took: took}
	}
	return openedSession{
		session: &session{
			Context: name,
			Token:   tok,
			Client:  armclient.New(armclient.DefaultHost, tok.AccessToken, nil),
		},
		took: took,
	}
}

// reportContextFailures prints per-context problems as warnings. They go to
// stderr so `-o json` stdout stays machine-readable.
func reportContextFailures(w io.Writer, failures []error) {
	for _, err := range failures {
		fmt.Fprintf(w, "warning: %v\n", err)
	}
}

// sessionFor finds the session a row belongs to by the label its rows were
// tagged with, and returns nil when there is none. Rows carry a label rather
// than a pointer because they survive being cached and re-read, so the session
// has to be looked up again when the row is finally acted on.
func sessionFor(sessions []*session, label string) *session {
	for _, s := range sessions {
		if s.Token.Label() == label {
			return s
		}
	}
	return nil
}

// retryOn401 retries one rejected call after a session-wide token refresh.
// Concurrent calls share the same refresh. The session identity never changes:
// a new account requires a new command and selection, rather than continuing
// a plan assembled for another principal.
func retryOn401(s *session, fn func() error) error {
	err := fn()
	var apiErr *armclient.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized || s.Token == nil ||
		!s.Token.FromCache {
		return err
	}
	s.refreshOnce.Do(func() { s.refreshErr = refreshSession(s) })
	if s.refreshErr != nil {
		return errors.Join(err, s.refreshErr)
	}
	return fn()
}

// refreshSession replaces a rejected credential after checking its identity.
// It is called once per session, drops the rejected cache entry, and stores
// only a replacement for the same tenant and principal. Failures are returned
// without changing the client's credential or the session's identity.
func refreshSession(s *session) error {
	azauth.DropTokenCache(s.Token.Context, azauth.DefaultRunner)
	fresh, err := azauth.Acquire(s.Token.Context, azauth.DefaultRunner)
	if err != nil {
		return err
	}
	if !s.owner().Matches(fresh.Owner()) {
		return errors.New("the signed-in account changed; rerun pimctl to select roles for the new account")
	}
	s.Client.SetToken(fresh.AccessToken)
	azauth.WriteTokenCache(fresh, azauth.DefaultRunner)
	return nil
}
