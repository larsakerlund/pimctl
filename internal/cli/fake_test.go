// The fake Azure Resource Manager every command test runs against, and the
// helpers for driving a cobra command at it. Nothing here touches the network
// beyond a local httptest listener. The assertion and fixture helpers are in
// testhelpers_test.go.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/larsakerlund/pimctl/internal/armclient"
	"github.com/larsakerlund/pimctl/internal/azauth"
)

// fakeARM is a stand-in Azure Resource Manager covering the endpoints pimctl
// uses. Every command test runs against it; nothing here touches the network
// beyond the local httptest listener.
type fakeARM struct {
	t *testing.T
	// eligible roles keyed by scope|roleGUID.
	eligibilities []armclient.Eligibility
	activated     []armclient.Assignment
	maxDuration   string
	enabledRules  []string

	// mu guards everything the HTTP handlers touch. pimctl fires activations
	// concurrently, so several handler goroutines write here at once while the
	// test goroutine reads — without this the race detector fires, and the
	// recorded request list would genuinely be corruptible.
	mu sync.Mutex
	// putStatus is the status returned for a newly created request.
	putStatus string
	// putErr, when set, is returned instead of creating the request.
	putErr func(scope string) (int, string)
	puts   []map[string]any
	// requestGets counts read-backs of a schedule request, so a test can prove
	// pimctl consulted ARM instead of trusting the clock.
	requestGets int
	// tenantWideGets counts the slow, lossy asTarget() listing at root scope,
	// which is a fallback nothing should reach on a normal path.
	tenantWideGets int
	// eligGets counts eligibility listings, to tell a cold read from a cached one.
	eligGets int
	// activeDelay simulates ARM's slow roleAssignmentScheduleInstances call,
	// which takes 12-19s on the real tenant.
	activeDelay time.Duration

	srv *httptest.Server
}

// setPutStatus changes the status new requests report. Safe to call while the
// fake server is running.
func (f *fakeARM) setPutStatus(status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.putStatus = status
}

func (f *fakeARM) status() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.putStatus
}

// setPutErr installs a handler that fails PUTs instead of creating requests.
func (f *fakeARM) setPutErr(fn func(scope string) (int, string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.putErr = fn
}

func (f *fakeARM) errFn() func(string) (int, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.putErr
}

// countRequestGet records that a schedule request was read back, which is how
// the verification tests prove pimctl asked ARM rather than trusting a clock.
// countTenantWide records a listing at root scope — the fallback path.
func (f *fakeARM) countTenantWide() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tenantWideGets++
}

func (f *fakeARM) tenantWideCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tenantWideGets
}

func (f *fakeARM) countEligGet() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.eligGets++
}

func (f *fakeARM) eligCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.eligGets
}

func (f *fakeARM) countRequestGet() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requestGets++
}

func (f *fakeARM) requestGetCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requestGets
}

func (f *fakeARM) recordPut(body map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts = append(f.puts, body)
}

// putBodies returns a copy of the request bodies received so far.
func (f *fakeARM) putBodies() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.puts...)
}

func (f *fakeARM) resetPuts() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts = nil
}

// serveActivated answers the activation listing, honouring the artificial delay
// that stands in for ARM's slow tenant-wide call.
func (f *fakeARM) serveActivated(w http.ResponseWriter) {
	f.mu.Lock()
	d := f.activeDelay
	f.mu.Unlock()
	if d > 0 {
		time.Sleep(d)
	}
	writeJSON(f.t, w, map[string]any{"value": f.activated})
}

// servePolicyAssignment points every scope at the same stub policy.
func (f *fakeARM) servePolicyAssignment(w http.ResponseWriter, path string) {
	scope := strings.TrimSuffix(path, "/providers/Microsoft.Authorization/roleManagementPolicyAssignments")
	writeJSON(f.t, w, map[string]any{"value": []any{
		map[string]any{
			"properties": map[string]any{
				"policyId": scope + "/providers/Microsoft.Authorization/roleManagementPolicies/pol1",
			},
		},
	}})
}

// servePolicy answers with the rule set the tests configure through maxDuration
// and enabledRules.
func (f *fakeARM) servePolicy(w http.ResponseWriter, path string) {
	writeJSON(f.t, w, map[string]any{"id": path, "properties": map[string]any{"rules": []any{
		map[string]any{"id": "Expiration_EndUser_Assignment", "maximumDuration": f.maxDuration},
		map[string]any{"id": "Enablement_EndUser_Assignment", "enabledRules": f.enabledRules},
		map[string]any{
			"id":      "Approval_EndUser_Assignment",
			"setting": map[string]any{"isApprovalRequired": false},
		},
		map[string]any{"id": "AuthenticationContext_EndUser_Assignment", "isEnabled": false},
	}}})
}

