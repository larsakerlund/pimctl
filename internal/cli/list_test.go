// Tests for list.go, including the one that blocks every request to prove a
// warm run prints before any network call. The fake ARM is in fake_test.go.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/larsakerlund/pimctl/internal/armclient"
	"github.com/larsakerlund/pimctl/internal/azauth"
	"github.com/larsakerlund/pimctl/internal/cache"
)

func TestListCommandTable(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()}
	f.install()
	out, _, err := runCmd(t, "list", "-c", "contoso")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, want := range []string{"Cost Management Contributor", "Resource Policy Contributor", "ManagementGroup", "2 eligible role(s)."} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}
	assertWidth(t, out)
}

func TestListCommandJSON(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()}
	f.install()
	out, _, err := runCmd(t, "list", "-c", "contoso", "-o", "json")
	if err != nil {
		t.Fatalf("list -o json: %v", err)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows", len(rows))
	}
	// JSON carries the full scope id, unlike the truncated table column.
	if rows[0]["scope"] != "/providers/Microsoft.Management/managementGroups/contoso-prod" {
		t.Errorf("scope = %v", rows[0]["scope"])
	}
	if rows[0]["roleEligibilityScheduleId"] == "" {
		t.Error("roleEligibilityScheduleId missing from JSON output")
	}
}

func TestListJSONCarriesTheDisambiguatedScopeLabel(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()}
	f.install()
	out, _, err := runCmd(t, "list", "-c", "contoso", "-o", "json")
	if err != nil {
		t.Fatalf("list -o json: %v", err)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	// scopeName stays the bare display name; scopeLabel is what the table
	// prints, which for a management group always names the group itself.
	// Three management groups in this tenant are called "Contoso landing zones",
	// so a caller rendering scopeName alone shows the operator the wrong place.
	want := map[string][2]string{
		"/providers/Microsoft.Management/managementGroups/contoso-prod": {
			"Contoso landing zones", "Contoso landing zones (contoso-prod)",
		},
		"/providers/Microsoft.Management/managementGroups/contoso-qa": {"QA", "QA (contoso-qa)"},
	}
	for _, row := range rows {
		scope, ok := row["scope"].(string)
		if !ok {
			t.Fatalf("a row carried no scope id:\n%s", out)
		}
		expect, ok := want[scope]
		if !ok {
			t.Fatalf("unexpected scope %q in:\n%s", scope, out)
		}
		if row["scopeName"] != expect[0] {
			t.Errorf("scopeName for %s = %v, want %q", scope, row["scopeName"], expect[0])
		}
		if row["scopeLabel"] != expect[1] {
			t.Errorf("scopeLabel for %s = %v, want %q", scope, row["scopeLabel"], expect[1])
		}
	}
}

func TestLsIsAliasForList(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()}
	f.install()
	out, _, err := runCmd(t, "ls", "-c", "contoso")
	if err != nil {
		t.Fatalf("ls: %v", err)
	}
	if !strings.Contains(out, "2 eligible role(s).") {
		t.Errorf("ls output:\n%s", out)
	}
}

func TestSelectByKeyEndToEnd(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()}
	f.install()

	out, _, err := runCmd(t, "list", "-c", "contoso", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatal(err)
	}
	key := mustText(t, rows[0], "key")
	if len(key) != 8 {
		t.Fatalf("list -o json should carry an 8-character key, got %q", key)
	}

	f.resetPuts()
	if _, _, err := runCmd(t, "up", "-c", "contoso", "--key", key, "-j", "x", "-y"); err != nil {
		t.Fatalf("up --key: %v", err)
	}
	if len(f.putBodies()) != 1 {
		t.Fatalf("--key sent %d activations, want 1", len(f.putBodies()))
	}
	props := mustObject(t, f.putBodies()[0], "properties")
	wantRole := mustText(t, rows[0], "roleDefinitionId")
	if props["roleDefinitionId"] != wantRole {
		t.Errorf("--key activated the wrong role: %v", props["roleDefinitionId"])
	}
}

func TestCacheServesTheSecondListAndRefreshBypassesIt(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()}
	f.install()

	if _, _, err := runCmd(t, "list", "-c", "contoso"); err != nil {
		t.Fatal(err)
	}
	// Change what the server would return; a cached run must not see it.
	f.eligibilities = twoLowImpactRoles()[:1]

	out, errOut, err := runCmd(t, "list", "-c", "contoso")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "2 eligible role(s).") {
		t.Errorf("the second list should have come from cache:\n%s", out)
	}
	if !strings.Contains(errOut, "cached") || !strings.Contains(errOut, "--refresh") {
		t.Errorf("the user must be told the list is cached and how to bypass it: %q", errOut)
	}

	out, _, err = runCmd(t, "list", "-c", "contoso", "--refresh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1 eligible role(s).") {
		t.Errorf("--refresh should have re-read from the server:\n%s", out)
	}
}

