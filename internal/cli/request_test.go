// Tests for the ARM request bodies and the status classification, pinned
// against responses shaped like ARM's — including the three things ARM is strict
// about: principalId, the re-qualified roleDefinitionId, and the linked
// eligibility schedule id that must go through verbatim.

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
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

// TestClassifyRequestRevokedOrExpiredIsOver: an activation that ended Revoked,
// RevokedAndCanceled or Expired is over, not stalled — FAILED, worded as such,
// never "still Revoked after". Revoked stays the success of a deactivation.
func TestClassifyRequestRevokedOrExpiredIsOver(t *testing.T) {
	mk := func(status string) *armclient.ScheduleRequest {
		sr := &armclient.ScheduleRequest{}
		sr.Properties.Status = status
		return sr
	}
	for _, tc := range []struct{ status, want string }{
		{"Revoked", "revoked"},
		{"RevokedAndCanceled", "revoked"},
		{"Expired", "expired"},
	} {
		got := classifyRequest(result{}, mk(tc.status), false, OutcomeActivated, defaultTimeouts().poll)
		if got.Outcome != OutcomeFailed || !strings.Contains(got.Detail, tc.want) ||
			strings.Contains(got.Detail, "still ") {
			t.Errorf("%s activation -> %s / %q, want FAILED worded as %s", tc.status, got.Outcome, got.Detail, tc.want)
		}
		if !strings.Contains(got.Detail, tc.status) {
			t.Errorf("%s activation: ARM's own status is missing from %q", tc.status, got.Detail)
		}
	}
	got := classifyRequest(result{}, mk("Revoked"), false, OutcomeDeactivated, defaultTimeouts().poll)
	if got.Outcome != OutcomeDeactivated {
		t.Errorf("Revoked deactivation -> %s / %q, want DEACTIVATED", got.Outcome, got.Detail)
	}
}

// TestClassifyUnreadKeepsATerminalAnswer: when the poll ends on a read error,
// the last status ARM did return decides. A terminal one is classified as
// usual, so a good final status still reaches the record; a non-terminal one
// is STILL PENDING with the error in the detail, never FAILED.
func TestClassifyUnreadKeepsATerminalAnswer(t *testing.T) {
	sr := &armclient.ScheduleRequest{}
	sr.Properties.Status = "Provisioned"
	got := classifyUnread(result{Status: "Provisioned"}, sr, errors.New("boom"), OutcomeActivated, time.Minute)
	if got.Outcome != OutcomeActivated {
		t.Errorf("a provisioned request with a later read error -> %s / %q, want ACTIVATED", got.Outcome, got.Detail)
	}
	sr.Properties.Status = "Accepted"
	got = classifyUnread(result{Status: "Accepted"}, sr, errors.New("boom"), OutcomeActivated, time.Minute)
	if got.Outcome != OutcomeWaiting {
		t.Errorf("an unread request -> %s, want STILL PENDING", got.Outcome)
	}
	for _, want := range []string{"Accepted", "boom", "pimctl status"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("detail %q is missing %q", got.Detail, want)
		}
	}
	if exitCodeFor([]result{got}) != ExitFailed {
		t.Error("an unread request must exit 1 like a stalled one")
	}
}

