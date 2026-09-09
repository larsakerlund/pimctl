// Tests for the ARM request bodies and the status classification, pinned
// against responses shaped like ARM's — including the three things ARM is strict
// about: principalId, the re-qualified roleDefinitionId, and the linked
// eligibility schedule id that must go through verbatim.

package cli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/larsakerlund/pimctl/internal/armclient"
	"github.com/larsakerlund/pimctl/internal/azauth"
)

// TestBuildActivateBodyMatchesProvenTemplate compares the generated request
// body field-for-field against the SelfActivate body ARM accepts.
// assertScheduleInfoMatches checks the window pimctl asks for against the one
// the fixture request carries.
func assertScheduleInfoMatches(t *testing.T, got, want *armclient.ScheduleInfo) {
	t.Helper()
	if got == nil {
		t.Fatal("scheduleInfo missing")
	}
	if got.StartDateTime != "2026-08-10T09:27:45Z" {
		t.Errorf("startDateTime = %q, want RFC3339 UTC", got.StartDateTime)
	}
	if got.Expiration.Type != want.Expiration.Type {
		t.Errorf("expiration.type = %q", got.Expiration.Type)
	}
	if got.Expiration.Duration != want.Expiration.Duration {
		t.Errorf("expiration.duration = %q, want %q", got.Expiration.Duration, want.Expiration.Duration)
	}
}

func TestBuildActivateBodyMatchesProvenTemplate(t *testing.T) {
	golden, err := os.ReadFile("../armclient/testdata/request_provisioned.json")
	if err != nil {
		t.Fatal(err)
	}
	var proven armclient.ScheduleRequest
	if err := json.Unmarshal(golden, &proven); err != nil {
		t.Fatal(err)
	}

	// Rebuild the eligibility the proven request was activated from. It lived at
	// the management group while the activation was requested at a subscription.
	e := armclient.Eligibility{}
	e.Properties.Scope = proven.Properties.Scope
	e.Properties.RoleDefinitionID = proven.Properties.RoleDefinitionID
	e.Properties.RoleEligibilityScheduleID = proven.Properties.LinkedRoleEligibilityScheduleID
	e.Properties.MemberType = "Group"

	item := &planItem{
		Row:      row{Context: "contoso", Elig: e},
		Duration: 4 * time.Hour,
		Settings: &armclient.RoleSettings{
			MaximumDuration:    4 * time.Hour,
			MaximumDurationISO: "PT4H",
			EnabledRules:       []string{"MultiFactorAuthentication", "Justification"},
		},
	}
	now := time.Date(2026, 8, 10, 9, 27, 45, 0, time.UTC)
	got := buildActivateBody(item, proven.Properties.PrincipalID, proven.Properties.Justification, "", "", now)
	p := got.Properties

	if p.PrincipalID != proven.Properties.PrincipalID {
		t.Errorf("principalId = %q, want the signed-in user %q", p.PrincipalID, proven.Properties.PrincipalID)
	}
	if p.RoleDefinitionID != proven.Properties.RoleDefinitionID {
		t.Errorf("roleDefinitionId =\n got  %q\n want %q", p.RoleDefinitionID, proven.Properties.RoleDefinitionID)
	}
	if p.RequestType != "SelfActivate" {
		t.Errorf("requestType = %q", p.RequestType)
	}
	if p.LinkedRoleEligibilityScheduleID != proven.Properties.LinkedRoleEligibilityScheduleID {
		t.Errorf("linkedRoleEligibilityScheduleId =\n got  %q\n want %q",
			p.LinkedRoleEligibilityScheduleID, proven.Properties.LinkedRoleEligibilityScheduleID)
	}
	if p.Justification != proven.Properties.Justification {
		t.Errorf("justification = %q, want %q", p.Justification, proven.Properties.Justification)
	}
	assertScheduleInfoMatches(t, p.ScheduleInfo, proven.Properties.ScheduleInfo)
	if p.TicketInfo != nil {
		t.Error("ticketInfo must be omitted when the policy has no Ticketing rule")
	}

	// The serialised body must carry exactly the keys the template used, and
	// no extras, because ARM rejects unknown properties on some scopes.
	envelope := roundTripJSON(t, got)
	wantKeys := map[string]bool{
		"principalId": true, "roleDefinitionId": true, "requestType": true,
		"linkedRoleEligibilityScheduleId": true, "justification": true, "scheduleInfo": true,
	}
	for k := range envelope["properties"] {
		if !wantKeys[k] {
			t.Errorf("unexpected property %q in the request body", k)
		}
		delete(wantKeys, k)
	}
	for k := range wantKeys {
		t.Errorf("missing property %q from the request body", k)
	}
}

