// Reading the eligible-role listing, which is the cheap half of pimctl: it is
// cached per context, and it is what the picker and every table are built from.
// This file starts the activation fan-out but does not perform it — that is
// active.go — and it does not decide which rows a command acts on.

package cli

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/larsakerlund/pimctl/internal/armclient"
	"github.com/larsakerlund/pimctl/internal/cache"
	"github.com/larsakerlund/pimctl/internal/term"
)

// readEligibilities lists eligible roles, serving them from the per-context
// cache when it is fresh, and starts the activation listing in the background.
// Activations are never cached — `status` and the ACTIVE marker have to reflect
// what is held right now.
func readEligibilities(ctx context.Context, cmd *cobra.Command, rc *runContext) ([]row, []error, *activeFuture) {
	sp := term.NewSpinner(cmd.ErrOrStderr(), "reading eligible roles…")
	defer sp.Stop()

	var (
		mu      sync.Mutex
		rows    []row
		scopes  []activationScope
		errs    []error
		wg      sync.WaitGroup
		oldest  time.Duration
		cacheOK = true
	)
	for _, s := range rc.Sessions {
		wg.Add(1)
		go func(s *session) {
			defer wg.Done()
			label := s.Token.Label()

			var (
				elig      []armclient.Eligibility
				fromCache bool
				age       time.Duration
			)
			if !rc.Refresh {
				if cached, a, ok := cache.Read(label); ok {
					elig, age, fromCache = cached, a, true
				}
			}
			if !fromCache {
				err := rc.Timings.Track("ARM roleEligibilityScheduleInstances ("+label+")", func() error {
					return retryOn401(s, func() error {
						var e error
						elig, e = s.Client.ListEligibilities(ctx)
						return e
					})
				})
				if err != nil {
					mu.Lock()
					errs = append(errs, fmt.Errorf("listing eligible roles in %s: %w", label, err))
					mu.Unlock()
					return
				}
			}

			mu.Lock()
			defer mu.Unlock()
			if fromCache {
				if age > oldest {
					oldest = age
				}
			} else {
				cacheOK = false
				cache.Write(label, elig)
			}
			scopes = append(scopes, scopesFor(label, elig)...)
			for _, e := range elig {
				rc.names.learn(e.Properties.Scope, e.ScopeName())
				rows = append(rows, row{Context: label, Elig: e})
			}
		}(s)
	}
	wg.Wait()
	sp.Stop()

	if cacheOK && len(rc.Sessions) > 0 && len(errs) == 0 {
		fmt.Fprintf(
			cmd.ErrOrStderr(),
			"(eligible roles cached %s ago — pass --refresh to re-read)\n",
			cache.FormatAge(oldest),
		)
	}
	rows = dedupeRows(rows)
	sortRows(rows)

	// The fan-out needs the eligibility scopes, so it starts once they are
	// known — which is the point at which the picker can already be shown.
	sortScopes(scopes)
	future := startActivationListing(ctx, rc, scopes)
	return rows, errs, future
}

// scopesFor retains the context for each distinct eligibility scope. A session
// with no eligible scopes gets one tenant-wide fallback, never another tenant's
// scopes. The result is sorted for deterministic discovery.
func scopesFor(label string, elig []armclient.Eligibility) []activationScope {
	seen := map[string]bool{}
	out := make([]activationScope, 0, len(elig))
	for _, e := range elig {
		id := e.Properties.Scope
		if id == "" || seen[strings.ToLower(id)] {
			continue
		}
		seen[strings.ToLower(id)] = true
		out = append(out, activationScope{Context: label, ID: id})
	}
	if len(out) == 0 {
		out = append(out, activationScope{Context: label})
	}
	sortScopes(out)
	return out
}

// applyActive marks rows using activation identity, including their context.
func applyActive(rows []row, active []activeRow) []row {
	return matchActivations(rows, active)
}
