// The ARM documents pimctl exchanges and the request statuses it reads them
// for: the two kinds of schedule instance, the schedule request and the PUT
// body that creates one. The calls that fetch and submit them are in client.go;
// the id and duration helpers that pick them apart are in scope.go.

package armclient

import "time"

// APIVersion is the stable PIM api-version for Azure resource roles.
const APIVersion = "2020-10-01"

// DefaultHost is the ARM endpoint.
const DefaultHost = "https://management.azure.com"

// Named is the shape of expandedProperties.{principal,roleDefinition,scope}.
type Named struct {
	ID          string `json:"id"`          // the full ARM id of the thing named.
	DisplayName string `json:"displayName"` // human-readable, and not unique: three management groups here are called "Contoso landing zones".
	Type        string `json:"type"`        // what kind of thing it is, e.g. "managementgroup" or "Group".
}

// ExpandedProperties is the display-name bundle ARM attaches to schedule
// instances and requests so callers do not need extra lookups.
type ExpandedProperties struct {
	Principal      Named `json:"principal"`      // who holds it: the user, or the group the eligibility came through.
	RoleDefinition Named `json:"roleDefinition"` // the role, whose display name is what every table prints.
	Scope          Named `json:"scope"`          // where it applies, and the only source of a scope's display name.
}

// EligibilityProperties is properties{} on a roleEligibilityScheduleInstance.
type EligibilityProperties struct {
	Condition        string `json:"condition,omitempty"`        // ARM access constraint; preserved on activation.
	ConditionVersion string `json:"conditionVersion,omitempty"` // Syntax version of Condition.
	// Scope is where the eligibility applies. It is also the scope an
	// activation request must re-qualify RoleDefinitionID against.
	Scope string `json:"scope"`
	// RoleDefinitionID is qualified against the scope the eligibility was
	// granted at, which is not always the scope being activated.
	RoleDefinitionID string `json:"roleDefinitionId"`
	// RoleEligibilityScheduleID goes into an activation request verbatim as
	// linkedRoleEligibilityScheduleId; rewriting it makes ARM reject the
	// request.
	RoleEligibilityScheduleID string `json:"roleEligibilityScheduleId"`
	// PrincipalID is who the eligibility belongs to — the group, when it came
	// through one. The request still carries the signed-in user's own id.
	PrincipalID   string `json:"principalId"`
	PrincipalType string `json:"principalType"` // "User" or "Group".
	// MemberType is "Direct" or "Group", which is how a row can say it was
	// inherited rather than granted.
	MemberType    string     `json:"memberType"`
	Status        string     `json:"status"`        // ARM's provisioning status for the eligibility itself.
	StartDateTime *time.Time `json:"startDateTime"` // when the eligibility began; nil when open-ended.
	// EndDateTime is when the eligibility lapses, not when an activation of it
	// ends. Nil for a permanent eligibility.
	EndDateTime        *time.Time         `json:"endDateTime"`
	ExpandedProperties ExpandedProperties `json:"expandedProperties"` // display names, so no second lookup is needed.
}

// Eligibility is one roleEligibilityScheduleInstance.
type Eligibility struct {
	ID         string                `json:"id"`         // full ARM id of the instance.
	Name       string                `json:"name"`       // the instance's GUID, unique per eligibility.
	Type       string                `json:"type"`       // the ARM resource type, always the schedule-instance one here.
	Properties EligibilityProperties `json:"properties"` // everything pimctl actually reads.
}

// RoleName is the role's display name, falling back to the definition id.
func (e Eligibility) RoleName() string {
	if n := e.Properties.ExpandedProperties.RoleDefinition.DisplayName; n != "" {
		return n
	}
	return e.Properties.RoleDefinitionID
}

// ScopeName is the scope's display name, falling back to its id.
func (e Eligibility) ScopeName() string {
	if n := e.Properties.ExpandedProperties.Scope.DisplayName; n != "" {
		return n
	}
	return e.Properties.Scope
}

