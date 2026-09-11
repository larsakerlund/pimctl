// Covers client.go against the ARM-shaped responses in testdata: nextLink
// paging, policy parsing, the proven SelfActivate body shape, the retry and
// backoff rules, and the poll loop. It also covers the status predicates in
// types.go, which have no test file of their own. The error classification
// those tests lean on is tested in errors_test.go.

package armclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name) //nolint:gosec // a fixture name from this file, not input
	if err != nil {
		t.Fatalf("reading testdata/%s: %v", name, err)
	}
	return b
}

// respond writes a stub response body, reporting a failed write rather than
// discarding it. It is called from the fake server's goroutine, so it uses
// Errorf: only the test's own goroutine may call Fatal.
func respond(t *testing.T, w http.ResponseWriter, b []byte) {
	t.Helper()
	if _, err := w.Write(b); err != nil {
		t.Errorf("writing the stub response: %v", err)
	}
}

// marshalStub renders a stub response body from the fake server's goroutine.
func marshalStub(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Errorf("marshalling the stub response: %v", err)
		return nil
	}
	return b
}

// mustObject reads m[key] as a JSON object, failing the test if it is anything
// else. Called only from a test's own goroutine.
func mustObject(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	v, ok := m[key].(map[string]any)
	if !ok {
		t.Fatalf("%s is not a JSON object: %#v", key, m[key])
	}
	return v
}

// newTestClient wires a Client to a fake ARM. No test touches the network.
func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(srv.URL, "fake-token-not-a-real-credential", srv.Client())
}

// assertListingQuery checks the query string every eligibility listing must
// carry, including the one built from a nextLink.
func assertListingQuery(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.URL.Query().Get("api-version"); got != APIVersion {
		t.Errorf("api-version = %q", got)
	}
	if f := r.URL.Query().Get("$filter"); f != "asTarget()" {
		t.Errorf("$filter = %q, want asTarget()", f)
	}
}

func TestListEligibilitiesFollowsNextLink(t *testing.T) {
	var page atomic.Int32
	var srvURL string
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		assertListingQuery(t, r)
		w.Header().Set("Content-Type", "application/json")
		if page.Add(1) == 1 {
			// First page carries one item and a nextLink.
			var doc struct {
				Value    []json.RawMessage `json:"value"`
				NextLink string            `json:"nextLink"`
			}
			if err := json.Unmarshal(readTestdata(t, "eligibilities.json"), &doc); err != nil {
				t.Fatal(err)
			}
			respond(t, w, marshalStub(t, map[string]any{
				"value":    doc.Value[:1],
				"nextLink": srvURL + "/page2?api-version=" + APIVersion + "&$filter=asTarget()",
			}))
			return
		}
		var doc struct {
			Value []json.RawMessage `json:"value"`
		}
		if err := json.Unmarshal(readTestdata(t, "eligibilities.json"), &doc); err != nil {
			t.Errorf("decoding the eligibilities fixture: %v", err)
			return
		}
		respond(t, w, marshalStub(t, map[string]any{"value": doc.Value[1:]}))
	}))
	defer srv.Close()
	srvURL = srv.URL

	c := New(srv.URL, "fake-token", srv.Client())
	got, err := c.ListEligibilities(context.Background())
	if err != nil {
		t.Fatalf("ListEligibilities: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d eligibilities across 2 pages, want 2", len(got))
	}
	if page.Load() != 2 {
		t.Errorf("fetched %d pages, want 2 (nextLink not followed?)", page.Load())
	}
	if gotAuth != "Bearer fake-token" {
		t.Errorf("Authorization header = %q", gotAuth)
	}

	// Field mapping against the real capture.
	sub := got[0]
	if sub.RoleName() != "Contributor" {
		t.Errorf("RoleName = %q", sub.RoleName())
	}
	if sub.ScopeName() != "Contoso Identity" {
		t.Errorf("ScopeName = %q", sub.ScopeName())
	}
	if sub.ScopeType() != "Subscription" {
		t.Errorf("ScopeType = %q", sub.ScopeType())
	}
	if sub.Properties.MemberType != "Group" {
		t.Errorf("MemberType = %q", sub.Properties.MemberType)
	}
	if !strings.Contains(sub.Properties.RoleEligibilityScheduleID, "/roleEligibilitySchedules/") {
		t.Errorf("roleEligibilityScheduleId = %q", sub.Properties.RoleEligibilityScheduleID)
	}
	if got[1].ScopeType() != "ManagementGroup" {
		t.Errorf("second item ScopeType = %q", got[1].ScopeType())
	}
}