func TestBuildActivateBodySendsTicketInfoOnlyWhenRequired(t *testing.T) {
	e := armclient.Eligibility{}
	e.Properties.Scope = subScope
	e.Properties.RoleDefinitionID = subScope + "/providers/Microsoft.Authorization/roleDefinitions/" + contribGUID
	e.Properties.RoleEligibilityScheduleID = subScope + "/providers/Microsoft.Authorization/roleEligibilitySchedules/x"
	item := &planItem{
		Row:      row{Context: "contoso", Elig: e},
		Duration: time.Hour,
		Settings: &armclient.RoleSettings{EnabledRules: []string{"MultiFactorAuthentication", "Ticketing"}},
	}
	body := buildActivateBody(item, "oid", "why", "INC-42", "ServiceNow", time.Now())
	if body.Properties.TicketInfo == nil {
		t.Fatal("ticketInfo must be sent when the Ticketing rule is enabled")
	}
	if body.Properties.TicketInfo.TicketNumber != "INC-42" || body.Properties.TicketInfo.TicketSystem != "ServiceNow" {
		t.Errorf("ticketInfo = %+v", *body.Properties.TicketInfo)
	}
}

// TestBuildActivateBodyUsesUserOIDForGroupEligibility pins the single most
// error-prone rule: the principal is the signed-in user even though the
// eligibility itself belongs to a group.
func TestBuildActivateBodyUsesUserOIDForGroupEligibility(t *testing.T) {
	e := armclient.Eligibility{}
	e.Properties.Scope = mgScope
	e.Properties.RoleDefinitionID = "/providers/Microsoft.Authorization/roleDefinitions/" + costGUID
	e.Properties.RoleEligibilityScheduleID = mgScope + "/providers/Microsoft.Authorization/roleEligibilitySchedules/abc"
	e.Properties.MemberType = "Group"
	e.Properties.PrincipalID = "bbbbbbbb-cccc-dddd-eeee-ffffffffffff" // the group.
	e.Properties.PrincipalType = "Group"

	userOID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	body := buildActivateBody(&planItem{Row: row{Elig: e}, Duration: time.Hour}, userOID, "j", "", "", time.Now())
	if body.Properties.PrincipalID != userOID {
		t.Fatalf("principalId = %q, want the user's oid %q — the group's id must never be sent",
			body.Properties.PrincipalID, userOID)
	}
	if body.Properties.LinkedRoleEligibilityScheduleID != e.Properties.RoleEligibilityScheduleID {
		t.Fatalf("linkedRoleEligibilityScheduleId must be the eligibility's own schedule id verbatim")
	}
}

func TestBuildDeactivateBody(t *testing.T) {
	roleDef := subScope + "/providers/Microsoft.Authorization/roleDefinitions/" + contribGUID
	body := buildDeactivateBody("oid-1", roleDef)
	if body.Properties.RequestType != "SelfDeactivate" {
		t.Errorf("requestType = %q", body.Properties.RequestType)
	}
	props := roundTripJSON(t, body)["properties"]
	if len(props) != 3 {
		t.Errorf("deactivation body should carry exactly principalId, roleDefinitionId and requestType; got %v", props)
	}
	for _, k := range []string{"scheduleInfo", "linkedRoleEligibilityScheduleId", "justification", "ticketInfo"} {
		if _, ok := props[k]; ok {
			t.Errorf("%s must not be sent on a deactivation", k)
		}
	}
}

