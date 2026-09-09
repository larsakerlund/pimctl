// Package armclient is the whole of pimctl's ARM REST surface. Every call the
// tool makes to Azure Resource Manager goes out through [Client], every shape
// it sends or receives is declared here, and every non-2xx answer is
// classified here. Nothing above this package speaks HTTP to Azure, so the ARM
// behaviours that are easy to get wrong are got right in one place.
//
// The structs here mirror ARM's own responses, kept as fixtures under testdata
// and replayed by the tests; the field names are the wire's, not invented.
//
// The main types are [Client] and the documents it exchanges: [Eligibility]
// and [Assignment] (the roleEligibility and roleAssignment schedule
// instances), [ScheduleRequest] (a roleAssignmentScheduleRequest),
// [RoleSettings] (the activation-relevant subset of a role management policy)
// and [RequestBody], the PUT payload internal/cli fills in.
//
// # The api-version is pinned to 2020-10-01
//
// [APIVersion] goes on every URL built here: the eligibility and assignment
// schedule instances, the schedule requests, and the role management policy
// assignments and the policies they point at. It must not drift. The types
// above are transcribed from responses at that version and the tests replay
// those fixtures, so changing the constant would not change the code that reads
// what comes back; a newer version has to be proven against ARM, fixtures and
// all, before it can be pinned instead.
//
// # The activation request template
//
// A SelfActivate body is assembled in internal/cli and submitted by
// [Client.SubmitRequest]. Three of its fields are counter-intuitive, and all
// three are settled by this tenant's own successful requests:
//
//   - principalId is the signed-in user's own object id — the oid claim of the
//     ARM token — even when the eligibility is inherited through a group.
//     Group-derived eligibility is the normal case, not the exception, and the
//     group's id never goes in this field.
//   - roleDefinitionId must be re-qualified to the scope being activated at.
//     The eligibility carries the definition id under the scope it was granted
//     at, which is not necessarily the same one; [QualifyRoleDefinitionID] does
//     the rewrite.
//   - linkedRoleEligibilityScheduleId goes through verbatim, exactly as the
//     eligibility reported it. Rewriting it makes ARM reject the request.
//
// # Failures are classified, not merely returned
//
// errors.go turns every non-2xx response into an [APIError] carrying an
// [ErrorKind], so a caller can tell "you already hold this role" from "the
// policy refused you" from "Conditional Access wants a stepped-up token" — and,
// for the last of those, hand the operator the exact login command that fixes
// it, via [APIError.RecoveryCommand]. The same classification decides what is
// worth trying again: only throttling and transient gateway failures are
// retried, up to [Client.MaxRetries] times and honouring ARM's Retry-After header.
// Everything else is returned on the first attempt.
//
// # The tenant-wide activation listing is lossy
//
// [Client.ListAssignments] asks ARM for every assignment schedule instance in
// the tenant with $filter=asTarget(). It is slow, and it silently drops rows:
// three runs against an unchanged tenant returned 126, 131 and 132 instances,
// none of them paged. Callers therefore fan out over
// [Client.ListAssignmentsAtScope], one scope at a time, and pay the extra calls
// for an answer that is complete. Do not make the tenant-wide call
// authoritative again.
package armclient