// TestListDoesNotBlockOnTheActiveListing pins the snappiness requirement: the
// eligible-role table appears immediately, without waiting for the per-scope
// activation fan-out at all.
func TestListDoesNotBlockOnTheActiveListing(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles(), activeDelay: 5 * time.Second}
	f.install()

	start := time.Now()
	out, errOut, err := runCmd(t, "list", "-c", "contoso")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("list waited %v; it must not block on the activation listing", elapsed)
	}
	if !strings.Contains(out, "Cost Management Contributor") {
		t.Errorf("the eligible roles should still be shown:\n%s", out)
	}
	if !strings.Contains(errOut, "this machine's own record") ||
		!strings.Contains(errOut, "--with-active") {
		t.Errorf("the user must be told where ACTIVE came from: %q", errOut)
	}
}

// TestListWithActiveWaits: --with-active opts back into the wait.
func TestListWithActiveWaits(t *testing.T) {
	end := time.Now().Add(time.Hour)
	elig := twoLowImpactRoles()
	a := armclient.Assignment{ID: "/x"}
	a.Properties.AssignmentType = "Activated"
	a.Properties.Scope = elig[0].Properties.Scope
	a.Properties.RoleDefinitionID = elig[0].Properties.RoleDefinitionID
	a.Properties.EndDateTime = &end
	a.Properties.ExpandedProperties = elig[0].Properties.ExpandedProperties

	f := &fakeARM{t: t, eligibilities: elig, activated: []armclient.Assignment{a}, activeDelay: 300 * time.Millisecond}
	f.install()

	out, _, err := runCmd(t, "list", "-c", "contoso", "--with-active")
	if err != nil {
		t.Fatalf("list --with-active: %v", err)
	}
	if !strings.Contains(out, "ACTIVE") || !strings.Contains(out, "until ") {
		t.Errorf("--with-active should have waited and filled the column:\n%s", out)
	}
}

// TestInterruptedListSaysNothingAboutTheTenant: after Ctrl-C an empty listing
// means "we never got the data", so stdout must not claim the tenant is empty.
func TestInterruptedListSaysNothingAboutTheTenant(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()}
	f.install()

	root := newRootCmd(testDeps())
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs([]string{"list", "-c", "contoso"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already interrupted.
	err := root.ExecuteContext(ctx)

	if err == nil {
		t.Fatal("an interrupted list must not report success")
	}
	if strings.Contains(out.String(), "No eligible Azure resource roles") {
		t.Errorf("stdout claimed the tenant is empty after an interrupt:\n%s", out.String())
	}
}

// TestListFromCacheProducesOutputBeforeAnyNetworkCall is the snappiness
// guarantee stated as a test: with a warm eligibility cache, `ls` must reach
// stdout without waiting on ARM at all. The fake blocks every request
// indefinitely, so any network dependency on the critical path hangs the test
// rather than merely slowing it.
func TestListFromCacheProducesOutputBeforeAnyNetworkCall(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv(envContext, "")
	installFakeRunner(t, []string{"contoso"})

	// Warm the eligibility cache directly: this is the state after any earlier
	// command in the same ten minutes.
	cache.Write("contoso", twoLowImpactRoles())

	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })
	var reached atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reached.Store(true)
		select {
		case <-blocked:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)

	installSessionOpener(t, func(resolution, *timings, bool) ([]*session, []error, error) {
		tok := &azauth.Token{Context: "contoso", AccessToken: "fake", PrincipalID: "oid", TenantID: "tid"}
		return []*session{{
			Context: "contoso", Token: tok,
			Client: armclient.New(srv.URL, tok.AccessToken, srv.Client()),
		}}, nil, nil
	})

	done := make(chan string, 1)
	go func() {
		// The error is irrelevant here: the test is about *when* stdout
		// appears, not whether the background listing eventually failed.
		out, _, runErr := runCmd(t, "list", "-c", "contoso")
		if runErr != nil {
			t.Logf("list returned %v (expected: the background listing never completes)", runErr)
		}
		done <- out
	}()

	select {
	case out := <-done:
		if !strings.Contains(out, "Cost Management Contributor") {
			t.Errorf("list printed no roles:\n%s", out)
		}
		if !strings.Contains(out, "2 eligible role(s).") {
			t.Errorf("list output:\n%s", out)
		}
	case <-time.After(3 * time.Second):
		t.Fatal(
			"list did not produce output while ARM was unreachable — something on the warm path still waits for the network",
		)
	}
}