// ScopeType is ARM's scope type ("subscription", "managementgroup",
// "resourcegroup", "resource"), normalised for display.
func (e Eligibility) ScopeType() string {
	return NormalizeScopeType(e.Properties.ExpandedProperties.Scope.Type, e.Properties.Scope)
}

// RoleDefinitionGUID is the trailing GUID of the role definition id, which is
// the stable identity of a role across scopes.
func (e Eligibility) RoleDefinitionGUID() string {
	return RoleDefinitionGUID(e.Properties.RoleDefinitionID)
}

// AssignmentProperties is properties{} on a roleAssignmentScheduleInstance.
type AssignmentProperties struct {
	Scope                    string `json:"scope"`                    // where the assignment applies.
	RoleDefinitionID         string `json:"roleDefinitionId"`         // qualified against Scope.
	RoleAssignmentScheduleID string `json:"roleAssignmentScheduleId"` // the schedule this instance belongs to.
	// LinkedRoleEligibilityScheduleID is the eligibility this activation was
	// made from, and is empty for permanent RBAC.
	LinkedRoleEligibilityScheduleID string `json:"linkedRoleEligibilityScheduleId"`
	OriginRoleAssignmentID          string `json:"originRoleAssignmentId"` // the underlying RBAC assignment ARM created.
	PrincipalID                     string `json:"principalId"`            // whose access this is.
	PrincipalType                   string `json:"principalType"`          // "User" or "Group".
	MemberType                      string `json:"memberType"`             // "Direct" or "Group".
	// AssignmentType is "Assigned" for permanent RBAC and "Activated" for a
	// live PIM activation. Only the latter counts as an active role.
	AssignmentType string     `json:"assignmentType"`
	Status         string     `json:"status"`        // ARM's provisioning status for the assignment.
	StartDateTime  *time.Time `json:"startDateTime"` // when the window opened.
	// EndDateTime is when the activation expires, and is what `status` counts
	// down to. Nil for permanent RBAC, which never expires.
	EndDateTime        *time.Time         `json:"endDateTime"`
	ExpandedProperties ExpandedProperties `json:"expandedProperties"` // display names for the tables.
}

// Assignment is one roleAssignmentScheduleInstance.
type Assignment struct {
	// ID is the instance's full ARM id, and the identity pimctl dedupes on
	// when nested scopes return the same activation twice.
	ID         string               `json:"id"`
	Name       string               `json:"name"`       // the instance's GUID.
	Type       string               `json:"type"`       // the ARM resource type.
	Properties AssignmentProperties `json:"properties"` // everything pimctl actually reads.
}

// IsActivated reports whether this assignment is a live PIM activation rather
// than permanent RBAC.
func (a Assignment) IsActivated() bool { return a.Properties.AssignmentType == "Activated" }

// RoleName is the role's display name, falling back to the definition id.
func (a Assignment) RoleName() string {
	if n := a.Properties.ExpandedProperties.RoleDefinition.DisplayName; n != "" {
		return n
	}
	return a.Properties.RoleDefinitionID
}

// ScopeName is the scope's display name, falling back to its id.
func (a Assignment) ScopeName() string {
	if n := a.Properties.ExpandedProperties.Scope.DisplayName; n != "" {
		return n
	}
	return a.Properties.Scope
}

// ScopeType is ARM's normalised scope type.
func (a Assignment) ScopeType() string {
	return NormalizeScopeType(a.Properties.ExpandedProperties.Scope.Type, a.Properties.Scope)
}

// RoleDefinitionGUID is the trailing GUID of the role definition id.
func (a Assignment) RoleDefinitionGUID() string {
	return RoleDefinitionGUID(a.Properties.RoleDefinitionID)
}

// ScheduleInfo is the activation window on a schedule request.
type ScheduleInfo struct {
	// StartDateTime is RFC 3339, sent as "now" and echoed back by ARM as the
	// moment the window actually opened.
	StartDateTime string     `json:"startDateTime"`
	Expiration    Expiration `json:"expiration"` // how the window ends.
}

