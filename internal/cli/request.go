// Everything between one planned role and its outcome: the two ARM request
// bodies — the same PUT with a different requestType, so activation and
// deactivation cannot drift apart — the submit-and-poll loop, and the mapping
// from an ARM status or error onto a [result]. Which roles get here, and for
// how long, is decided in plan.go; printing what came back is report.go.

package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/larsakerlund/pimctl/internal/armclient"
	"github.com/larsakerlund/pimctl/internal/cache"
)

// baseResult is the part of a [result] that is known before anything is sent:
// the row's identity and the request GUID [buildPlan] already fixed for it.
// Outcome is deliberately left at its zero value — every caller sets it.
func baseResult(item *planItem) result {
	return result{
		Owner:            item.Session.owner(),
		Key:              item.Row.SelectionKey(),
		Context:          item.Row.Context,
		Role:             item.Row.Elig.RoleName(),
		ScopeName:        item.Row.Elig.ScopeName(),
		ScopeType:        item.Row.Elig.ScopeType(),
		Scope:            item.Row.Elig.Properties.Scope,
		RoleDefinitionID: item.Row.Elig.Properties.RoleDefinitionID,
		RequestName:      item.RequestName,
	}
}

// buildActivateBody assembles the SelfActivate request body. It mirrors the
// template proven by this tenant's own successful activation history, and the
// three fields ARM is strict about are all in it:
//
//   - principalId is the signed-in user's own object id — the ARM token's oid
//     claim, passed in as principalID — even when the eligibility is inherited
//     through a group. Sending the group's id is rejected.
//   - roleDefinitionId is re-qualified to the activation scope by
//     [armclient.QualifyRoleDefinitionID]. Both scopes are the eligibility's own
//     here, since pimctl activates at the scope the eligibility was granted at,
//     so today that returns the id unchanged.
//   - linkedRoleEligibilityScheduleId goes through exactly as the eligibility
//     reported it. Rewriting it makes ARM reject the request.
//
// now is the requested start; the window's length comes from the plan item's
// already-clamped duration. ticketNumber and ticketSystem are only sent when
// the role's policy has the Ticketing rule, because an unwanted ticketInfo is
// itself a policy violation.
func buildActivateBody(
	item *planItem,
	principalID, justification, ticketNumber, ticketSystem string,
	now time.Time,
) armclient.RequestBody {
	scope := item.Row.Elig.Properties.Scope
	props := armclient.RequestProperties{
		PrincipalID: principalID,
		RoleDefinitionID: armclient.QualifyRoleDefinitionID(
			scope,
			scope,
			item.Row.Elig.Properties.RoleDefinitionID,
		),
		RequestType:                     armclient.RequestTypeSelfActivate,
		LinkedRoleEligibilityScheduleID: item.Row.Elig.Properties.RoleEligibilityScheduleID,
		Justification:                   justification,
		ScheduleInfo: &armclient.ScheduleInfo{
			StartDateTime: now.UTC().Format(time.RFC3339),
			Expiration: armclient.Expiration{
				Type:     "AfterDuration",
				Duration: armclient.FormatISODuration(item.Duration),
			},
		},
	}
	// ticketInfo is sent only when the policy's Ticketing rule is enabled.
	if item.Settings != nil && item.Settings.RequiresTicket() {
		props.TicketInfo = &armclient.TicketInfo{TicketNumber: ticketNumber, TicketSystem: ticketSystem}
	}
	return armclient.RequestBody{Properties: props}
}