func TestListActivatedFiltersAssignmentType(t *testing.T) {
	body := `{"value":[
      {"id":"/subscriptions/s/providers/Microsoft.Authorization/roleAssignmentScheduleInstances/1",
       "properties":{"assignmentType":"Assigned","scope":"/subscriptions/s","status":"Provisioned",
        "roleDefinitionId":"/providers/Microsoft.Authorization/roleDefinitions/aaa",
        "expandedProperties":{"roleDefinition":{"displayName":"Billing Reader"},"scope":{"displayName":"Sub","type":"subscription"}}}},
      {"id":"/subscriptions/s/providers/Microsoft.Authorization/roleAssignmentScheduleInstances/2",
       "properties":{"assignmentType":"Activated","scope":"/subscriptions/s","status":"Provisioned",
        "endDateTime":"2026-09-04T15:00:00Z",
        "roleDefinitionId":"/providers/Microsoft.Authorization/roleDefinitions/bbb",
        "expandedProperties":{"roleDefinition":{"displayName":"Cost Management Contributor"},"scope":{"displayName":"Sub","type":"subscription"}}}}]}`
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, body) })
	got, err := c.ListActivated(context.Background())
	if err != nil {
		t.Fatalf("ListActivated: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d activated, want 1 (permanent 'Assigned' entries must not count)", len(got))
	}
	if got[0].RoleName() != "Cost Management Contributor" {
		t.Errorf("RoleName = %q", got[0].RoleName())
	}
	if got[0].Properties.EndDateTime == nil {
		t.Fatal("endDateTime not parsed")
	}
}

// assertPolicyFixture checks the parsed policy against the response in
// testdata/policy.json, field by field.
func assertPolicyFixture(t *testing.T, s *RoleSettings) {
	t.Helper()
	if s.MaximumDurationISO != "PT4H" || s.MaximumDuration != 4*time.Hour {
		t.Errorf("maximumDuration = %q / %v", s.MaximumDurationISO, s.MaximumDuration)
	}
	if !s.RequiresMFA() || !s.RequiresJustification() {
		t.Errorf("enabledRules = %v", s.EnabledRules)
	}
	if s.RequiresTicket() {
		t.Errorf("Ticketing must not be reported for this policy: %v", s.EnabledRules)
	}
	if s.ApprovalRequired {
		t.Error("isApprovalRequired is false in the capture")
	}
	if s.AuthContextEnabled {
		t.Error("AuthenticationContext isEnabled is false in the capture")
	}
	if !strings.HasSuffix(s.PolicyID, "99999999-9999-9999-9999-999999999999") {
		t.Errorf("policyId = %q", s.PolicyID)
	}
}

func TestGetRoleSettingsParsesRealPolicy(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch {
		case strings.Contains(r.URL.Path, "roleManagementPolicyAssignments"):
			filter := r.URL.Query().Get("$filter")
			want := "roleDefinitionId eq '/providers/Microsoft.Authorization/roleDefinitions/b24988ac-6180-42a0-ab88-20f7382dd24c'"
			if filter != want {
				t.Errorf("$filter =\n %q\nwant %q", filter, want)
			}
			respond(t, w, readTestdata(t, "policy_assignment.json"))
		case strings.Contains(r.URL.Path, "roleManagementPolicies"):
			respond(t, w, readTestdata(t, "policy.json"))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})

	scope := "/providers/Microsoft.Management/managementGroups/contoso-prod"
	roleDef := "/providers/Microsoft.Authorization/roleDefinitions/b24988ac-6180-42a0-ab88-20f7382dd24c"
	s, err := c.GetRoleSettings(context.Background(), scope, roleDef)
	if err != nil {
		t.Fatalf("GetRoleSettings: %v", err)
	}
	assertPolicyFixture(t, s)

	// A second lookup for the same (scope, role) must be served from cache.
	before := calls.Load()
	if _, err := c.GetRoleSettings(context.Background(), scope, roleDef); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != before {
		t.Errorf("policy lookup was not cached: %d extra call(s)", calls.Load()-before)
	}
}

