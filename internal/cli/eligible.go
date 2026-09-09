// Reading the eligible-role listing, which is the cheap half of pimctl: it is
// cached per context, and it is what the picker and every table are built from.
// This file starts the activation fan-out but does not perform it — that is
// active.go — and it does not decide which rows a command acts on.

package cli

import (
	"context"
	"fmt"
	"slices"
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
				retried := false
				err := rc.Timings.Track("ARM roleEligibilityScheduleInstances ("+label+")", func() error {
					return retryOn401(s, &retried, func() error {
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
			for _, e := range elig {
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
	scopes := distinctScopes(rows)
	future := startActivationListing(ctx, rc, scopes)
	return rows, errs, future
}

// distinctScopes lists the scopes a row set covers.
func distinctScopes(rows []row) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range rows {
		sc := r.Elig.Properties.Scope
		if sc == "" || seen[strings.ToLower(sc)] {
			continue
		}
		seen[strings.ToLower(sc)] = true
		out = append(out, sc)
	}
	slices.Sort(out)
	return out
}

// applyActive marks the rows covered by an activation listing.
func applyActive(rows []row, active []activeRow) []row {
	assignments := make([]armclient.Assignment, 0, len(active))
	for _, a := range active {
		assignments = append(assignments, a.Assignment)
	}
	return matchActivations(rows, assignments)
}