// serveScheduleRequest records an activation or deactivation request and echoes
// back the shape ARM returns, or the stubbed failure if one is installed.
func (f *fakeARM) serveScheduleRequest(w http.ResponseWriter, r *http.Request, path string) {
	scope, _, _ := strings.Cut(path, "/providers/Microsoft.Authorization/roleAssignmentScheduleRequests/")
	if fn := f.errFn(); fn != nil {
		if code, body := fn(scope); code != 0 {
			w.WriteHeader(code)
			fmt.Fprint(w, body)
			return
		}
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.t.Errorf("decoding the request body: %v", err)
		return
	}
	f.recordPut(body)
	props, ok := body["properties"].(map[string]any)
	if !ok {
		f.t.Errorf("request body has no properties object: %#v", body)
		return
	}
	// scheduleInfo is absent on a deactivation; echoing back nil is the point.
	si, _ := props["scheduleInfo"].(map[string]any) //nolint:errcheck,revive // see above
	w.WriteHeader(http.StatusCreated)
	writeJSON(f.t, w, map[string]any{"id": path, "name": "req", "properties": map[string]any{
		"status": f.status(), "scope": scope, "scheduleInfo": si,
		"roleDefinitionId": props["roleDefinitionId"], "principalId": props["principalId"],
	}})
}

// applyDefaults fills in the policy and request-status stubs a test did not set.
func (f *fakeARM) applyDefaults() {
	if f.maxDuration == "" {
		f.maxDuration = "PT4H"
	}
	if f.enabledRules == nil {
		f.enabledRules = []string{"MultiFactorAuthentication", "Justification"}
	}
	if f.putStatus == "" {
		f.putStatus = "Provisioned"
	}
}

func (f *fakeARM) start() *httptest.Server {
	f.applyDefaults()
	mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/roleEligibilityScheduleInstances"):
			f.countEligGet()
			writeJSON(f.t, w, map[string]any{"value": f.eligibilities})
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/roleAssignmentScheduleInstances"):
			// At root scope this is the tenant-wide listing; anywhere else it
			// is one leg of the per-scope fan-out.
			if p == "/providers/Microsoft.Authorization/roleAssignmentScheduleInstances" {
				f.countTenantWide()
			}
			f.serveActivated(w)
		case r.Method == http.MethodGet && strings.Contains(p, "/roleManagementPolicyAssignments"):
			f.servePolicyAssignment(w, p)
		case r.Method == http.MethodGet && strings.Contains(p, "/roleManagementPolicies/"):
			f.servePolicy(w, p)
		case r.Method == http.MethodPut && strings.Contains(p, "/roleAssignmentScheduleRequests/"):
			f.serveScheduleRequest(w, r, p)
		case r.Method == http.MethodGet && strings.Contains(p, "/roleAssignmentScheduleRequests/"):
			f.countRequestGet()
			writeJSON(f.t, w, map[string]any{"id": p, "properties": map[string]any{"status": f.status()}})
		default:
			f.t.Errorf("fakeARM: unexpected %s %s", r.Method, p)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	f.srv = httptest.NewServer(mux)
	f.t.Cleanup(f.srv.Close)
	return f.srv
}

// install points the CLI at the fake ARM for the duration of a test, and
// isolates the config directory so tests never touch the real one.
func (f *fakeARM) install() {
	f.installContexts([]string{"contoso"}, nil)
}