// Expiration says how an activation window ends: pimctl always sends the
// duration form, and ARM answers in whichever form it prefers, so both are
// decoded.
type Expiration struct {
	Type        string `json:"type"`                  // "AfterDuration" or "AfterDateTime".
	EndDateTime string `json:"endDateTime,omitempty"` // RFC 3339, set when Type is AfterDateTime.
	Duration    string `json:"duration,omitempty"`    // ISO-8601, e.g. PT8H, set when Type is AfterDuration.
}

// TicketInfo is the ticketing metadata some PIM policies demand.
type TicketInfo struct {
	TicketNumber string `json:"ticketNumber"` // from --ticket-number; ARM does not validate it.
	TicketSystem string `json:"ticketSystem"` // from --ticket-system, e.g. "Jira".
}

// RequestProperties is the properties{} body of a roleAssignmentScheduleRequest.
// Field order and omitempty rules match the 17 proven SelfActivate requests
// recovered from this tenant's request history.
type RequestProperties struct {
	Condition        string `json:"condition,omitempty"`        // Eligibility's access constraint, copied verbatim.
	ConditionVersion string `json:"conditionVersion,omitempty"` // Eligibility's condition syntax version.
	// PrincipalID is the signed-in user's own object id, even when the
	// eligibility is held by a group. Sending the group's id is rejected.
	PrincipalID string `json:"principalId"`
	// RoleDefinitionID must be re-qualified to the scope being activated, not
	// copied from the eligibility, which carries it under the grant's scope.
	RoleDefinitionID string `json:"roleDefinitionId"`
	RequestType      string `json:"requestType"` // SelfActivate or SelfDeactivate.
	// LinkedRoleEligibilityScheduleID is the eligibility's schedule id,
	// verbatim. It is mandatory for group-derived eligibility, which is the
	// normal case, and omitted on a deactivation.
	LinkedRoleEligibilityScheduleID string `json:"linkedRoleEligibilityScheduleId,omitempty"`
	Justification                   string `json:"justification,omitempty"` // lands in the PIM audit log.
	// ScheduleInfo carries the requested window, and is absent on a
	// deactivation, which ends the window it finds.
	ScheduleInfo *ScheduleInfo `json:"scheduleInfo,omitempty"`
	// TicketInfo is sent only when the role's policy enables the Ticketing
	// rule; sending it otherwise is harmless but meaningless.
	TicketInfo *TicketInfo `json:"ticketInfo,omitempty"`
}

// RequestBody is the full PUT payload.
type RequestBody struct {
	Properties RequestProperties `json:"properties"` // the whole request; ARM takes nothing outside it.
}

// ScheduleRequestProperties is properties{} on a schedule request response.
type ScheduleRequestProperties struct {
	Scope            string `json:"scope"`            // where the request was made.
	RoleDefinitionID string `json:"roleDefinitionId"` // as ARM stored it, qualified to Scope.
	PrincipalID      string `json:"principalId"`      // the requesting user.
	PrincipalType    string `json:"principalType"`    // "User" for everything pimctl sends.
	RequestType      string `json:"requestType"`      // echoes SelfActivate or SelfDeactivate.
	// Status is the field the whole exit-code contract hangs on: Provisioned
	// and Granted are success, PendingApproval is exit 2, and the failure
	// spellings are listed in failureStatuses.
	Status        string `json:"status"`
	ApprovalID    string `json:"approvalId"`    // set when the role's policy sent this to an approver.
	Justification string `json:"justification"` // as sent, and as the audit log will show it.
	// LinkedRoleEligibilityScheduleID and TargetRoleAssignmentScheduleID say
	// which eligibility this came from and which assignment it produced.
	LinkedRoleEligibilityScheduleID string `json:"linkedRoleEligibilityScheduleId"`
	// TargetRoleAssignmentScheduleID is the assignment schedule the request
	// produced, empty until it has taken effect.
	TargetRoleAssignmentScheduleID string             `json:"targetRoleAssignmentScheduleId"`
	CreatedOn                      *time.Time         `json:"createdOn"`          // when ARM accepted the request; the fallback for a window's start.
	ScheduleInfo                   *ScheduleInfo      `json:"scheduleInfo"`       // the window ARM granted, which may be shorter than the one asked for.
	TicketInfo                     *TicketInfo        `json:"ticketInfo"`         // echoed back when one was sent.
	ExpandedProperties             ExpandedProperties `json:"expandedProperties"` // display names.
}