// activateOne submits one role's SelfActivate request and, unless noWait is
// set, polls it to a terminal status. It never returns an error: a failure is
// an outcome, because one role going wrong must not abandon the rest of the
// batch.
//
// It returns without touching the network when the plan already found a problem
// with this role, and when ctx is cancelled before its turn came up — reported
// as SKIPPED rather than letting the dead context surface as an ARM rejection.
// A cancellation during the submit or the poll gives ABORTED, which says the
// request may well have been granted and is deliberately not FAILED.
//
// justification, ticketNumber and ticketSystem go into the request body;
// the two ticket fields are only sent when the role's policy has the Ticketing
// rule.
func activateOne(
	ctx context.Context,
	item *planItem,
	justification, ticketNumber, ticketSystem string,
	noWait bool,
	pollTimeout time.Duration,
) result {
	res := baseResult(item)
	if item.PrepErr != nil {
		res.Outcome = OutcomeFailed
		res.Detail = item.PrepErr.Error()
		return res
	}
	// The user interrupted before this role's turn came up. Report it as never
	// attempted rather than letting SubmitRequest fail on the dead context and
	// look like an ARM rejection.
	if ctx.Err() != nil {
		res.Outcome = OutcomeSkipped
		res.Detail = "not attempted — the run was interrupted"
		return res
	}
	body := buildActivateBody(
		item,
		item.Session.Token.PrincipalID,
		justification,
		ticketNumber,
		ticketSystem,
		time.Now(),
	)
	sr, err := item.Session.Client.SubmitRequest(ctx, item.Row.Elig.Properties.Scope, item.RequestName, body)
	if err != nil {
		if ctx.Err() != nil {
			res.Outcome = OutcomeAborted
			res.Detail = "interrupted while submitting — check `pimctl status`"
			return res
		}
		return applyRequestError(res, item.Session, err, item.Row.ActiveUntil())
	}
	res.RequestID = sr.ID
	res.Status = sr.Properties.Status
	if !noWait {
		polled, perr := item.Session.Client.Poll(ctx, sr, pollTimeout)
		if polled != nil {
			sr = polled
			res.Status = sr.Properties.Status
		}
		switch {
		case errors.Is(perr, context.Canceled), errors.Is(perr, context.DeadlineExceeded):
			// Do not pretend we waited out the poll timeout: say the request was
			// submitted and its fate is unknown.
			res.Outcome = OutcomeAborted
			res.Detail = fmt.Sprintf("interrupted while waiting (last status %s) — check `pimctl status`", res.Status)
			return res
		case perr != nil:
			res.Outcome = OutcomeFailed
			res.Detail = perr.Error()
			return res
		}
	}
	return classifyRequest(res, sr, noWait, OutcomeActivated, pollTimeout)
}

// classifyRequest turns an ARM request status into a user-facing outcome.
// successOutcome differs between activation and deactivation; everything else
// is shared so the two commands cannot drift apart.
//
// The four cases, in the order they are tried: a success status becomes
// successOutcome, and for an activation also carries the window ARM granted;
// a pending-approval status becomes PENDING APPROVAL, the one outcome that
// earns exit code 2, because the change has not taken effect and only an
// approver can make it; an explicit failure status becomes FAILED;
// and anything still short of a terminal status is SUBMITTED under --no-wait
// or STILL PENDING otherwise. STILL PENDING is a plain failure (exit 1), not a
// second flavour of "waiting for an approver": there is nobody to resolve it.
func classifyRequest(
	res result,
	sr *armclient.ScheduleRequest,
	noWait bool,
	successOutcome outcome,
	pollTimeout time.Duration,
) result {
	status := sr.Properties.Status
	requestType := armclient.RequestTypeSelfActivate
	if successOutcome == OutcomeDeactivated {
		requestType = armclient.RequestTypeSelfDeactivate
	}
	switch {
	case armclient.IsSuccessStatusFor(requestType, status):
		res.Outcome = successOutcome
		if successOutcome == OutcomeActivated && sr.Properties.ScheduleInfo != nil {
			res.Since = activationStart(sr)
			res.Until = activationEnd(sr)
		}
	case armclient.IsPendingApprovalStatus(status):
		res.Outcome = OutcomePending
		res.Detail = "waiting for an approver"
	case armclient.IsFailureStatus(status):
		res.Outcome = OutcomeFailed
		res.Detail = "request status " + status
	case noWait:
		res.Outcome = OutcomeSubmitted
		res.Detail = "not polled (--no-wait); status " + status
	default:
		res.Outcome = OutcomeWaiting
		res.Detail = fmt.Sprintf("still %s after %s — the access is not held", status, pollTimeout)
	}
	return res
}

