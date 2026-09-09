// Asking ARM about an activation's own schedule request, which is current
// minutes before the per-scope listing is. Deciding which activations are
// worth asking about happens here; acting on the answers is reconcile.go and
// localrecord.go.

package cli

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/larsakerlund/pimctl/internal/armclient"
)

// ARM's per-scope listing of activated roles runs minutes behind its own
// writes: measured on a real tenant, activations were still missing from the
// listing four and a half minutes after ARM had accepted and provisioned them.
// Trusting that listing alone makes `status` announce that nothing is activated
// while the roles are held.
//
// A fixed grace period cannot fix it. Any constant is either shorter than some
// real lag, and reports access as lost while it is held, or long enough to keep
// reporting access that is gone; the lag has no upper bound worth betting on.
//
// So pimctl asks instead of waiting. Every activation it performed has a
// schedule request, and that request reports Provisioned as soon as the role is
// granted — minutes before the listing catches up. One cheap GET per unlisted
// activation turns a guess into evidence.

// entryVerdict is what an activation's own schedule request says about it.
type entryVerdict int

const (
	// verdictUnknown: ARM could not be asked, or gave no usable answer. The
	// entry stands until the ceiling, because absence of evidence about a role
	// pimctl activated is not evidence that it is gone.
	verdictUnknown entryVerdict = iota
	// verdictHeld: the request is provisioned, so the role is held whatever the
	// listing currently says.
	verdictHeld
	// verdictGone: the request was revoked, denied, cancelled or failed, so the
	// entry is wrong and must go — with the loss reported, never in silence.
	verdictGone
)

// verifyConfirming asks ARM about each recorded activation its listing has not
// shown, and returns one verdict per row selection key.
//
// Only entries that need it are asked about: a role ARM has just listed needs no
// second opinion, nor does one at a scope ARM never answered for, where the
// listing says nothing to contradict.
func verifyConfirming(
	ctx context.Context,
	rc *runContext,
	active []activeRow,
	unconfirmedScopes []string,
) map[string]entryVerdict {
	listed := map[string]bool{}
	for _, a := range active {
		listed[activeSelectionKey(a)] = true
	}
	unread := unreadScopes(unconfirmedScopes)

	var ask []confirmQuestion
	now := time.Now()
	for _, s := range rc.Sessions {
		label := s.Token.Label()
		for _, e := range readRecord(label) {
			switch {
			case !e.Confirming(now), listed[entryKey(e)], unread[strings.ToLower(e.Scope)]:
				continue
			case e.RequestID == "":
				// An entry from an older version, or one written before the
				// request id was known. Nothing to ask; the ceiling covers it.
				continue
			}
			ask = append(ask, confirmQuestion{key: entryKey(e), requestID: e.RequestID, session: s})
		}
	}
	if len(ask) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, rc.Timeouts.verify)
	defer cancel()

	// Named for --debug: this step is ARM calls on the critical path of
	// `status`, and an unnamed one just swells "(other)" and looks like nothing.
	span := fmt.Sprintf("confirm %d activation(s) against their requests", len(ask))

	var (
		mu       sync.Mutex
		verdicts = make(map[string]entryVerdict, len(ask))
		wg       sync.WaitGroup
		sem      = make(chan struct{}, maxConcurrency)
	)
	rc.Timings.TrackVoid(span, func() {
		askAll(ctx, ask, &wg, sem, &mu, verdicts)
	})
	return verdicts
}

// askAll reads every outstanding request concurrently.
func askAll(
	ctx context.Context,
	ask []confirmQuestion,
	wg *sync.WaitGroup,
	sem chan struct{},
	mu *sync.Mutex,
	verdicts map[string]entryVerdict,
) {
	for _, q := range ask {
		wg.Add(1)
		go func(q confirmQuestion) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			v := verdictUnknown
			if sr, err := q.session.Client.GetRequest(ctx, q.requestID); err == nil {
				v = verdictFor(sr)
			}
			mu.Lock()
			verdicts[q.key] = v
			mu.Unlock()
		}(q)
	}
	wg.Wait()
}

// confirmQuestion is one activation to ask ARM about.
type confirmQuestion struct {
	key       string   // the row selection key the verdict is filed under.
	requestID string   // the full ARM id of the schedule request to read back.
	session   *session // which context to ask, since a run may span several.
}

// verdictFor reads one schedule request.
func verdictFor(sr *armclient.ScheduleRequest) entryVerdict {
	if sr == nil {
		return verdictUnknown
	}
	switch sr.Properties.Status {
	case armclient.StatusProvisioned, armclient.StatusGranted:
		return verdictHeld
	case armclient.StatusRevoked, armclient.StatusCanceled,
		"Denied", "Failed", "Expired", "RevokedAndCanceled", "AdminDenied":
		return verdictGone
	default:
		// Accepted, PendingApproval, Provisioning and anything new: ARM has not
		// finished deciding, which is not the same as deciding against us.
		return verdictUnknown
	}
}