func TestClassifyRequest(t *testing.T) {
	mk := func(status, duration string) *armclient.ScheduleRequest {
		sr := &armclient.ScheduleRequest{}
		sr.Properties.Status = status
		sr.Properties.ScheduleInfo = &armclient.ScheduleInfo{
			StartDateTime: "2026-09-04T12:00:00Z",
			Expiration:    armclient.Expiration{Type: "AfterDuration", Duration: duration},
		}
		return sr
	}
	res := classifyRequest(result{}, mk("Provisioned", "PT1H"), false, OutcomeActivated, defaultTimeouts().poll)
	if res.Outcome != OutcomeActivated {
		t.Errorf("Provisioned -> %s", res.Outcome)
	}
	if res.Until == nil || !res.Until.Equal(time.Date(2026, 9, 4, 13, 0, 0, 0, time.UTC)) {
		t.Errorf("end time = %v, want start+PT1H", res.Until)
	}

	if got := classifyRequest(
		result{},
		mk("Granted", "PT4H"),
		false,
		OutcomeActivated,
		defaultTimeouts().poll,
	); got.Outcome != OutcomeActivated {
		t.Errorf("Granted -> %s", got.Outcome)
	}
	if got := classifyRequest(
		result{},
		mk("PendingApproval", "PT1H"),
		false,
		OutcomeActivated,
		defaultTimeouts().poll,
	); got.Outcome != OutcomePending {
		t.Errorf("PendingApproval -> %s", got.Outcome)
	}
	if got := classifyRequest(
		result{},
		mk("PendingAdminDecision", "PT1H"),
		false,
		OutcomeActivated,
		defaultTimeouts().poll,
	); got.Outcome != OutcomePending {
		t.Errorf("PendingAdminDecision -> %s", got.Outcome)
	}
	got := classifyRequest(result{}, mk("Failed", "PT1H"), false, OutcomeActivated, defaultTimeouts().poll)
	if got.Outcome != OutcomeFailed || !strings.Contains(got.Detail, "Failed") {
		t.Errorf("Failed -> %s / %q", got.Outcome, got.Detail)
	}
	got = classifyRequest(result{}, mk("Accepted", "PT1H"), true, OutcomeActivated, defaultTimeouts().poll)
	if got.Outcome != OutcomeSubmitted {
		t.Errorf("--no-wait on a non-terminal status -> %s", got.Outcome)
	}
	got = classifyRequest(result{}, mk("PendingProvisioning", "PT1H"), false, OutcomeActivated, defaultTimeouts().poll)
	if got.Outcome != OutcomeWaiting || !strings.Contains(got.Detail, "still PendingProvisioning") {
		t.Errorf("timed-out poll -> %s / %q", got.Outcome, got.Detail)
	}
}

func TestApplyRequestErrorMapping(t *testing.T) {
	sess := &session{
		Context: "contoso",
		Token:   &azauth.Token{Context: "contoso", TenantID: "11111111-2222-3333-4444-555555555555"},
	}

	// Already active is an outcome, not a failure — it must not fail the run.
	ae := armclient.ParseAPIError("PUT", "/x", 400,
		[]byte(`{"error":{"code":"RoleAssignmentExists","message":"The Role assignment already exists."}}`), nil)
	res := applyRequestError(result{Role: "Contributor"}, sess, ae, nil)
	if res.Outcome != OutcomeAlreadyActive {
		t.Fatalf("outcome = %s, want ALREADY ACTIVE", res.Outcome)
	}
	if res.IsFailure() {
		t.Error("ALREADY ACTIVE must not count as a failure")
	}

	// Policy validation surfaces the failing rule names.
	ae = armclient.ParseAPIError(
		"PUT",
		"/x",
		400,
		[]byte(
			`{"error":{"code":"RoleAssignmentRequestPolicyValidationFailed","message":"The following policy rules failed: [\"MfaRule\",\"ExpirationRule\"]"}}`,
		),
		nil,
	)
	res = applyRequestError(result{}, sess, ae, nil)
	if res.Outcome != OutcomeFailed {
		t.Fatalf("outcome = %s", res.Outcome)
	}
	if !strings.Contains(res.Detail, "MfaRule") || !strings.Contains(res.Detail, "ExpirationRule") {
		t.Errorf("failing rules not surfaced: %q", res.Detail)
	}

	// A claims challenge produces the exact recovery command.
	claims := `{"access_token":{"acrs":{"essential":true,"value":"c1"}}}`
	ae = armclient.ParseAPIError("PUT", "/x", 403,
		[]byte(`{"error":{"code":"RoleAssignmentRequestAcrsValidationFailed","message":"claims=`+
			strings.ReplaceAll(claims, `"`, `\"`)+`"}}`), nil)
	res = applyRequestError(result{}, sess, ae, nil)
	if res.Outcome != OutcomeFailed {
		t.Fatalf("outcome = %s", res.Outcome)
	}
	if !strings.HasPrefix(
		res.Recovery,
		"cloudctx exec contoso -- az login --tenant 11111111-2222-3333-4444-555555555555",
	) {
		t.Errorf("recovery command = %q", res.Recovery)
	}
	if !strings.Contains(res.Recovery, "--claims-challenge") {
		t.Errorf("recovery command has no claims challenge: %q", res.Recovery)
	}

	// Anything else is passed through verbatim.
	ae = armclient.ParseAPIError("PUT", "/x", 409,
		[]byte(`{"error":{"code":"Conflict","message":"Something specific went wrong."}}`), nil)
	res = applyRequestError(result{}, sess, ae, nil)
	if res.Detail != "Conflict: Something specific went wrong." {
		t.Errorf("verbatim passthrough failed: %q", res.Detail)
	}
}