// activationStart is when ARM says the window opened.
//
// It matters because the alternative — the moment pimctl wrote the record — is
// a second or two later for a fresh activation and hours later for one that was
// already active. That would be pimctl's own clock dressed up as ARM's answer.
func activationStart(sr *armclient.ScheduleRequest) *time.Time {
	if si := sr.Properties.ScheduleInfo; si != nil {
		if start, err := time.Parse(time.RFC3339, si.StartDateTime); err == nil {
			return &start
		}
	}
	if sr.Properties.CreatedOn != nil {
		created := *sr.Properties.CreatedOn
		return &created
	}
	return nil
}

// activationEnd is when ARM says the window closes: the explicit end time when
// the response carried one, otherwise the start plus the ISO-8601 duration. It
// returns nil when the response says neither, so the caller prints "-" rather
// than a time pimctl invented.
func activationEnd(sr *armclient.ScheduleRequest) *time.Time {
	si := sr.Properties.ScheduleInfo
	if si == nil {
		return nil
	}
	start, err := time.Parse(time.RFC3339, si.StartDateTime)
	if err != nil {
		if sr.Properties.CreatedOn == nil {
			return nil
		}
		start = *sr.Properties.CreatedOn
	}
	if si.Expiration.EndDateTime != "" {
		if end, parseErr := time.Parse(time.RFC3339, si.Expiration.EndDateTime); parseErr == nil {
			return &end
		}
	}
	if si.Expiration.Duration == "" {
		return nil
	}
	d, err := armclient.ParseISODuration(si.Expiration.Duration)
	if err != nil {
		return nil
	}
	end := start.Add(d)
	return &end
}

// applyRequestError maps an ARM failure onto a result, including the
// already-active special case and the Conditional Access recovery command.
func applyRequestError(res result, s *session, err error, alreadyActiveUntil *time.Time) result {
	var ae *armclient.APIError
	if !errors.As(err, &ae) {
		res.Outcome = OutcomeFailed
		res.Detail = err.Error()
		return res
	}
	switch ae.Kind {
	case armclient.KindAlreadyActive:
		res.Outcome = OutcomeAlreadyActive
		res.Detail = ae.Code
		if res.Detail == "" {
			res.Detail = "already active"
		}
		// The existing window's end time was already fetched during listing,
		// so show it rather than making the user run `pimctl status` to find
		// out how much of it is left.
		res.Until = alreadyActiveUntil
	case armclient.KindPolicyValidation:
		// The cached policy is now known to disagree with ARM's, so drop it:
		// the next run re-reads rather than repeating the same rejection.
		cache.DropPolicy(res.Owner, res.Scope, res.RoleDefinitionID)
		res.Outcome = OutcomeFailed
		if len(ae.FailedRules) > 0 {
			res.Detail = fmt.Sprintf("%s: policy rules failed: %s", ae.Code, strings.Join(ae.FailedRules, ", "))
		} else {
			res.Detail = ae.Error()
		}
	case armclient.KindClaimsChallenge:
		res.Outcome = OutcomeFailed
		res.Detail = ae.Error()
		res.Recovery = ae.RecoveryCommand(s.Context, s.Token.TenantID)
	default:
		res.Outcome = OutcomeFailed
		res.Detail = ae.Error()
	}
	return res
}

// buildDeactivateBody assembles the SelfDeactivate request body. The REST
// contract requires only principalId, roleDefinitionId and requestType; a
// deactivation has no schedule and no eligibility to link, so neither is sent.
func buildDeactivateBody(principalID, roleDefinitionID string) armclient.RequestBody {
	return armclient.RequestBody{Properties: armclient.RequestProperties{
		PrincipalID:      principalID,
		RoleDefinitionID: roleDefinitionID,
		RequestType:      armclient.RequestTypeSelfDeactivate,
	}}
}