// ScheduleRequest is a roleAssignmentScheduleRequest as returned by ARM.
type ScheduleRequest struct {
	// ID is the full ARM id, which pimctl stores in the activation record and
	// reads back to confirm an activation the per-scope listing has not caught
	// up with.
	ID         string                    `json:"id"`
	Name       string                    `json:"name"`       // the GUID pimctl generated for the PUT.
	Type       string                    `json:"type"`       // the ARM resource type.
	Properties ScheduleRequestProperties `json:"properties"` // the answer itself.
}

// The two requestType values pimctl sends. PIM defines administrator forms as
// well; pimctl only ever acts on the signed-in user's own access.
const (
	RequestTypeSelfActivate   = "SelfActivate"   // activate a role you are eligible for.
	RequestTypeSelfDeactivate = "SelfDeactivate" // give one up before it expires.
)

// The terminal statuses pimctl names in code. ARM has more spellings than
// these, which is why the predicates below consult maps rather than comparing
// against this list.
const (
	StatusProvisioned = "Provisioned" // the activation is in effect.
	StatusGranted     = "Granted"     // ARM's other spelling of the same outcome.
	StatusRevoked     = "Revoked"     // the good end of a deactivation, and a failed activation.
	StatusCanceled    = "Canceled"    // the request was withdrawn before it took effect.
)

// successStatuses are the good terminal states for an activation.
var successStatuses = map[string]bool{
	StatusProvisioned: true,
	StatusGranted:     true,
	// Revoked is the good outcome of a SelfDeactivate. It is accepted here too
	// because pimctl only ever polls a request it just created and knows the
	// type of; see IsSuccessStatusFor for the type-aware predicate.
	StatusRevoked: true,
}

// pendingApprovalStatuses are the statuses that mean the request is genuine but
// has not taken effect yet. They are why exit code 2 exists: a script must not
// read "waiting on an approver" as success.
var pendingApprovalStatuses = map[string]bool{
	"PendingApproval":             true,
	"PendingApprovalProvisioning": true,
	"PendingAdminDecision":        true,
}

// failureStatuses are the bad terminal states. Revoked is deliberately absent:
// it is the successful end state of a deactivation, not a failure.
var failureStatuses = map[string]bool{
	"Failed":                   true,
	"Denied":                   true,
	"AdminDenied":              true,
	"TimedOut":                 true,
	StatusCanceled:             true,
	"Invalid":                  true,
	"FailedAsResourceIsLocked": true,
}

// IsSuccessStatus reports whether the request reached a good terminal state.
func IsSuccessStatus(s string) bool { return successStatuses[s] }

// IsSuccessStatusFor is the request-type-aware form: an activation that somehow
// landed on Revoked has not granted anything, and must not be reported as
// activated.
func IsSuccessStatusFor(requestType, status string) bool {
	if status == StatusRevoked {
		return requestType == RequestTypeSelfDeactivate
	}
	return successStatuses[status]
}

// IsPendingApprovalStatus reports whether the request is waiting on an approver.
func IsPendingApprovalStatus(s string) bool { return pendingApprovalStatuses[s] }

// IsFailureStatus reports whether the request reached a bad terminal state.
func IsFailureStatus(s string) bool { return failureStatuses[s] }

// IsTerminalStatus reports whether polling can stop.
func IsTerminalStatus(s string) bool {
	return IsSuccessStatus(s) || IsPendingApprovalStatus(s) || IsFailureStatus(s)
}