func TestParsePolicyVariants(t *testing.T) {
	mk := func(maxDur string, enabled []string, approval, authCtx bool) []byte {
		rules := []map[string]any{
			{
				"id":              "Expiration_EndUser_Assignment",
				"ruleType":        "RoleManagementPolicyExpirationRule",
				"maximumDuration": maxDur,
			},
			{
				"id":           "Enablement_EndUser_Assignment",
				"ruleType":     "RoleManagementPolicyEnablementRule",
				"enabledRules": enabled,
			},
			{
				"id":       "Approval_EndUser_Assignment",
				"ruleType": "RoleManagementPolicyApprovalRule",
				"setting":  map[string]any{"isApprovalRequired": approval},
			},
			{
				"id":         "AuthenticationContext_EndUser_Assignment",
				"ruleType":   "RoleManagementPolicyAuthenticationContextRule",
				"isEnabled":  authCtx,
				"claimValue": "c1",
			},
			// Admin-side rules must be ignored.
			{
				"id":              "Expiration_Admin_Eligibility",
				"ruleType":        "RoleManagementPolicyExpirationRule",
				"maximumDuration": "P365D",
			},
		}
		b, err := json.Marshal(map[string]any{"id": "/p", "properties": map[string]any{"rules": rules}})
		if err != nil {
			t.Fatalf("marshalling the policy fixture: %v", err)
		}
		return b
	}
	for _, c := range []struct {
		iso  string
		want time.Duration
	}{{"PT1H", time.Hour}, {"PT10H", 10 * time.Hour}, {"PT1H30M", 90 * time.Minute}} {
		s, err := ParsePolicy(
			mk(c.iso, []string{"MultiFactorAuthentication", "Justification", "Ticketing"}, true, true),
		)
		if err != nil {
			t.Fatalf("ParsePolicy(%s): %v", c.iso, err)
		}
		if s.MaximumDuration != c.want {
			t.Errorf("%s -> %v, want %v", c.iso, s.MaximumDuration, c.want)
		}
		if !s.RequiresTicket() {
			t.Error("Ticketing not detected")
		}
		if !s.ApprovalRequired {
			t.Error("approval not detected")
		}
		if !s.AuthContextEnabled || s.AuthContextClaimValue != "c1" {
			t.Errorf("auth context = %v / %q", s.AuthContextEnabled, s.AuthContextClaimValue)
		}
	}
	if _, err := ParsePolicy([]byte(`{"properties":{"rules":[]}}`)); err == nil {
		t.Error("a policy with no expiration rule should be an error, not a silent zero duration")
	}
}

func TestSubmitRequestSendsProvenBodyShape(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decoding the request body: %v", err)
			return
		}
		w.WriteHeader(http.StatusCreated)
		respond(t, w, readTestdata(t, "request_provisioned.json"))
	})

	scope := "/subscriptions/44444444-4444-4444-4444-444444444444"
	body := RequestBody{Properties: RequestProperties{
		PrincipalID:                     "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		RoleDefinitionID:                scope + "/providers/Microsoft.Authorization/roleDefinitions/b24988ac-6180-42a0-ab88-20f7382dd24c",
		RequestType:                     RequestTypeSelfActivate,
		LinkedRoleEligibilityScheduleID: "/providers/Microsoft.Management/managementGroups/contoso-prod/providers/Microsoft.Authorization/roleEligibilitySchedules/77777777-7777-7777-7777-777777777777",
		Justification:                   "Deploy!",
		ScheduleInfo: &ScheduleInfo{
			StartDateTime: "2026-08-10T09:27:45Z",
			Expiration:    Expiration{Type: "AfterDuration", Duration: "PT4H"},
		},
	}}
	sr, err := c.SubmitRequest(context.Background(), scope, "dddddddd-dddd-dddd-dddd-dddddddddddd", body)
	if err != nil {
		t.Fatalf("SubmitRequest: %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("method = %s, want PUT", gotMethod)
	}
	wantPath := scope + "/providers/Microsoft.Authorization/roleAssignmentScheduleRequests/dddddddd-dddd-dddd-dddd-dddddddddddd"
	if gotPath != wantPath {
		t.Errorf("path =\n %s\nwant %s", gotPath, wantPath)
	}
	props := mustObject(t, gotBody, "properties")
	// Golden comparison against the proven template's field set.
	for k, want := range map[string]string{
		"principalId":                     "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		"requestType":                     "SelfActivate",
		"roleDefinitionId":                scope + "/providers/Microsoft.Authorization/roleDefinitions/b24988ac-6180-42a0-ab88-20f7382dd24c",
		"linkedRoleEligibilityScheduleId": "/providers/Microsoft.Management/managementGroups/contoso-prod/providers/Microsoft.Authorization/roleEligibilitySchedules/77777777-7777-7777-7777-777777777777",
		"justification":                   "Deploy!",
	} {
		got, ok := props[k].(string)
		if !ok || got != want {
			t.Errorf("properties.%s = %#v, want %q", k, props[k], want)
		}
	}
	si := mustObject(t, props, "scheduleInfo")
	exp := mustObject(t, si, "expiration")
	if exp["type"] != "AfterDuration" || exp["duration"] != "PT4H" {
		t.Errorf("expiration = %v", exp)
	}
	if _, ok := props["ticketInfo"]; ok {
		t.Error("ticketInfo must be omitted when the policy has no Ticketing rule")
	}
	if sr.Properties.Status != StatusProvisioned {
		t.Errorf("status = %q", sr.Properties.Status)
	}
}

