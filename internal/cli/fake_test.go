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
	// which takes 11-21 s on the real tenant.
	activeDelay time.Duration
	// pagedScope names one scope whose activation listing is served in two
	// pages joined by a nextLink, the way ARM pages a long listing. Other
	// scopes answer in one page.
	pagedScope string
	// secondPageGets counts fetches of a paged listing's second page, which is
	// the only way its rows can reach the caller.
	secondPageGets int
	// faults are the transient failures a scope's activation listing serves
	// before answering normally, keyed by lower-cased scope id.
	faults map[string]*scopeFault
	// scopeGets counts activation listings per lower-cased scope id, so a test
	// can prove a faulted scope was asked again rather than given up on.
	scopeGets map[string]int
	// getStatuses are the statuses successive read-backs of a schedule request
	// report, consumed from the front and holding at the last one. Empty means
	// every read-back reports putStatus.
	getStatuses []string
	// resources are the subscription, resource-group and resource listings the
	// init scope browser reads, keyed by the lower-cased request path.
	resources map[string][]armclient.Resource
	// pagedResources is the resources key whose listing is served in two pages
	// joined by a nextLink.
	pagedResources string

	srv *httptest.Server
}

// scopeFault is a transient failure the activation listing at one scope
// answers with a bounded number of times before answering normally: the shape
// of ARM throttling a burst, or a gateway timing out on one leg of the fan-out.
type scopeFault struct {
	code       int    // the HTTP status, one of the three the client retries.
	retryAfter string // the Retry-After header value, or empty for no header.
	remaining  int    // how many more calls answer with the fault.
}

// failScope makes the next times activation listings at scope answer with
// code, carrying retryAfter as the Retry-After header when it is non-empty,
// after which the scope answers normally.
func (f *fakeARM) failScope(scope string, code int, retryAfter string, times int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.faults == nil {
		f.faults = map[string]*scopeFault{}
	}
	f.faults[strings.ToLower(scope)] = &scopeFault{code: code, retryAfter: retryAfter, remaining: times}
}

// takeFault consumes one fault at scope, reporting whether there was one to
// serve and what it looks like.
func (f *fakeARM) takeFault(scope string) (fault scopeFault, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sf := f.faults[strings.ToLower(scope)]
	if sf == nil || sf.remaining == 0 {
		return scopeFault{}, false
	}
	sf.remaining--
	return *sf, true
}

// countScopeGet records one activation listing at scope.
func (f *fakeARM) countScopeGet(scope string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.scopeGets == nil {
		f.scopeGets = map[string]int{}
	}
	f.scopeGets[strings.ToLower(scope)]++
}

// scopeGetCount returns how many activation listings scope has answered,
// faulted ones included.
func (f *fakeARM) scopeGetCount(scope string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.scopeGets[strings.ToLower(scope)]
}

// countSecondPage records that a paged listing's second page was fetched.
func (f *fakeARM) countSecondPage() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.secondPageGets++
}

// secondPageCount returns how many second pages have been fetched.
func (f *fakeARM) secondPageCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.secondPageGets
}

// setGetStatuses installs the statuses successive read-backs of a schedule
// request report, in order; the last one is held once the rest are consumed.
func (f *fakeARM) setGetStatuses(statuses ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getStatuses = statuses
}

// nextGetStatus returns the status the next read-back reports: the front of
// getStatuses, consumed until one remains, or putStatus when none were set.
func (f *fakeARM) nextGetStatus() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.getStatuses) == 0 {
		return f.putStatus
	}
	s := f.getStatuses[0]
	if len(f.getStatuses) > 1 {
		f.getStatuses = f.getStatuses[1:]
	}
	return s
}

// isPaged reports whether the activation listing at scope is served in pages.
func (f *fakeARM) isPaged(scope string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pagedScope != "" && strings.EqualFold(f.pagedScope, scope)
}

// pageToken is the query parameter a nextLink carries to ask for the second
// page. ARM's own continuation token is opaque; this one just has to be
// recognisable.
const pageToken = "$skiptoken"

// nextLinkFor builds the absolute URL of a listing's second page, on the same
// origin the request arrived at so the client's origin check accepts it.
func nextLinkFor(r *http.Request) string {
	q := r.URL.Query()
	q.Set(pageToken, "page2")
	return "http://" + r.Host + r.URL.Path + "?" + q.Encode()
}

