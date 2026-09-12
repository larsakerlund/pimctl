// Covers active.go: that the default really is a per-scope fan-out, that
// --all-scopes falls back to the tenant-wide call, that the fan-out is sized to
// the work, and — the one that matters most — that a scope missing its soft
// deadline is named rather than dropped.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/larsakerlund/pimctl/internal/armclient"
	"github.com/larsakerlund/pimctl/internal/azauth"
	"github.com/larsakerlund/pimctl/internal/cache"
)

func TestActivationDiscoveryKeepsEachContextsScopes(t *testing.T) {
	for _, emptySecond := range []bool{false, true} {
		t.Run(strconv.FormatBool(emptySecond), func(t *testing.T) {
			t.Setenv("XDG_CACHE_HOME", t.TempDir())
			var mu sync.Mutex
			queries := map[string][]string{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				label := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				scope := strings.TrimSuffix(
					r.URL.Path,
					"/providers/Microsoft.Authorization/roleAssignmentScheduleInstances",
				)
				mu.Lock()
				queries[label] = append(queries[label], scope)
				mu.Unlock()
				if scope != "/subscriptions/"+label && (!emptySecond || label != "globex" || scope != "") {
					w.WriteHeader(http.StatusForbidden)
					fmt.Fprint(w, `{"error":{"code":"AuthorizationFailed","message":"wrong tenant"}}`)
					return
				}
				fmt.Fprint(w, `{"value":[]}`)
			}))
			t.Cleanup(srv.Close)
			rc := &runContext{Ctx: context.Background(), Timeouts: defaultTimeouts(), Timings: newTimings(false)}
			for _, label := range []string{"contoso", "globex"} {
				tok := &azauth.Token{Context: label, AccessToken: label, TenantID: label, PrincipalID: "oid-1"}
				rc.Sessions = append(
					rc.Sessions,
					&session{Context: label, Token: tok, Client: armclient.New(srv.URL, label, srv.Client())},
				)
				elig := []armclient.Eligibility{
					mkElig("Reader", "reader-guid", "/subscriptions/"+label, label, "Subscription"),
				}
				if emptySecond && label == "globex" {
					elig = nil
				}
				cache.Write(tok.Owner(), elig)
			}
			_, errs, slow := listActivations(rc.Ctx, rc, nil)
			if len(errs) > 0 || len(slow) > 0 {
				t.Fatalf("healthy contexts failed: %v %v", errs, slow)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, label := range []string{"contoso", "globex"} {
				if len(queries[label]) != 1 {
					t.Errorf("%s queried %v; want exactly its own scope or fallback", label, queries[label])
				}
			}
		})
	}
}

func TestActivationMatchingAndUnknownScopesKeepContext(t *testing.T) {
	scope := "/providers/Microsoft.Management/managementGroups/shared-name"
	a := mkActivated("Reader", "reader-guid", scope, "Shared", time.Now().Add(time.Hour))
	a.ID = scope + "/instances/shared-id"
	active := []activeRow{{Context: "contoso", Assignment: a}, {Context: "globex", Assignment: a}}
	if got := len(dedupeActivations(active)); got != 2 {
		t.Fatalf("deduplication lost another context: %d", got)
	}
	rows := []row{
		{Context: "contoso", Elig: mkElig("Reader", "reader-guid", scope, "Shared", "ManagementGroup")},
		{Context: "globex", Elig: mkElig("Reader", "reader-guid", scope, "Shared", "ManagementGroup")},
	}
	matched := applyActive(rows, active[:1])
	if !matched[0].IsActive() || matched[1].IsActive() {
		t.Fatal("one context's activation marked another context active")
	}
	for i := range active {
		active[i].State = RowUnconfirmed
	}
	local := localRecord{rows: active}
	merged := mergeActive(local, nil, []activationScope{{Context: "contoso", ID: scope}})
	if len(merged) != 1 || merged[0].Context != "contoso" {
		t.Fatalf("unknown scope affected another context: %+v", merged)
	}
}