func TestPollUntilTerminal(t *testing.T) {
	fastPolling(t)

	statuses := []string{"PendingProvisioning", "ProvisioningStarted", "Provisioned"}
	var n atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		i := int(n.Add(1)) - 1
		if i >= len(statuses) {
			i = len(statuses) - 1
		}
		fmt.Fprintf(w, `{"id":"/req/1","properties":{"status":%q}}`, statuses[i])
	})
	sr := &ScheduleRequest{ID: "/req/1"}
	sr.Properties.Status = "Accepted"
	got, err := c.Poll(context.Background(), sr, 5*time.Second)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if got.Properties.Status != StatusProvisioned {
		t.Fatalf("final status = %q", got.Properties.Status)
	}
}

func TestPollStopsOnPendingApprovalAndOnFailure(t *testing.T) {
	fastPolling(t)
	for _, status := range []string{"PendingApproval", "Failed", "Denied", "TimedOut", "AdminDenied"} {
		var calls atomic.Int32
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			fmt.Fprintf(w, `{"id":"/req/1","properties":{"status":%q}}`, status)
		})
		sr := &ScheduleRequest{ID: "/req/1"}
		sr.Properties.Status = "Accepted"
		got, err := c.Poll(context.Background(), sr, 2*time.Second)
		if err != nil {
			t.Fatalf("%s: %v", status, err)
		}
		if got.Properties.Status != status {
			t.Errorf("%s: got %q", status, got.Properties.Status)
		}
		if calls.Load() != 1 {
			t.Errorf("%s: polled %d times, want 1 — %s is terminal", status, calls.Load(), status)
		}
	}
}

func TestPollTimesOutReportingLastStatus(t *testing.T) {
	fastPolling(t)
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"id":"/req/1","properties":{"status":"PendingProvisioning"}}`)
	})
	sr := &ScheduleRequest{ID: "/req/1"}
	sr.Properties.Status = "Accepted"
	got, err := c.Poll(context.Background(), sr, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if got.Properties.Status != "PendingProvisioning" {
		t.Fatalf("expected the last observed status, got %q", got.Properties.Status)
	}
}

func TestStatusClassification(t *testing.T) {
	for _, s := range []string{"Provisioned", "Granted", "Revoked"} {
		if !IsSuccessStatus(s) {
			t.Errorf("%s should be success", s)
		}
	}
	for _, s := range []string{"PendingApproval", "PendingApprovalProvisioning", "PendingAdminDecision"} {
		if !IsPendingApprovalStatus(s) || IsSuccessStatus(s) {
			t.Errorf("%s should be pending approval and not success", s)
		}
	}
	for _, s := range []string{"Failed", "Denied", "AdminDenied", "TimedOut"} {
		if !IsFailureStatus(s) {
			t.Errorf("%s should be a failure", s)
		}
	}
	for _, s := range []string{"Accepted", "PendingProvisioning", "ProvisioningStarted", "PendingEvaluation"} {
		if IsTerminalStatus(s) {
			t.Errorf("%s must not be terminal — polling has to continue", s)
		}
	}
}

func TestErrorResponseIsAnAPIError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(
			w,
			`{"error":{"code":"RoleAssignmentRequestPolicyValidationFailed","message":"The following policy rules failed: [\"MfaRule\"]"}}`,
		)
	})
	_, err := c.ListEligibilities(context.Background())
	ae := &APIError{}
	ok := errors.As(err, &ae)
	if !ok {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if ae.Kind != KindPolicyValidation {
		t.Errorf("kind = %v", ae.Kind)
	}
	if len(ae.FailedRules) != 1 || ae.FailedRules[0] != "MfaRule" {
		t.Errorf("FailedRules = %v", ae.FailedRules)
	}
	if !strings.Contains(ae.Error(), "RoleAssignmentRequestPolicyValidationFailed") {
		t.Errorf("error text drops the code: %q", ae.Error())
	}
}

func TestRetriesThrottledResponses(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) <= 2 {
			// ARM asks us to come back; a zero wait keeps the test fast.
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"code":"TooManyRequests","message":"Rate limit exceeded."}}`)
			return
		}
		fmt.Fprint(w, `{"value":[]}`)
	})
	if _, err := c.ListEligibilities(context.Background()); err != nil {
		t.Fatalf("a throttled call should have been retried to success: %v", err)
	}
	if calls.Load() != 3 {
		t.Errorf("made %d calls, want 3 (two throttled plus one success)", calls.Load())
	}
}