// serveScopeFault writes the transient failure a faulted scope answers with.
func serveScopeFault(w http.ResponseWriter, fault scopeFault) {
	if fault.retryAfter != "" {
		w.Header().Set("Retry-After", fault.retryAfter)
	}
	w.WriteHeader(fault.code)
	fmt.Fprintf(w, `{"error":{"code":"%s","message":"transient"}}`, http.StatusText(fault.code))
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

// serveActivated answers the activation listing at scope (empty for the
// tenant-wide call), honouring the artificial delay that stands in for ARM's
// slow tenant-wide call, then any fault installed at the scope, then paging.
func (f *fakeARM) serveActivated(w http.ResponseWriter, r *http.Request, scope string) {
	f.countScopeGet(scope)
	f.mu.Lock()
	d := f.activeDelay
	f.mu.Unlock()
	if d > 0 {
		time.Sleep(d)
	}
	if fault, ok := f.takeFault(scope); ok {
		serveScopeFault(w, fault)
		return
	}
	if f.isPaged(scope) {
		f.servePagedActivated(w, r)
		return
	}
	writeJSON(f.t, w, map[string]any{"value": f.activated})
}

// servePagedActivated splits the activations into a first page carrying one
// row and a nextLink, and a second page carrying the rest.
func (f *fakeARM) servePagedActivated(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get(pageToken) == "" {
		writeJSON(f.t, w, map[string]any{"value": f.activated[:1], "nextLink": nextLinkFor(r)})
		return
	}
	f.countSecondPage()
	writeJSON(f.t, w, map[string]any{"value": f.activated[1:]})
}

// serveResources answers the subscription, resource-group and resource
// listings the init scope browser reads, from the resources map. A path the
// map does not know is answered the way ARM answers a subscription or group
// the caller cannot see: a 404 in ARM's error envelope.
func (f *fakeARM) serveResources(w http.ResponseWriter, r *http.Request, path string) {
	key := strings.ToLower(path)
	rows, ok := f.resources[key]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"code":"ResourceNotFound","message":"the scope was not found"}}`)
		return
	}
	if f.pagedResources == key && len(rows) > 1 {
		if r.URL.Query().Get(pageToken) == "" {
			writeJSON(f.t, w, map[string]any{"value": rows[:1], "nextLink": nextLinkFor(r)})
			return
		}
		f.countSecondPage()
		rows = rows[1:]
	}
	writeJSON(f.t, w, map[string]any{"value": rows})
}

// isResourceListing reports whether path is one of the three resource
// listings the init scope browser reads: the tenant's subscriptions, the
// groups under one subscription, or the resources under one group.
func isResourceListing(path string) bool {
	lower := strings.ToLower(path)
	return lower == "/subscriptions" ||
		strings.HasSuffix(lower, "/resourcegroups") ||
		strings.HasSuffix(lower, "/resources")
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

// serveRequest routes one schedule request call: a PUT creates it, a GET reads
// it back with the next status in the configured sequence, and anything else
// is a test bug.
func (f *fakeARM) serveRequest(w http.ResponseWriter, r *http.Request, path string) {
	switch r.Method {
	case http.MethodPut:
		f.serveScheduleRequest(w, r, path)
	case http.MethodGet:
		f.countRequestGet()
		writeJSON(f.t, w, map[string]any{"id": path, "properties": map[string]any{"status": f.nextGetStatus()}})
	default:
		f.t.Errorf("fakeARM: unexpected %s %s", r.Method, path)
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
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
			scope := strings.TrimSuffix(p, "/providers/Microsoft.Authorization/roleAssignmentScheduleInstances")
			if scope == "" {
				f.countTenantWide()
			}
			f.serveActivated(w, r, scope)
		case r.Method == http.MethodGet && strings.Contains(p, "/roleManagementPolicyAssignments"):
			f.servePolicyAssignment(w, p)
		case r.Method == http.MethodGet && strings.Contains(p, "/roleManagementPolicies/"):
			f.servePolicy(w, p)
		case strings.Contains(p, "/roleAssignmentScheduleRequests/"):
			f.serveRequest(w, r, p)
		case r.Method == http.MethodGet && isResourceListing(p):
			f.serveResources(w, r, p)
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

// testDeps is defaultDeps with whatever the test installed. The terminal probe
// is one of the pinned answers rather than the process's own streams, because
// how the suite was started is not something a test controls: a tree that
// asked the host would take the interactive branch on one machine and the
// unattended one on another. [runCmdOn] substitutes the interactive answer
// where a test wants one.
func testDeps() deps {
	d := defaultDeps()
	d.tty = noTTY()
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