// deactivateOne submits one role's SelfDeactivate request and, unless noWait is
// set, polls it to a terminal status. Like [activateOne] it reports trouble as
// an outcome rather than an error, and ends in the same [classifyRequest], so a
// deactivation queued for approval cannot be misreported as a poll timeout.
//
// Two ARM errors get their own reading. How "no such assignment" reads depends
// on where the target came from: a role the listing showed active moments ago
// has probably not finished propagating and is still held, which is a failure;
// a speculatively named role is simply NOT ACTIVE, the end state that was
// asked for. The minimum-duration rejection is PIM's five-minute floor, worth
// saying in words rather than passing ARM's code through.
func deactivateOne(ctx context.Context, row target, noWait bool, pollTimeout time.Duration) result {
	res := result{
		Owner:            row.Session.owner(),
		Key:              selectionKeyFor(row.Context, row.Scope, armclient.RoleDefinitionGUID(row.RoleDefinitionID)),
		Context:          row.Context,
		Role:             row.RoleName,
		ScopeName:        row.ScopeName,
		ScopeType:        row.ScopeType,
		Scope:            row.Scope,
		RoleDefinitionID: row.RoleDefinitionID,
		RequestName:      uuid.NewString(),
	}
	if ctx.Err() != nil {
		res.Outcome = OutcomeSkipped
		res.Detail = "not attempted — the run was interrupted"
		return res
	}
	body := buildDeactivateBody(row.Session.Token.PrincipalID, row.RoleDefinitionID)
	sr, err := row.Session.Client.SubmitRequest(ctx, row.Scope, res.RequestName, body)
	if err != nil {
		if ctx.Err() != nil {
			res.Outcome = OutcomeAborted
			res.Detail = "interrupted while submitting — check `pimctl status`"
			return res
		}
		var ae *armclient.APIError
		if errors.As(err, &ae) {
			switch ae.Kind {
			case armclient.KindNotActive:
				// How this reads depends on where the target came from.
				//
				// If the activation listing showed the role active moments ago,
				// "no such assignment" means the assignment has not finished
				// propagating and the role is still held — a failure, because
				// telling the user they gave up access they still have is the
				// worst possible answer.
				//
				// If the user named the role speculatively (a preset, --role,
				// --key) then the listing was never the authority, and ARM has
				// just told us plainly that it is not active. Nothing to do.
				if row.SeenActive {
					res.Outcome = OutcomeFailed
					res.Detail = "ARM reports no such role assignment — the activation has probably not finished propagating (this lasts a minute or two after activating); check `pimctl status` and retry"
					return res
				}
				res.Outcome = OutcomeNotActive
				res.Detail = "not active"
				return res
			case armclient.KindMinimumDuration:
				res.Outcome = OutcomeFailed
				res.Detail = "PIM requires a role to stay active for at least 5 minutes before it can be deactivated; try again shortly"
				return res
			}
		}
		return applyRequestError(res, row.Session, err, nil)
	}
	res.RequestID = sr.ID
	res.Status = sr.Properties.Status
	if !noWait {
		polled, perr := row.Session.Client.Poll(ctx, sr, pollTimeout)
		if polled != nil {
			sr = polled
			res.Status = sr.Properties.Status
		}
		switch {
		case errors.Is(perr, context.Canceled), errors.Is(perr, context.DeadlineExceeded):
			res.Outcome = OutcomeAborted
			res.Detail = fmt.Sprintf("interrupted while waiting (last status %s) — check `pimctl status`", res.Status)
			return res
		case perr != nil:
			res.Outcome = OutcomeFailed
			res.Detail = perr.Error()
			return res
		}
	}
	// Shared with activate so the two cannot drift — in particular so a
	// deactivation queued for approval is reported as such rather than as a
	// poll timeout.
	return classifyRequest(res, sr, noWait, OutcomeDeactivated, pollTimeout)
}