// TestGivesUpAfterMaxRetries: retrying is per client now, not a package
// variable a test has to put back, so a client can be told how patient to be.
func TestGivesUpAfterMaxRetries(t *testing.T) {
	const maxRetries = 2

	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{}`)
	})
	c.MaxRetries = maxRetries
	_, err := c.ListEligibilities(context.Background())
	if err == nil {
		t.Fatal("expected an error after exhausting the retries")
	}
	if calls.Load() != maxRetries+1 {
		t.Errorf("made %d calls, want %d", calls.Load(), maxRetries+1)
	}
	if !strings.Contains(err.Error(), "throttling") {
		t.Errorf("the message should explain throttling: %q", err.Error())
	}
}

func TestDoesNotRetryClientErrors(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"code":"RoleAssignmentExists","message":"exists"}}`)
	})
	if _, err := c.ListEligibilities(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	if calls.Load() != 1 {
		t.Errorf("a 400 must not be retried; made %d calls", calls.Load())
	}
}

func TestRetryDelayHonoursRetryAfter(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "7")
	if got := retryDelay(ParseAPIError("GET", "/x", 429, nil, h), 0); got != 7*time.Second {
		t.Errorf("retryDelay = %v, want 7s", got)
	}
	// Retry-After: 0 means retry immediately, not "no header".
	h.Set("Retry-After", "0")
	if got := retryDelay(ParseAPIError("GET", "/x", 429, nil, h), 3); got != 0 {
		t.Errorf("retryDelay with Retry-After: 0 = %v, want 0", got)
	}
	// Without the header, back off exponentially.
	none := ParseAPIError("GET", "/x", 429, nil, http.Header{})
	if got := retryDelay(none, 0); got != time.Second {
		t.Errorf("attempt 0 backoff = %v, want 1s", got)
	}
	if got := retryDelay(none, 3); got != 8*time.Second {
		t.Errorf("attempt 3 backoff = %v, want 8s", got)
	}
}

func TestRetryBackoffGrowsWithoutRetryAfter(t *testing.T) {
	// Without a Retry-After header the wait must actually back off rather than
	// sitting at a flat 1s: the attempt counter has to reach retryDelay.
	seen := map[time.Duration]bool{}
	none := ParseAPIError("GET", "/x", 429, nil, http.Header{})
	for attempt := range 4 {
		d := retryDelay(none, attempt)
		want := time.Duration(1<<attempt) * time.Second
		if d != want {
			t.Errorf("attempt %d delay = %v, want %v", attempt, d, want)
		}
		seen[d] = true
	}
	if len(seen) != 4 {
		t.Errorf("backoff is flat: only %d distinct delays", len(seen))
	}
}

