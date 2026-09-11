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
	"sync"
	"testing"
	"time"

	"github.com/larsakerlund/pimctl/internal/armclient"
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
	assertListPrintsBeforeARM(t, false)
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
	assertListPrintsBeforeARM(t, true)
}

// listOutput signals when the complete initial table has reached stdout.
// Its buffer is read only after the command has finished.
type listOutput struct {
	bytes.Buffer

	printed chan struct{}
	once    sync.Once
}

func (w *listOutput) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	if strings.Contains(w.String(), "2 eligible role(s).") {
		w.once.Do(func() { close(w.printed) })
	}
	return n, err
}

func assertListPrintsBeforeARM(t *testing.T, cached bool) {
	t.Helper()
	elig := twoLowImpactRoles()
	a := armclient.Assignment{ID: "/portal-activation"}
	a.Properties.AssignmentType = "Activated"
	a.Properties.Scope = elig[0].Properties.Scope
	a.Properties.RoleDefinitionID = elig[0].Properties.RoleDefinitionID
	a.Properties.ExpandedProperties = elig[0].Properties.ExpandedProperties
	end := time.Now().Add(time.Hour)
	a.Properties.EndDateTime = &end
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/roleEligibilityScheduleInstances") {
			if cached {
				t.Error("a warm list fetched eligibility")
			}
			writeJSON(t, w, map[string]any{"value": elig})
			return
		}
		select {
		case <-release:
			writeJSON(t, w, map[string]any{"value": []armclient.Assignment{a}})
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	installStatusFake(t, srv)
	if cached {
		cache.Write(testOwner("contoso"), elig)
	}
	root := newRootCmd(testDeps())
	out := &listOutput{printed: make(chan struct{})}
	var stderr bytes.Buffer
	root.SetOut(out)
	root.SetErr(&stderr)
	root.SetArgs([]string{"list", "-c", "contoso"})
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	var runErr error
	go func() {
		defer close(finished)
		runErr = root.ExecuteContext(ctx)
	}()
	t.Cleanup(func() { cancel(); <-finished })
	select {
	case <-out.printed:
	case <-time.After(time.Second):
		t.Fatal("list waited for ARM before printing its initial table")
	}
	select {
	case <-finished:
		t.Fatal("list abandoned reconciliation after printing")
	default:
	}
	close(release)
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("list did not complete reconciliation after ARM answered")
	}
	if runErr != nil {
		t.Fatal(runErr)
	}
	assertListReconciled(t, out.String(), stderr.String())
}

func TestListRespectsDeactivationTombstone(t *testing.T) {
	entry := mkRecordEntry("Contributor", "contoso-prod", time.Hour)
	entry.Start = time.Now().Add(-time.Hour)
	stale := recordRow("contoso", nil, entry).Assignment
	stale.ID = "/stale-instance"
	elig := mkElig("Contributor", contribGUID, entry.Scope, entry.ScopeName, "managementgroup")
	f := &fakeARM{t: t, eligibilities: []armclient.Eligibility{elig}, activated: []armclient.Assignment{stale}}
	f.install()
	entry.Status = recordRevoked
	writeRecord(testOwner("contoso"), []recordEntry{entry})
	out, _, err := runCmd(t, "list", "-c", "contoso", "--with-active", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []rowJSON
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Active {
		t.Fatalf("list resurrected deactivated role: %s", out)
	}
}

func assertListReconciled(t *testing.T, output, notices string) {
	t.Helper()
	if !strings.Contains(output, "?") {
		t.Errorf("local state lacks a confidence marker: %s", output)
	}
	if !strings.Contains(notices, "activated elsewhere") {
		t.Errorf("missing correction: %s", notices)
	}
	if len(readRecord(testOwner("contoso"))) != 1 {
		t.Error("list did not persist Azure's activation")
	}
}

func TestListKeepsRecordedRoleAtUnreadScope(t *testing.T) {
	srv := statusFake(t, "contoso-slow", false)
	installStatusFake(t, srv)
	shortDeadlines(t, 25*time.Millisecond)
	entry := mkRecordEntry("Owner", "contoso-slow", time.Hour)
	entry.RoleDefinitionID = "/providers/Microsoft.Authorization/roleDefinitions/8e3af657"
	entry.Key = recordKey(entry.Context, entry.Scope, entry.RoleDefinitionID)
	entry.Listed = true
	writeRecord(testOwner("contoso"), []recordEntry{entry})
	out, stderr, err := runCmd(t, "list", "-c", "contoso", "--with-active", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []rowJSON
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		if r.Scope == entry.Scope {
			found = true
			if !r.Active || r.Confirmed || r.State != "unconfirmed" {
				t.Fatalf("unread role lost its uncertainty: %+v", r)
			}
		}
	}
	if !found || !strings.Contains(stderr, "contoso-slow") {
		t.Fatalf("unread scope missing: %s / %s", out, stderr)
	}
	if len(readRecord(testOwner("contoso"))) != 1 {
		t.Fatal("unread role was removed from the record")
	}
}
