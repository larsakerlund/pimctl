// From a selection to a plan, and from a run to a verdict: each role's PIM
// policy, the duration that survives it, and the [result]/[outcome] vocabulary
// every command reports in. Sending the requests is execute.go and request.go;
// printing them is report.go.

package cli

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/larsakerlund/pimctl/internal/armclient"
	"github.com/larsakerlund/pimctl/internal/cache"
	"github.com/larsakerlund/pimctl/internal/store"
)

// planItem is one role about to be activated, with its policy resolved and its
// request GUID already fixed. The GUID is generated once per role per run so a
// retry updates the same request instead of creating a duplicate.
type planItem struct {
	Row     row      // the eligible role this activates.
	Session *session // the context's token and client, so the plan can be executed as it stands.
	// Settings is the role's PIM policy, nil when it could not be read — in
	// which case PrepErr says why and nothing is sent.
	Settings *armclient.RoleSettings
	// Duration is the activation length after clamping to the policy maximum.
	Duration time.Duration
	// Capped is true when the requested duration exceeded the policy maximum.
	Capped bool
	// Requested is what the user asked for, zero when they asked for the max.
	Requested time.Duration
	// RequestName is the GUID the request is PUT under, fixed here so a retry
	// updates the same request instead of creating a second one.
	RequestName string
	// PrepErr is a problem found before anything was sent to ARM, e.g. the
	// policy could not be read or a required ticket number is missing.
	PrepErr error
}

// resolveDuration clamps a requested duration to a policy maximum. A zero or
// negative request means "use the policy maximum".
func resolveDuration(requested, policyMax time.Duration) (resolved time.Duration, capped bool) {
	if policyMax <= 0 {
		return requested, false
	}
	if requested <= 0 {
		return policyMax, false
	}
	if requested > policyMax {
		return policyMax, true
	}
	return requested, false
}

// maxConcurrency bounds both the policy lookups and the activation calls.
const maxConcurrency = 8

// buildPlan resolves the PIM policy for every selected role and works out the
// duration each activation will request. Policy lookups run concurrently and
// are cached per (scope, role) inside each session's client.
//
// ticketNumber is only read to decide whether a ticket-requiring policy can be
// satisfied; the ticket system name is not needed until the request is built.
func buildPlan(
	ctx context.Context,
	rows []row,
	sessions []*session,
	requested time.Duration,
	ticketNumber string,
	refresh bool,
) []*planItem {
	items := make([]*planItem, len(rows))
	sem := make(chan struct{}, maxConcurrency)
	var (
		wg      sync.WaitGroup
		freshMu sync.Mutex
		// fresh collects the policies actually read from ARM, so they can be
		// persisted in one write rather than one per role.
		fresh = map[store.Owner]map[string]*armclient.RoleSettings{}
	)
	for i, r := range rows {
		items[i] = &planItem{
			Row:         r,
			Session:     sessionFor(sessions, r.Context),
			Requested:   requested,
			RequestName: uuid.NewString(),
		}
		wg.Add(1)
		go func(item *planItem) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if item.Session == nil {
				item.PrepErr = fmt.Errorf("no session for context %q", item.Row.Context)
				return
			}
			scope := item.Row.Elig.Properties.Scope
			roleDef := item.Row.Elig.Properties.RoleDefinitionID
			owner := item.Session.owner()

			// Two sequential ARM GETs, about 1.2s per role, on the critical
			// path of the command people run most. A persisted copy skips them.
			s := cache.LookupPolicy(owner, scope, roleDef, refresh)
			if s == nil {
				var err error
				if s, err = item.Session.Client.GetRoleSettings(ctx, scope, roleDef); err != nil {
					item.PrepErr = fmt.Errorf("could not read the PIM policy: %w", err)
					return
				}
				freshMu.Lock()
				if fresh[owner] == nil {
					fresh[owner] = map[string]*armclient.RoleSettings{}
				}
				fresh[owner][cache.PolicyKey(scope, roleDef)] = s
				freshMu.Unlock()
			}
			item.Settings = s
			item.Duration, item.Capped = resolveDuration(requested, s.MaximumDuration)
			if s.RequiresTicket() && ticketNumber == "" {
				item.PrepErr = errors.New(
					"this role's policy requires ticket information; pass --ticket-number (and --ticket-system)",
				)
			}
		}(items[i])
	}
	wg.Wait()
	for owner, settings := range fresh {
		cache.StorePolicies(owner, settings)
	}
	return items
}

// outcome is the per-role result category printed in the results table.
type outcome string

