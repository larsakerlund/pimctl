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

	"github.com/larsakerlund/pimctl/internal/armclient"
	"github.com/larsakerlund/pimctl/internal/azauth"
)

// session is one context's token plus the ARM client bound to it.
//
// Everything a command does against a tenant goes through a session, so a token
// for one tenant can never reach another's client. It is mutable: [retryOn401]
// replaces Token and re-points Client at the new one. The zero value is not
// usable — sessions only come from [openSessions].
type session struct {
	Context string            // the cloudctx context name; empty for the shared az login.
	Token   *azauth.Token     // the ARM token and the claims read out of it.
	Client  *armclient.Client // bound to that token, and so to one tenant.
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
// refresh is passed in rather than read from the flags so a caller can force a
// fresh mint. It
// records a token span per context in t, and notes whether the token came from
// the cache — never the token itself.
func openSessionsWith(res resolution, t *timings, refresh bool) ([]*session, []error, error) {
	names := res.Names
	if res.Bare {
		names = []string{""}
	}
	var (
		sessions = make([]*session, 0, len(names))
		failures []error
	)
	for _, name := range names {
		var tok *azauth.Token
		err := t.Track("token ("+labelOf(name)+")", func() error {
			var e error
			tok, e = azauth.AcquireCached(name, azauth.DefaultRunner, refresh)
			return e
		})
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if t != nil {
			// Only whether it was a hit — never the token.
			state := "miss (minted via cloudctx/az)"
			if tok.FromCache {
				state = "cache hit"
			}
			t.note("token " + labelOf(name) + ": " + state)
		}
		sessions = append(sessions, &session{
			Context: name,
			Token:   tok,
			Client:  armclient.New(armclient.DefaultHost, tok.AccessToken, nil),
		})
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

// retryOn401 re-runs fn once with a freshly minted token when ARM rejects the
// cached one.
//
// A cached token can be revoked, or invalidated by a Conditional Access policy
// change, before it expires. Without this the user would have to work out for
// themselves that a stale file was the problem; with it the first 401 is
// invisible and self-healing. Only one retry: a second 401 is a real
// authorization failure and must be reported.
func retryOn401(s *session, refreshed *bool, fn func() error) error {
	err := fn()
	var apiErr *armclient.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized {
		return err
	}
	if s.Token == nil || !s.Token.FromCache || (refreshed != nil && *refreshed) {
		return err
	}
	azauth.DropTokenCache(s.Token.Context, azauth.DefaultRunner)
	fresh, acquireErr := azauth.AcquireCached(s.Token.Context, azauth.DefaultRunner, true)
	if acquireErr != nil {
		// Report the original 401: it is the more informative of the two.
		return err
	}
	s.Token = fresh
	s.Client.SetToken(fresh.AccessToken)
	if refreshed != nil {
		*refreshed = true
	}
	return fn()
}