// pollReadErrorSession is an ARM stand-in that accepts every request as
// PendingProvisioning and then refuses to read it back, which is the shape of
// a poll that ends on an error rather than a status.
func pollReadErrorSession(t *testing.T) *session {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusCreated)
			writeJSON(t, w, map[string]any{
				"id": r.URL.Path, "properties": map[string]any{"status": "PendingProvisioning"},
			})
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"code":"ReadBackFailed","message":"the read-back failed"}}`)
	}))
	t.Cleanup(srv.Close)
	tok := &azauth.Token{Context: "contoso", AccessToken: "t", TenantID: "tid-1", PrincipalID: "oid-1"}
	return &session{Context: "contoso", Token: tok, Client: armclient.New(srv.URL, tok.AccessToken, srv.Client())}
}

// assertUnreadOutcome checks a result for a request whose read-back failed.
func assertUnreadOutcome(t *testing.T, what string, got result) {
	t.Helper()
	if got.Outcome != OutcomeWaiting {
		t.Fatalf("%s whose read-back failed -> %s / %q, want STILL PENDING", what, got.Outcome, got.Detail)
	}
	if got.Status != "PendingProvisioning" {
		t.Errorf("%s: status = %q, want the last observed status", what, got.Status)
	}
	for _, want := range []string{"PendingProvisioning", "the read-back failed", "pimctl status"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("%s: detail %q is missing %q", what, got.Detail, want)
		}
	}
}

// TestPollReadErrorIsNotReportedAsFailed: ARM accepted the request, so a GET
// that then fails says nothing about whether the role was granted. The row must
// read like a timed-out poll — exit 1, last status, "check pimctl status" —
// not FAILED, which would claim the access is not held.
func TestPollReadErrorIsNotReportedAsFailed(t *testing.T) {
	s := pollReadErrorSession(t)
	item := &planItem{
		Row:         row{Context: "contoso", Elig: twoLowImpactRoles()[0]},
		Session:     s,
		Duration:    time.Hour,
		RequestName: "11111111-1111-1111-1111-111111111111",
	}
	assertUnreadOutcome(
		t,
		"an activation",
		activateOne(context.Background(), item, "why", "", "", false, 5*time.Second),
	)

	down := target{
		Session:          s,
		Context:          "contoso",
		Scope:            mgScope,
		RoleDefinitionID: "/providers/Microsoft.Authorization/roleDefinitions/" + costGUID,
		RoleName:         "Cost Management Contributor",
	}
	assertUnreadOutcome(t, "a deactivation", deactivateOne(context.Background(), down, false, 5*time.Second))
}

// TestSubmitReplacesARejectedCachedToken: a 401 on the PUT gets the same
// treatment as one on a listing. The cached token is dropped and replaced
// once, the request is sent again with the new one, and the stale entry never
// comes back.
func TestSubmitReplacesARejectedCachedToken(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	installFakeRunner(t, []string{"contoso"})
	azauth.WriteTokenCache(&azauth.Token{
		Context:     "contoso",
		AccessToken: "stale-token",
		ExpiresOn:   time.Now().Add(time.Hour).Format("2006-01-02 15:04:05.000000"),
		Tenant:      "tid-1",
		TenantID:    "tid-1",
		PrincipalID: "oid-1",
	}, azauth.DefaultRunner)
	tok := readTokenCache(t, "contoso")
	if tok == nil || !tok.FromCache {
		t.Fatal("the stale token should have been readable from the cache")
	}

	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		seen = append(seen, auth)
		mu.Unlock()
		if auth == "stale-token" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":{"code":"ExpiredAuthenticationToken","message":"rejected"}}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
		writeJSON(t, w, map[string]any{"id": r.URL.Path, "properties": map[string]any{"status": "Provisioned"}})
	}))
	t.Cleanup(srv.Close)

	s := &session{Context: "contoso", Token: tok, Client: armclient.New(srv.URL, tok.AccessToken, srv.Client())}
	item := &planItem{
		Row:         row{Context: "contoso", Elig: twoLowImpactRoles()[0]},
		Session:     s,
		Duration:    time.Hour,
		RequestName: "22222222-2222-2222-2222-222222222222",
	}
	got := activateOne(context.Background(), item, "why", "", "", false, 5*time.Second)
	if got.Outcome != OutcomeActivated {
		t.Fatalf("a 401 on a cached token should self-heal, got %s / %q", got.Outcome, got.Detail)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != "stale-token" || seen[1] == "stale-token" {
		t.Fatalf("expected the stale token once and a fresh one once, saw %q", seen)
	}
	if c := readTokenCache(t, "contoso"); c != nil && c.AccessToken == "stale-token" {
		t.Error("the rejected token is still cached")
	}
}

// TestApplyRequestErrorRequestExists: an open request is not a held role —
// FAILED, exit 1, and the user is pointed at status rather than told the role
// is active. The existing window's end time is not borrowed either, because
// there is no window.
func TestApplyRequestErrorRequestExists(t *testing.T) {
	sess := &session{Context: "contoso", Token: &azauth.Token{Context: "contoso", TenantID: "tid-1"}}
	for _, code := range []string{"RoleAssignmentRequestExists", "RoleAssignmentScheduleRequestExists"} {
		until := time.Now().Add(time.Hour)
		ae := armclient.ParseAPIError("PUT", "/x", 400,
			[]byte(`{"error":{"code":"`+code+`","message":"A request already exists."}}`), nil)
		res := applyRequestError(result{Role: "Contributor"}, sess, ae, &until)
		if res.Outcome != OutcomeFailed || !res.IsFailure() {
			t.Errorf("%s: outcome = %s, want FAILED", code, res.Outcome)
		}
		for _, want := range []string{"request exists", code, "not held", "pimctl status", "pimctl help"} {
			if !strings.Contains(res.Detail, want) {
				t.Errorf("%s: detail %q is missing %q", code, res.Detail, want)
			}
		}
		if res.Until != nil {
			t.Errorf("%s: an open request must not carry a window end", code)
		}
	}
}

// TestApplyRequestErrorClaimsChallenge: a claims challenge produces the exact
// recovery command, points at --refresh, and deletes the cached token that can
// never satisfy it.
func TestApplyRequestErrorClaimsChallenge(t *testing.T) {
	// Deleting the token resolves the context's store: never against a real
	// cloudctx.
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	installFakeRunner(t, []string{"contoso"})
	sess := &session{
		Context: "contoso",
		Token:   &azauth.Token{Context: "contoso", TenantID: "11111111-2222-3333-4444-555555555555"},
	}
	azauth.WriteTokenCache(&azauth.Token{
		Context:     "contoso",
		AccessToken: "cached",
		ExpiresOn:   time.Now().Add(time.Hour).Format("2006-01-02 15:04:05.000000"),
		Tenant:      "tid-1",
		TenantID:    "tid-1",
		PrincipalID: "oid-1",
	}, azauth.DefaultRunner)
	cachePath, err := azauth.TokenCachePath("contoso", azauth.DefaultRunner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("the token cache was not written: %v", err)
	}

	claims := `{"access_token":{"acrs":{"essential":true,"value":"c1"}}}`
	ae := armclient.ParseAPIError("PUT", "/x", 403,
		[]byte(`{"error":{"code":"RoleAssignmentRequestAcrsValidationFailed","message":"claims=`+
			strings.ReplaceAll(claims, `"`, `\"`)+`"}}`), nil)
	res := applyRequestError(result{}, sess, ae, nil)
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
	if !strings.Contains(res.Detail, "--refresh") {
		t.Errorf("the claims-challenge detail should point at --refresh: %q", res.Detail)
	}
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Errorf("the cached token must be gone after a claims challenge; stat: %v", err)
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

	// Anything else is passed through verbatim.
	ae = armclient.ParseAPIError("PUT", "/x", 409,
		[]byte(`{"error":{"code":"Conflict","message":"Something specific went wrong."}}`), nil)
	res = applyRequestError(result{}, sess, ae, nil)
	if res.Detail != "Conflict: Something specific went wrong." {
		t.Errorf("verbatim passthrough failed: %q", res.Detail)
	}
}