func TestListAllErrorsRatherThanTruncating(t *testing.T) {
	// A server that always hands back another nextLink must produce an error,
	// never a silently short list.
	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"value":[{"id":"x"}],"nextLink":%q}`, srvURL+"/next?api-version="+APIVersion)
	}))
	defer srv.Close()
	srvURL = srv.URL

	c := New(srv.URL, "fake", srv.Client())
	_, err := c.ListEligibilities(context.Background())
	if err == nil {
		t.Fatal("an endless nextLink chain must be an error, not a partial list")
	}
	if !strings.Contains(err.Error(), "partial") {
		t.Errorf("the error should say the list would be partial: %v", err)
	}
}

func TestRevokedIsSuccessOnlyForDeactivation(t *testing.T) {
	if !IsSuccessStatusFor(RequestTypeSelfDeactivate, StatusRevoked) {
		t.Error("Revoked is the successful end state of a deactivation")
	}
	if IsSuccessStatusFor(RequestTypeSelfActivate, StatusRevoked) {
		t.Error("an activation that lands on Revoked has granted nothing and must not read as success")
	}
	for _, s := range []string{StatusProvisioned, StatusGranted} {
		if !IsSuccessStatusFor(RequestTypeSelfActivate, s) {
			t.Errorf("%s should be success for an activation", s)
		}
	}
	if _, ok := failureStatuses["Revoked"]; ok {
		t.Error("Revoked must not appear in failureStatuses at all")
	}
}

// fastPolling collapses the poll backoff so a test does not sit out the real
// schedule.
func fastPolling(t *testing.T) {
	t.Helper()
	prev := pollSchedule
	pollSchedule = []time.Duration{time.Millisecond}
	t.Cleanup(func() { pollSchedule = prev })
}

func TestPollBackoffSchedule(t *testing.T) {
	// The first poll dominates an activation's wall time, so it is short; the
	// rest back off rather than hammering ARM.
	want := []time.Duration{
		500 * time.Millisecond,
		time.Second,
		2 * time.Second,
		3 * time.Second,
	}
	for i, w := range want {
		if got := pollWait(i); got != w {
			t.Errorf("pollWait(%d) = %v, want %v", i, got, w)
		}
	}
	// It holds at the last value rather than growing without bound.
	for _, attempt := range []int{4, 5, 20, 100} {
		if got := pollWait(attempt); got != 3*time.Second {
			t.Errorf("pollWait(%d) = %v, want it to hold at 3s", attempt, got)
		}
	}
	// Strictly increasing up to the cap: a schedule that went backwards would
	// poll harder over time, which is the opposite of the intent.
	for i := 1; i < len(want); i++ {
		if want[i] <= want[i-1] {
			t.Fatalf("schedule is not increasing at %d", i)
		}
	}
}

func TestPollReturnsImmediatelyWhenThePutIsAlreadyTerminal(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"id":"/req/1","properties":{"status":"Provisioned"}}`)
	})
	sr := &ScheduleRequest{ID: "/req/1"}
	sr.Properties.Status = StatusProvisioned

	start := time.Now()
	got, err := c.Poll(context.Background(), sr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.Properties.Status != StatusProvisioned {
		t.Errorf("status = %q", got.Properties.Status)
	}
	if calls.Load() != 0 {
		t.Errorf("a terminal PUT must not be re-read; made %d calls", calls.Load())
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("waited %v on an already-terminal request", elapsed)
	}
}

func TestPollDeadlineBoundsSleepRequestsAndRetryAfter(t *testing.T) {
	for _, mode := range []string{"sleep", "request", "retry"} {
		t.Run(mode, func(t *testing.T) {
			if mode != "sleep" {
				fastPolling(t)
			}
			var calls atomic.Int32
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if mode == "request" {
					<-r.Context().Done()
					return
				}
				w.Header().Set("Retry-After", "3600")
				w.WriteHeader(http.StatusTooManyRequests)
			})
			sr := &ScheduleRequest{ID: "/req/1", Properties: ScheduleRequestProperties{Status: "Accepted"}}
			start := time.Now()
			got, err := c.Poll(context.Background(), sr, 50*time.Millisecond)
			if err != nil || got != sr {
				t.Fatalf("budget exhaustion must return last request without interruption: %v, %v", got, err)
			}
			if time.Since(start) > time.Second {
				t.Fatal("poll budget did not bound network and retry waits")
			}
			wantCalls := int32(1)
			if mode == "sleep" {
				wantCalls = 0
			}
			if calls.Load() != wantCalls {
				t.Fatalf("made %d calls, want %d", calls.Load(), wantCalls)
			}
		})
	}
}

func TestPollPreservesCallerCancellation(t *testing.T) {
	fastPolling(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := newTestClient(t, func(_ http.ResponseWriter, r *http.Request) { cancel(); <-r.Context().Done() })
	sr := &ScheduleRequest{ID: "/req/1", Properties: ScheduleRequestProperties{Status: "Accepted"}}
	got, err := c.Poll(ctx, sr, time.Second)
	if got != sr || !errors.Is(err, context.Canceled) {
		t.Fatalf("caller cancellation became normal timeout: %v", err)
	}
}