// The outcomes a role can end a run with. These strings are printed verbatim
// in the results table and in the JSON, so they are part of the same public
// contract as [result]'s field names; [exitCodeFor] is what turns them into an
// exit code.
const (
	OutcomeActivated     outcome = "ACTIVATED"        // the role is held.
	OutcomeDeactivated   outcome = "DEACTIVATED"      // the role was given up.
	OutcomeAlreadyActive outcome = "ALREADY ACTIVE"   // it was already held; not a failure.
	OutcomePending       outcome = "PENDING APPROVAL" // an approver has to act; exit 2.
	OutcomeSubmitted     outcome = "SUBMITTED"        // sent under --no-wait, so its fate is unknown by design.
	OutcomeWaiting       outcome = "STILL PENDING"    // no terminal status within the poll timeout; a failure, with no approver to wait for.
	OutcomeFailed        outcome = "FAILED"           // ARM refused it, and Detail says why.
	// OutcomeAborted is a role the user interrupted before pimctl learned what
	// happened to it. Deliberately distinct from FAILED: the request may well
	// have been granted, so the honest answer is "check pimctl status".
	OutcomeAborted outcome = "ABORTED"
	// OutcomeSkipped is a role never attempted, because the run was interrupted
	// before its turn came up.
	OutcomeSkipped outcome = "SKIPPED"
	// OutcomeNotActive is a role we were asked to give up that ARM says is not
	// held. Not a failure: the requested end state is the actual end state.
	OutcomeNotActive outcome = "NOT ACTIVE"
)

// result is what happened to one role.
//
// Its JSON encoding is a public contract: `pimctl up -o json` and `down -o json`
// emit an array of these, and the field names follow the same lowerCamel scheme
// as `list` and `status` so one jq vocabulary works across every command.
// Renaming a field, or dropping an omitempty, breaks scripts that are already
// written against it.
type result struct {
	Owner store.Owner `json:"-"` // authenticated owner for internal cache and record writes.
	// Key is the same selection key `ls` and `status` print, so a script can
	// feed a result straight back into `pimctl down --key`.
	Key       string `json:"key"`
	Context   string `json:"context"`   // which cloudctx context this role was acted on in.
	Role      string `json:"role"`      // the role's display name.
	ScopeName string `json:"scopeName"` // the scope's display name, which several scopes may share.
	// ScopeLabel is scopeName as the tables print it, disambiguated — a
	// management group always carries its own name, and a display name shared
	// by two scopes carries the leaf that tells them apart. `ls` and `status`
	// emit it too; a reader matching on scopeName alone can conflate two
	// scopes that a person reading the table never would.
	ScopeLabel string `json:"scopeLabel"`
	ScopeType  string `json:"scopeType,omitempty"` // Subscription, ManagementGroup, ResourceGroup or Resource.
	Scope      string `json:"scope"`               // the full ARM scope id.
	// RoleDefinitionID identifies the role for follow-up lookups.
	RoleDefinitionID string `json:"roleDefinitionId,omitempty"`
	// Outcome is what happened, and the only field the exit code is derived
	// from.
	Outcome outcome `json:"outcome"`
	// Status is ARM's request status, when a request was actually created.
	Status string `json:"status,omitempty"`
	// RequestID is the full ARM id of the schedule request, for auditing.
	RequestID string `json:"requestId,omitempty"`
	// RequestName is the GUID pimctl chose for the request, which is the last
	// path element of RequestID.
	RequestName string `json:"requestName,omitempty"`
	// Since is when ARM says the activation started, which is not quite when
	// pimctl asked for it. Nil when ARM did not say.
	Since *time.Time `json:"since,omitempty"`
	// Until is when the window ends, named as in ls and status. Nil when there
	// is no window: a failure, or a deactivation.
	Until *time.Time `json:"until,omitempty"`
	// Detail is the reason behind the outcome — ARM's message, or pimctl's
	// explanation of what to do — and is never truncated in the table.
	Detail string `json:"detail,omitempty"`
	// Recovery is the command that re-mints a token satisfying a Conditional
	// Access claims challenge, when that is what went wrong.
	Recovery string `json:"recovery,omitempty"`
}

// IsFailure reports whether this result should make the process exit non-zero.
func (r result) IsFailure() bool { return r.Outcome == OutcomeFailed }

// IsInterrupted reports whether the user cancelled before this role resolved.
func (r result) IsInterrupted() bool {
	return r.Outcome == OutcomeAborted || r.Outcome == OutcomeSkipped
}

// IsPending reports whether an approver still has to act on this request. This
// is the only outcome that earns exit code 2 — which on a deactivation means
// the role is still held, not that it is not yet held.
func (r result) IsPending() bool { return r.Outcome == OutcomePending }

// IsUnfinished reports whether the request never reached a terminal status
// within the poll timeout. The access is not held, so this counts as a failure
// — it is emphatically not "pending approval", which is a state an approver can
// actually resolve.
func (r result) IsUnfinished() bool { return r.Outcome == OutcomeWaiting }

// exitCodeFor maps a set of results onto pimctl's exit code contract: 1 if
// anything failed or never finished, 2 if nothing failed but something is
// waiting on an approver, 0 otherwise.
func exitCodeFor(results []result) int {
	pending := false
	for _, r := range results {
		if r.IsFailure() || r.IsUnfinished() || r.IsInterrupted() {
			return ExitFailed
		}
		if r.IsPending() {
			pending = true
		}
	}
	if pending {
		return ExitPending
	}
	return ExitOK
}