// TestActiveFanOutQueriesEachScope pins the replacement for ARM's tenant-wide
// activation listing, which takes 11-21s and was measured returning 126/131/132
// rows on three runs against an unchanged tenant.
func TestActiveFanOutQueriesEachScope(t *testing.T) {
	scopes := []string{
		"/providers/Microsoft.Management/managementGroups/contoso-prod",
		"/providers/Microsoft.Management/managementGroups/contoso-qa",
		"/providers/Microsoft.Management/managementGroups/contoso-test",
	}
	elig := make([]armclient.Eligibility, 0, len(scopes))
	for i, sc := range scopes {
		elig = append(
			elig,
			mkElig(
				"Cost Management Contributor",
				fmt.Sprintf("guid%d", i),
				sc,
				"Contoso landing zones",
				"managementgroup",
			),
		)
	}

	var (
		mu        sync.Mutex
		perScope  = map[string]int{}
		tenantHit int
	)
	end := time.Now().Add(time.Hour)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/roleEligibilityScheduleInstances"):
			writeJSON(t, w, map[string]any{"value": elig})
		case strings.HasSuffix(p, "/roleAssignmentScheduleInstances"):
			scope := strings.TrimSuffix(p, "/providers/Microsoft.Authorization/roleAssignmentScheduleInstances")
			mu.Lock()
			if scope == "" {
				tenantHit++
			} else {
				perScope[scope]++
			}
			mu.Unlock()
			if scope == "" {
				// The tenant-wide call must not be used by default.
				writeJSON(t, w, map[string]any{"value": []any{}})
				return
			}
			// Each scope reports its own activation, and every scope also
			// echoes a shared one to exercise dedupe by instance id.
			a := armclient.Assignment{
				ID: scope + "/providers/Microsoft.Authorization/roleAssignmentScheduleInstances/" + scopeLeaf(scope),
			}
			a.Properties.AssignmentType = "Activated"
			a.Properties.Scope = scope
			a.Properties.RoleDefinitionID = "/providers/Microsoft.Authorization/roleDefinitions/434105ed"
			a.Properties.EndDateTime = &end
			a.Properties.ExpandedProperties.RoleDefinition = armclient.Named{DisplayName: "Cost Management Contributor"}
			a.Properties.ExpandedProperties.Scope = armclient.Named{
				DisplayName: "Contoso landing zones",
				Type:        "managementgroup",
				ID:          scope,
			}

			shared := a
			shared.ID = "/shared/instance/one"
			shared.Properties.Scope = scopes[0]
			shared.Properties.ExpandedProperties.Scope = armclient.Named{
				DisplayName: "Contoso landing zones",
				Type:        "managementgroup",
				ID:          scopes[0],
			}

			writeJSON(t, w, map[string]any{"value": []armclient.Assignment{a, shared}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv(envContext, "")
	installSessionOpener(t, func(resolution, *timings, bool) ([]*session, []error, error) {
		tok := &azauth.Token{Context: "contoso", AccessToken: "fake", PrincipalID: "oid-1", TenantID: "tid-1"}
		return []*session{
			{Context: "contoso", Token: tok, Client: armclient.New(srv.URL, tok.AccessToken, srv.Client())},
		}, nil, nil
	})

	out, _, err := runCmd(t, "status", "-c", "contoso")
	if err != nil {
		t.Fatalf("status: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if tenantHit != 0 {
		t.Errorf("the tenant-wide listing was used %d times; the fan-out should replace it", tenantHit)
	}
	for _, sc := range scopes {
		if perScope[sc] != 1 {
			t.Errorf("scope %s was queried %d times, want exactly 1", scopeLeaf(sc), perScope[sc])
		}
	}
	// Three distinct instances plus one shared echoed by all three: dedupe by
	// instance id must leave four rows, not six.
	if n := strings.Count(out, "Cost Management Contributor"); n != 4 {
		t.Errorf("expected 4 deduped activations, got %d:\n%s", n, out)
	}
}

// TestAllScopesFallsBackToTheTenantWideCall keeps the escape hatch working.
func TestAllScopesFallsBackToTheTenantWideCall(t *testing.T) {
	var tenantHit, scopeHit int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/roleEligibilityScheduleInstances"):
			writeJSON(t, w, map[string]any{"value": []any{}})
		case strings.HasSuffix(p, "/roleAssignmentScheduleInstances"):
			mu.Lock()
			if strings.TrimSuffix(p, "/providers/Microsoft.Authorization/roleAssignmentScheduleInstances") == "" {
				tenantHit++
			} else {
				scopeHit++
			}
			mu.Unlock()
			writeJSON(t, w, map[string]any{"value": []any{}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv(envContext, "")
	installSessionOpener(t, func(resolution, *timings, bool) ([]*session, []error, error) {
		tok := &azauth.Token{Context: "contoso", AccessToken: "fake", PrincipalID: "oid-1", TenantID: "tid-1"}
		return []*session{
			{Context: "contoso", Token: tok, Client: armclient.New(srv.URL, tok.AccessToken, srv.Client())},
		}, nil, nil
	})

	if _, _, err := runCmd(t, "status", "-c", "contoso", "--all-scopes"); err != nil {
		t.Fatalf("status --all-scopes: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if tenantHit == 0 {
		t.Error("--all-scopes should use the tenant-wide call")
	}
	if scopeHit != 0 {
		t.Errorf("--all-scopes should not fan out, but made %d per-scope calls", scopeHit)
	}
}

// TestScopeFanOutSizesToTheWork pins the concurrency fix: the fan-out's wall
// time is ceil(scopes/concurrency) x slowest call, so a limit below the scope
// count costs a whole extra wave. Measured on a 25-scope tenant: 3.95s at 16,
// 2.44s at 25.
func TestScopeFanOutSizesToTheWork(t *testing.T) {
	for scopes, want := range map[int]int{
		1:   1,
		25:  25,
		31:  31,
		32:  maxScopeFanOut,
		200: maxScopeFanOut,
	} {
		if got := scopeFanOutFor(scopes); got != want {
			t.Errorf("scopeFanOutFor(%d) = %d, want %d", scopes, got, want)
		}
	}
	// Never zero: a zero-capacity semaphore would deadlock the fan-out.
	if got := scopeFanOutFor(0); got < 1 {
		t.Errorf("scopeFanOutFor(0) = %d, must be at least 1", got)
	}
}

// TestSlowScopeIsReportedNotWaitedFor pins the fix for the p90: roughly one
// fan-out in three has a single random scope take 12-15s, which turned a 2.4s
// median into a 16.8s p90 and a 25s worst case. A scope that misses the soft
// deadline is named, not waited for.
func TestSlowScopeIsReportedNotWaitedFor(t *testing.T) {
	fast := "/providers/Microsoft.Management/managementGroups/contoso-prod"
	slow := "/providers/Microsoft.Management/managementGroups/contoso-slow"
	elig := []armclient.Eligibility{
		mkElig("Cost Management Contributor", costGUID, fast, "Contoso landing zones", "managementgroup"),
		mkElig("Owner", "8e3af657", slow, "Contoso landing zones", "managementgroup"),
	}
	end := time.Now().Add(time.Hour)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/roleEligibilityScheduleInstances"):
			writeJSON(t, w, map[string]any{"value": elig})
		case strings.HasSuffix(p, "/roleAssignmentScheduleInstances"):
			scope := strings.TrimSuffix(p, "/providers/Microsoft.Authorization/roleAssignmentScheduleInstances")
			if strings.HasSuffix(scope, "contoso-slow") {
				<-r.Context().Done() // never answers within the deadline.
				return
			}
			a := armclient.Assignment{ID: scope + "/i/1"}
			a.Properties.AssignmentType = "Activated"
			a.Properties.Scope = scope
			a.Properties.RoleDefinitionID = "/providers/Microsoft.Authorization/roleDefinitions/" + costGUID
			a.Properties.EndDateTime = &end
			a.Properties.ExpandedProperties.RoleDefinition = armclient.Named{DisplayName: "Cost Management Contributor"}
			a.Properties.ExpandedProperties.Scope = armclient.Named{
				DisplayName: "Contoso landing zones", Type: "managementgroup", ID: scope,
			}
			writeJSON(t, w, map[string]any{"value": []armclient.Assignment{a}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv(envContext, "")
	installFakeRunner(t, []string{"contoso"})
	installSessionOpener(t, func(resolution, *timings, bool) ([]*session, []error, error) {
		tok := &azauth.Token{Context: "contoso", AccessToken: "fake", PrincipalID: "oid-1", TenantID: "tid-1"}
		return []*session{{
			Context: "contoso", Token: tok,
			Client: armclient.New(srv.URL, tok.AccessToken, srv.Client()),
		}}, nil, nil
	})

	shortDeadlines(t, 150*time.Millisecond)

	start := time.Now()
	out, errOut, err := runCmd(t, "status", "-c", "contoso")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("status waited %v on the slow scope", elapsed)
	}
	if !strings.Contains(out, "Cost Management Contributor") {
		t.Errorf("the scope that answered should still be reported:\n%s", out)
	}
	if !strings.Contains(errOut, "unconfirmed (ARM did not answer)") || !strings.Contains(errOut, "contoso-slow") {
		t.Errorf("the slow scope must be named, not silently dropped: %q", errOut)
	}
	if !strings.Contains(errOut, "--wait") {
		t.Errorf("the message should say how to read the slow scope properly: %q", errOut)
	}
}

// TestWaitRaisesThePerScopeDeadline: --wait asks for the authoritative answer,
// so the snappiness budget no longer applies — but the call stays bounded, since
// an unbounded one is what produced the 25s worst case.
func TestWaitRaisesThePerScopeDeadline(t *testing.T) {
	tm := defaultTimeouts()
	if tm.waitScope <= tm.scopeSoftDeadline {
		t.Fatalf("--wait deadline %v must exceed the soft deadline %v", tm.waitScope, tm.scopeSoftDeadline)
	}
	if tm.waitScope > 2*time.Minute {
		t.Errorf("--wait deadline %v is effectively unbounded", tm.waitScope)
	}
}

// TestColdCacheStillFansOut: `cache clear` wipes the eligibility listing the
// fan-out derives its scopes from. That used to leave it with nothing, so it
// fell back to the tenant-wide asTarget() call — the slow, lossy one — and a
// piped `status` straight after a `cache clear` took two minutes and exited 1
// with "the output above is incomplete". Reading the eligibilities costs well
// under a second; it is never the wrong trade.
func TestColdCacheStillFansOut(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()}
	f.install() // a fresh XDG_CACHE_HOME: nothing is cached.

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"waiting", []string{"status", "-c", "contoso", "--wait"}},
		{"json", []string{"status", "-c", "contoso", "-o", "json"}},
		{"default", []string{"status", "-c", "contoso"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := cache.Clear(); err != nil {
				t.Fatalf("clearing the listing cache: %v", err)
			}
			before := f.eligCount()

			_, errOut, err := runCmd(t, tc.args...)
			if err != nil {
				t.Fatalf("status: %v (stderr %q)", err, errOut)
			}
			if f.eligCount() == before {
				t.Error("a cold cache must be filled by reading the eligibilities, not given up on")
			}
			if n := f.tenantWideCount(); n != 0 {
				t.Errorf("fell back to the tenant-wide listing %d time(s) with a cold cache", n)
			}
		})
	}
}

// TestNoEligibilitiesStillFallsBack: the fallback is right for a user eligible
// for nothing, where there is no scope to fan out over and an activation could
// still exist somewhere the listing cannot suggest.
func TestNoEligibilitiesStillFallsBack(t *testing.T) {
	f := &fakeARM{t: t}
	f.install()

	if _, _, err := runCmd(t, "status", "-c", "contoso", "--wait"); err != nil {
		t.Fatalf("status: %v", err)
	}
	if f.tenantWideCount() == 0 {
		t.Error("with nothing eligible, the tenant-wide listing is the only thing left to ask")
	}
}

// activatedAt is a live activation of role at a management-group scope, with
// the instance id ARM would give it, ending an hour from now.
func activatedAt(role, roleGUID, scope, leaf string) armclient.Assignment {
	a := mkActivated(role, roleGUID, scope, "Contoso landing zones", time.Now().Add(time.Hour))
	a.ID = scope + "/providers/Microsoft.Authorization/roleAssignmentScheduleInstances/" + leaf
	a.Properties.ExpandedProperties.Scope = armclient.Named{
		DisplayName: "Contoso landing zones", Type: "managementgroup", ID: scope,
	}
	return a
}

// statusJSONOf runs `status -o json` for the one fake context and decodes the
// envelope.
func statusJSONOf(t *testing.T) statusJSON {
	t.Helper()
	out, errOut, err := runCmd(t, "status", "-c", "contoso", "-o", "json")
	if err != nil {
		t.Fatalf("status: %v (stderr %q)", err, errOut)
	}
	var got statusJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("status -o json is not valid JSON: %v\n%s", err, out)
	}
	return got
}

// assertConfirmedRole checks that the one role the fake holds came back with
// Azure's confirmation and that no scope was left unread.
func assertConfirmedRole(t *testing.T, got statusJSON) {
	t.Helper()
	if len(got.UnconfirmedScopes) != 0 {
		t.Errorf("a scope was left unconfirmed: %v", got.UnconfirmedScopes)
	}
	if len(got.Roles) != 1 {
		t.Fatalf("got %d roles, want 1: %+v", len(got.Roles), got.Roles)
	}
	if !got.Roles[0].Confirmed || got.Roles[0].State != "confirmed" {
		t.Errorf("the role is %q (confirmed=%v), want Azure's confirmation",
			got.Roles[0].State, got.Roles[0].Confirmed)
	}
}

// TestPagedScopeListingShowsEveryRow: ARM pages a long per-scope listing behind
// a nextLink, and a role on the second page is held just as much as one on the
// first. A listing that stopped at the first page would under-report.
func TestPagedScopeListingShowsEveryRow(t *testing.T) {
	elig := twoLowImpactRoles()[:1]
	scope := elig[0].Properties.Scope
	f := &fakeARM{
		t:             t,
		eligibilities: elig,
		activated: []armclient.Assignment{
			activatedAt("Cost Management Contributor", costGUID, scope, "page1"),
			activatedAt("Resource Policy Contributor", "36243c78-bf99-498c-9df9-86d9f8d28608", scope, "page2"),
		},
		pagedScope: scope,
	}
	f.install()

	out, errOut, err := runCmd(t, "status", "-c", "contoso")
	if err != nil {
		t.Fatalf("status: %v (stderr %q)", err, errOut)
	}
	if n := f.secondPageCount(); n != 1 {
		t.Errorf("the second page was fetched %d time(s), want 1", n)
	}
	for _, want := range []string{"Cost Management Contributor", "Resource Policy Contributor"} {
		if !strings.Contains(out, want) {
			t.Errorf("the paged listing lost %q:\n%s", want, out)
		}
	}
	if strings.Contains(errOut, "unconfirmed") {
		t.Errorf("a paged scope must read as confirmed: %q", errOut)
	}
}

// TestTransientScopeFailuresAreRetriedToConfirmation: a 503 or 504 on one leg
// of the fan-out is ARM asking to be asked again, not an answer. After the
// retry the scope is confirmed, so its roles carry no "?" and it is not named
// as unread.
func TestTransientScopeFailuresAreRetriedToConfirmation(t *testing.T) {
	for _, code := range []int{http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			elig := twoLowImpactRoles()[:1]
			scope := elig[0].Properties.Scope
			f := &fakeARM{
				t:             t,
				eligibilities: elig,
				activated: []armclient.Assignment{
					activatedAt("Cost Management Contributor", costGUID, scope, "one"),
				},
			}
			f.install()
			// Retry-After: 0 makes the retry immediate; waiting out a
			// non-zero header is TestRetryAfterIsHonouredOnAScopeListing.
			f.failScope(scope, code, "0", 1)

			got := statusJSONOf(t)
			if n := f.scopeGetCount(scope); n != 2 {
				t.Errorf("the scope was asked %d time(s), want 2: the %d and then the answer", n, code)
			}
			assertConfirmedRole(t, got)
		})
	}
}

// TestRetryAfterIsHonouredOnAScopeListing: when ARM says how long to wait,
// pimctl waits that long — no less, so the retry is not itself throttled, and
// not so much longer that the scope misses its soft deadline.
func TestRetryAfterIsHonouredOnAScopeListing(t *testing.T) {
	elig := twoLowImpactRoles()[:1]
	scope := elig[0].Properties.Scope
	f := &fakeARM{
		t:             t,
		eligibilities: elig,
		activated:     []armclient.Assignment{activatedAt("Cost Management Contributor", costGUID, scope, "one")},
	}
	f.install()
	f.failScope(scope, http.StatusTooManyRequests, "1", 1)

	start := time.Now()
	got := statusJSONOf(t)
	elapsed := time.Since(start)
	if elapsed < time.Second {
		t.Errorf("status answered after %v; a Retry-After of 1 s was not waited for", elapsed)
	}
	if elapsed > defaultScopeSoftDeadline {
		t.Errorf("status took %v; a 1 s Retry-After fits inside the %v soft deadline",
			elapsed, defaultScopeSoftDeadline)
	}
	if n := f.scopeGetCount(scope); n != 2 {
		t.Errorf("the scope was asked %d time(s), want 2: the 429 and then the answer", n)
	}
	assertConfirmedRole(t, got)
}