func TestPreservedActivationExpiryIsRecheckedAtExecution(t *testing.T) {
	expired := time.Now().Add(-time.Second)
	s := &session{Token: &azauth.Token{Context: "contoso", TenantID: "tid", PrincipalID: "oid"}}
	item := &planItem{
		KeepActive: true,
		Session:    s,
		Row: row{
			Context: "contoso",
			Active:  &armclient.Assignment{Properties: armclient.AssignmentProperties{EndDateTime: &expired}},
		},
	}
	got := activateOne(context.Background(), item, "", "", "", false, time.Second)
	if got.Outcome != OutcomeFailed || !strings.Contains(got.Detail, "expired") ||
		!strings.Contains(got.Detail, "run pimctl up again") {
		t.Fatalf("expired window reported as held: %#v", got)
	}
}

// TestActivationWindowFromScheduleRequest pins where the printed window comes
// from: ARM's own start and end, never pimctl's clock, and nil rather than an
// invented time when ARM said nothing.
func TestActivationWindowFromScheduleRequest(t *testing.T) {
	at := func(h, m int) *time.Time {
		v := time.Date(2026, 9, 4, h, m, 0, 0, time.UTC)
		return &v
	}
	mk := func(start string, exp armclient.Expiration, createdOn *time.Time) *armclient.ScheduleRequest {
		sr := &armclient.ScheduleRequest{}
		sr.Properties.CreatedOn = createdOn
		sr.Properties.ScheduleInfo = &armclient.ScheduleInfo{StartDateTime: start, Expiration: exp}
		return sr
	}
	same := func(got, want *time.Time) bool {
		if got == nil || want == nil {
			return got == want
		}
		return got.Equal(*want)
	}
	for _, tc := range []struct {
		name               string
		sr                 *armclient.ScheduleRequest
		wantStart, wantEnd *time.Time
	}{
		{
			name: "endDateTime beats duration",
			sr: mk("2026-09-04T12:00:00Z", armclient.Expiration{
				Type: "AfterDateTime", EndDateTime: "2026-09-04T12:30:00Z", Duration: "PT4H",
			}, nil),
			wantStart: at(12, 0), wantEnd: at(12, 30),
		},
		{
			name:      "duration is added to the start when endDateTime is absent",
			sr:        mk("2026-09-04T12:00:00Z", armclient.Expiration{Type: "AfterDuration", Duration: "PT4H"}, nil),
			wantStart: at(12, 0), wantEnd: at(16, 0),
		},
		{
			name:      "createdOn stands in for an unparsable start",
			sr:        mk("not a time", armclient.Expiration{Type: "AfterDuration", Duration: "PT1H"}, at(11, 59)),
			wantStart: at(11, 59), wantEnd: at(12, 59),
		},
		{
			name: "nil when ARM says neither",
			sr:   &armclient.ScheduleRequest{},
		},
		{
			name:      "nil end when the window has no expiration",
			sr:        mk("2026-09-04T12:00:00Z", armclient.Expiration{}, nil),
			wantStart: at(12, 0),
		},
		{
			name: "nil when the start is unparsable and nothing was created",
			sr:   mk("", armclient.Expiration{Type: "AfterDuration", Duration: "PT1H"}, nil),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := activationStart(tc.sr); !same(got, tc.wantStart) {
				t.Errorf("activationStart = %v, want %v", got, tc.wantStart)
			}
			if got := activationEnd(tc.sr); !same(got, tc.wantEnd) {
				t.Errorf("activationEnd = %v, want %v", got, tc.wantEnd)
			}
		})
	}
}

// TestActivationFollowsTheRequestToItsTerminalStatus: ARM lands a PUT below a
// terminal status and settles it on a later read-back. The CLI keeps asking
// until it does, reports the role ACTIVATED, and exits 0 — the same request
// read once would have been STILL PENDING and exit 1.
func TestActivationFollowsTheRequestToItsTerminalStatus(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()[:1], putStatus: "PendingProvisioning"}
	f.install()
	f.setGetStatuses("PendingProvisioning", "Provisioned")

	out, errOut, err := runCmd(t, "up", "-c", "contoso", "--all", "-j", "x", "-y")
	if err != nil {
		t.Fatalf("up exited %d: %v\n%s\n%s", ExitCode(err), err, out, errOut)
	}
	if n := f.requestGetCount(); n < 2 {
		t.Errorf("the request was read back %d time(s); the second read is the one that saw Provisioned", n)
	}
	if !strings.Contains(out, "ACTIVATED") || strings.Contains(out, "STILL PENDING") {
		t.Errorf("a request that settled must be reported as activated:\n%s", out)
	}
}
