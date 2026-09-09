// The bounded-concurrency runner `up` and `down` both submit through: it owns
// the concurrency bound, the result ordering, and the serialisation of the
// progress callback that reports each role the moment it lands — a batch takes
// as long as its slowest role, and printing nothing until then makes a working
// command look hung. What one request actually does is request.go.

package cli

import (
	"context"
	"sync"
	"time"
)

// submitAll fires one request per item, at most [maxConcurrency] at a time, and
// returns the results in the order the items were given — so the numbering in
// the results table matches the plan the user just confirmed. The bound is the
// same 8 the policy lookups use, and it is a bound on in-flight ARM requests,
// not a worker pool: every item gets a goroutine, which then waits its turn.
//
// progress is called once per role as it lands, and is serialised: two roles
// finishing together must not interleave halfway through a line, nor race on
// the activation record.
func submitAll[T any](items []T, progress func(result), submit func(T) result) []result {
	results := make([]result, len(items))
	sem := make(chan struct{}, maxConcurrency)
	var (
		wg       sync.WaitGroup
		reportMu sync.Mutex
	)
	for i := range items {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res := submit(items[idx])
			results[idx] = res
			if progress != nil {
				reportMu.Lock()
				progress(res)
				reportMu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	return results
}

// executeActivations fires every planned activation and then polls each one to
// a terminal status unless --no-wait was given.
func executeActivations(
	ctx context.Context,
	plan []*planItem,
	justification, ticketNumber, ticketSystem string,
	noWait bool,
	pollTimeout time.Duration,
	progress func(result),
) []result {
	return submitAll(plan, progress, func(item *planItem) result {
		return activateOne(ctx, item, justification, ticketNumber, ticketSystem, noWait, pollTimeout)
	})
}

// executeDeactivations gives up every target through the same runner, bound and
// ordering as [executeActivations], so `down` cannot quietly acquire concurrency
// behaviour that `up` does not have. Only the per-item call differs.
func executeDeactivations(
	ctx context.Context,
	rows []target,
	noWait bool,
	pollTimeout time.Duration,
	progress func(result),
) []result {
	return submitAll(rows, progress, func(row target) result {
		return deactivateOne(ctx, row, noWait, pollTimeout)
	})
}