// installContexts backs each named context with the fake ARM, and makes each
// error in failures look like a context whose token could not be minted.
func (f *fakeARM) installContexts(contexts []string, failures []error) {
	f.t.Setenv("XDG_CONFIG_HOME", f.t.TempDir())
	f.t.Setenv("XDG_CACHE_HOME", f.t.TempDir())
	// XDG_STATE_HOME too: without it the activation record went to the
	// developer's real ~/.local/state/pimctl, which then showed phantom roles
	// in their next `pimctl status`.
	f.t.Setenv("XDG_STATE_HOME", f.t.TempDir())
	// Every test names its context explicitly, so resolution never depends on
	// the developer's own shell.
	f.t.Setenv(envContext, "")
	// No unit test may reach a real cloudctx or az: CI has neither, and on a
	// developer's machine the result would otherwise depend on which contexts
	// that person happens to have configured.
	installFakeRunner(f.t, contexts)
	srv := f.start()
	installSessionOpener(f.t, func(resolution, *timings, bool) ([]*session, []error, error) {
		sessions := make([]*session, 0, len(contexts))
		for _, name := range contexts {
			tok := &azauth.Token{
				Context:     name,
				AccessToken: "fake",
				PrincipalID: "oid-1",
				TenantID:    "tid-1",
			}
			sessions = append(sessions, &session{
				Context: name,
				Token:   tok,
				Client:  armclient.New(srv.URL, tok.AccessToken, srv.Client()),
			})
		}
		if len(sessions) == 0 {
			return nil, failures, errors.New("no context could be used")
		}
		return sessions, failures, nil
	})
}

// fakeOpener is the session opener [runCmd] builds its command tree with while
// a test has one installed. Test-local, and restored by t.Cleanup: the
// production code carries no seam of its own — deps are injected at
// construction, and this is how a test injects them through a helper that
// builds the tree for it.
var fakeOpener sessionOpener

// installSessionOpener makes every runCmd in this test use open instead of
// minting real tokens.
func installSessionOpener(t *testing.T, open sessionOpener) {
	t.Helper()
	prev := fakeOpener
	fakeOpener = open
	t.Cleanup(func() { fakeOpener = prev })
}

// fakeTimeouts is the time budget runCmd builds its command tree with while a
// test has one installed. Nil means the production budgets.
var fakeTimeouts *timeouts

// installTimeouts shortens a run's budgets for one test. Passing the whole
// value rather than one field keeps a test's intent readable: a test that only
// shortens the poll is saying it does not care about the fan-out.
func installTimeouts(t *testing.T, tm timeouts) {
	t.Helper()
	prev := fakeTimeouts
	fakeTimeouts = &tm
	t.Cleanup(func() { fakeTimeouts = prev })
}

// testDeps is defaultDeps with whatever the test installed.
func testDeps() deps {
	d := defaultDeps()
	if fakeOpener != nil {
		d.openSessions = fakeOpener
	}
	if fakeTimeouts != nil {
		d.timeouts = *fakeTimeouts
	}
	return d
}

func mkElig(role, roleGUID, scope, scopeName, scopeType string) armclient.Eligibility {
	e := armclient.Eligibility{
		ID:   scope + "/providers/Microsoft.Authorization/roleEligibilityScheduleInstances/" + roleGUID,
		Name: roleGUID,
	}
	e.Properties.Scope = scope
	e.Properties.RoleDefinitionID = scope + "/providers/Microsoft.Authorization/roleDefinitions/" + roleGUID
	e.Properties.RoleEligibilityScheduleID = scope + "/providers/Microsoft.Authorization/roleEligibilitySchedules/" + roleGUID
	e.Properties.MemberType = "Group"
	e.Properties.Status = "Provisioned"
	e.Properties.ExpandedProperties.RoleDefinition = armclient.Named{DisplayName: role, Type: "BuiltInRole"}
	e.Properties.ExpandedProperties.Scope = armclient.Named{DisplayName: scopeName, Type: scopeType, ID: scope}
	e.Properties.ExpandedProperties.Principal = armclient.Named{DisplayName: "RBAC-ContosoDevelopers", Type: "Group"}
	return e
}

// runCmd executes pimctl with the given argv, capturing stdout and stderr.
func runCmd(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := newRootCmd(testDeps())
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs(args)
	err = root.ExecuteContext(context.Background())
	return out.String(), errb.String(), err
}

func twoLowImpactRoles() []armclient.Eligibility {
	return []armclient.Eligibility{
		mkElig(
			"Cost Management Contributor",
			"434105ed-43f6-45c7-a02f-909b2ba83430",
			"/providers/Microsoft.Management/managementGroups/contoso-prod",
			"Contoso landing zones",
			"managementgroup",
		),
		mkElig("Resource Policy Contributor", "36243c78-bf99-498c-9df9-86d9f8d28608",
			"/providers/Microsoft.Management/managementGroups/contoso-qa", "QA", "managementgroup"),
	}
}

// newRootCmdWithDiscard is a command whose output goes nowhere, for tests that
// call a gather helper directly and do not care what it prints.
func newRootCmdWithDiscard(t *testing.T) *cobra.Command {
	t.Helper()
	cmd := NewRootCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	return cmd
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
