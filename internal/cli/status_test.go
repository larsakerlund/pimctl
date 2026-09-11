// Tests for status.go and statusview.go: the fast local answer, the blocking
// one, and the difference between a scope that reported nothing and a scope
// that did not report. The record's own lag behaviour has its own file,
// reconcile_test.go; the fake ARM is in fake_test.go.

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/larsakerlund/pimctl/internal/armclient"
	"github.com/larsakerlund/pimctl/internal/azauth"
)

func TestStatusEmpty(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()}
	f.install()
	out, _, err := runCmd(t, "status", "-c", "contoso")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No roles are currently activated.") {
		t.Errorf("status output:\n%s", out)
	}
}

// TestStatusRendersFromTheRecordWithoutWaiting: the same guarantee for status,
// which previously always paid the per-scope fan-out.
func TestStatusRendersFromTheRecordWithoutWaiting(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv(envContext, "")
	installFakeRunner(t, []string{"contoso"})

	end := time.Now().Add(time.Hour)
	entry := recordEntry{
		Context:          "contoso",
		Scope:            "/providers/Microsoft.Management/managementGroups/contoso-prod",
		RoleDefinitionID: "/providers/Microsoft.Authorization/roleDefinitions/" + costGUID,
		Role:             "Cost Management Contributor",
		ScopeName:        "Contoso landing zones",
		ScopeType:        "ManagementGroup",
		Start:            time.Now(),
		End:              end,
		Status:           string(OutcomeActivated),
	}
	entry.Key = recordKey(entry.Context, entry.Scope, entry.RoleDefinitionID)
	writeRecord("contoso", []recordEntry{entry})

	rows := localActiveRows(&runContext{Sessions: []*session{{
		Context: "contoso",
		Token:   &azauth.Token{Context: "contoso", TenantID: "tid-1", PrincipalID: "oid-1"},
	}}})
	if len(rows) != 1 {
		t.Fatalf("the record produced %d rows, want 1", len(rows))
	}
	if !rows[0].Unconfirmed() {
		t.Error("a row from the local record must not be marked confirmed")
	}
	if rows[0].Assignment.RoleName() != "Cost Management Contributor" {
		t.Errorf("row = %+v", rows[0].Assignment)
	}
	if rows[0].Assignment.ScopeType() != "ManagementGroup" {
		t.Errorf("scope type lost: %q", rows[0].Assignment.ScopeType())
	}
}

// TestStatusUnconfirmedRowsAreMarkedInTheTable: a fast answer must never be
// mistaken for an authoritative one.
func TestStatusUnconfirmedRowsAreMarkedInTheTable(t *testing.T) {
	end := time.Now().Add(time.Hour)
	a := armclient.Assignment{ID: "/x"}
	a.Properties.AssignmentType = "Activated"
	a.Properties.Scope = "/providers/Microsoft.Management/managementGroups/contoso-prod"
	a.Properties.RoleDefinitionID = "/providers/Microsoft.Authorization/roleDefinitions/" + costGUID
	a.Properties.EndDateTime = &end
	a.Properties.ExpandedProperties.RoleDefinition = armclient.Named{DisplayName: "Cost Management Contributor"}
	a.Properties.ExpandedProperties.Scope = armclient.Named{
		DisplayName: "Contoso landing zones",
		Type:        "managementgroup",
	}

	cmd := NewRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	printActiveTable(cmd, []activeRow{{Context: "contoso", Assignment: a, State: RowUnconfirmed}})

	if !strings.Contains(out.String(), "?") {
		t.Errorf("an unconfirmed row must carry a marker:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Azure did not answer for that scope") {
		t.Errorf("the legend is missing:\n%s", out.String())
	}

	// A confirmed row carries neither.
	out.Reset()
	printActiveTable(cmd, []activeRow{{Context: "contoso", Assignment: a}})
	if strings.Contains(out.String(), "Azure did not answer") {
		t.Errorf("a confirmed table should not carry the legend:\n%s", out.String())
	}
}

func TestStatusFastAndWaitAreMutuallyExclusive(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()}
	f.install()
	if _, _, err := runCmd(t, "status", "-c", "contoso", "--fast", "--wait"); err == nil {
		t.Fatal("--fast and --wait ask for opposite things and should be rejected")
	}
}

// statusFake serves one answering scope and one that never replies, with a role
// held at the scope that stalls.
func statusFake(t *testing.T, slowLeaf string, throttle bool) *httptest.Server {
	t.Helper()
	fastScope := "/providers/Microsoft.Management/managementGroups/contoso-prod"
	slowScope := "/providers/Microsoft.Management/managementGroups/" + slowLeaf
	elig := []armclient.Eligibility{
		mkElig("Cost Management Contributor", costGUID, fastScope, "Contoso landing zones", "managementgroup"),
		mkElig("Owner", "8e3af657", slowScope, "Contoso landing zones", "managementgroup"),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/roleEligibilityScheduleInstances"):
			writeJSON(t, w, map[string]any{"value": elig})
		case strings.HasSuffix(p, "/roleAssignmentScheduleInstances"):
			scope := strings.TrimSuffix(p, "/providers/Microsoft.Authorization/roleAssignmentScheduleInstances")
			if strings.HasSuffix(scope, slowLeaf) {
				if throttle {
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(http.StatusTooManyRequests)
					fmt.Fprint(w, `{"error":{"code":"TooManyRequests","message":"slow down"}}`)
					return
				}
				<-r.Context().Done()
				return
			}
			writeJSON(t, w, map[string]any{"value": []any{}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func installStatusFake(t *testing.T, srv *httptest.Server) {
	t.Helper()
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
}

// TestUnconfirmedScopeKeepsItsRole is the fix for the observed data loss: ten
// consecutive runs returned 2,2,0,0,1,0,2,1,2,2 rows against a ground truth of
// 2, and one printed "No roles are currently activated." while naming the scope
// it had failed to read.
func TestUnconfirmedScopeKeepsItsRole(t *testing.T) {
	srv := statusFake(t, "contoso-slow", false)
	installStatusFake(t, srv)
	shortDeadlines(t, 150*time.Millisecond)

	// The record knows about a role at the scope that will stall.
	end := time.Now().Add(time.Hour)
	entry := recordEntry{
		Context:          "contoso",
		Scope:            "/providers/Microsoft.Management/managementGroups/contoso-slow",
		RoleDefinitionID: "/providers/Microsoft.Authorization/roleDefinitions/8e3af657",
		Role:             "Owner", ScopeName: "Contoso landing zones", ScopeType: "ManagementGroup",
		End: end,
	}
	entry.Key = recordKey(entry.Context, entry.Scope, entry.RoleDefinitionID)
	writeRecord("contoso", []recordEntry{entry})

	out, errOut, err := runCmd(t, "status", "-c", "contoso")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if strings.Contains(out, "No roles are currently activated") {
		t.Fatalf("status reported an empty tenant while a scope was unread:\n%s", out)
	}
	if !strings.Contains(out, "Owner") {
		t.Errorf("the role at the unconfirmed scope was dropped:\n%s", out)
	}
	if !strings.Contains(out, "?") {
		t.Errorf("the surviving row must be marked unconfirmed:\n%s", out)
	}
	if !strings.Contains(errOut, "contoso-slow") {
		t.Errorf("the unread scope must be named: %q", errOut)
	}
	if strings.Contains(errOut, "no longer held") {
		t.Errorf("a scope that was never read must not be reported as a loss: %q", errOut)
	}
}

// TestThrottledScopeIsUnconfirmedNotEmpty: ARM refusing to answer is not ARM
// answering "nothing".
func TestThrottledScopeIsUnconfirmedNotEmpty(t *testing.T) {
	srv := statusFake(t, "contoso-throttled", true)
	installStatusFake(t, srv)
	shortDeadlines(t, 2*time.Second)

	end := time.Now().Add(time.Hour)
	entry := recordEntry{
		Context:          "contoso",
		Scope:            "/providers/Microsoft.Management/managementGroups/contoso-throttled",
		RoleDefinitionID: "/providers/Microsoft.Authorization/roleDefinitions/8e3af657",
		Role:             "Owner", ScopeName: "Contoso landing zones", ScopeType: "ManagementGroup",
		End: end,
	}
	entry.Key = recordKey(entry.Context, entry.Scope, entry.RoleDefinitionID)
	writeRecord("contoso", []recordEntry{entry})

	out, errOut, err := runCmd(t, "status", "-c", "contoso")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(errOut, "contoso-throttled") {
		t.Errorf("a throttled scope must be reported as unconfirmed: %q", errOut)
	}
	if !strings.Contains(out, "Owner") {
		t.Errorf("the role at the throttled scope was dropped:\n%s", out)
	}
	if strings.Contains(errOut, "no longer held") {
		t.Errorf("a throttled scope must not be counted as a loss: %q", errOut)
	}
}

// TestStatusJSONCarriesUnconfirmedScopes: a parser must be able to tell an
// empty tenant from a partial read.
func TestStatusJSONCarriesUnconfirmedScopes(t *testing.T) {
	srv := statusFake(t, "contoso-slow", false)
	installStatusFake(t, srv)
	shortDeadlines(t, 150*time.Millisecond)

	end := time.Now().Add(time.Hour)
	entry := recordEntry{
		Context:          "contoso",
		Scope:            "/providers/Microsoft.Management/managementGroups/contoso-slow",
		RoleDefinitionID: "/providers/Microsoft.Authorization/roleDefinitions/8e3af657",
		Role:             "Owner", ScopeName: "Contoso landing zones", ScopeType: "ManagementGroup",
		End: end,
	}
	entry.Key = recordKey(entry.Context, entry.Scope, entry.RoleDefinitionID)
	writeRecord("contoso", []recordEntry{entry})

	out, _, err := runCmd(t, "status", "-c", "contoso", "-o", "json")
	if err != nil {
		t.Fatalf("status -o json: %v", err)
	}
	var doc struct {
		Roles             []statusRoleJSON `json:"roles"`
		UnconfirmedScopes []string         `json:"unconfirmedScopes"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("output is not the documented envelope: %v\n%s", err, out)
	}
	if len(doc.UnconfirmedScopes) == 0 {
		t.Errorf("unconfirmedScopes is empty though a scope was unread:\n%s", out)
	}
	if len(doc.Roles) == 0 {
		t.Fatalf("the role at the unconfirmed scope was dropped from JSON:\n%s", out)
	}
	checkRoleStates(t, doc.Roles, out)
}

// statusRoleJSON is the part of a `status -o json` row the shape tests read.
type statusRoleJSON struct {
	Role      string `json:"role"`
	Confirmed bool   `json:"confirmed"`
	State     string `json:"state"`
}

// checkRoleStates asserts the per-row half of the envelope: every row says
// which of the three states it is in, and never contradicts itself. A caller
// must be able to tell a confirmed row from a guess without inferring it from
// unconfirmedScopes.
func checkRoleStates(t *testing.T, roles []statusRoleJSON, out string) {
	t.Helper()
	var sawUnconfirmed, sawConfirmed bool
	for _, r := range roles {
		if r.Confirmed != (r.State == "confirmed") {
			t.Errorf("confirmed and state disagree for %q: %+v", r.Role, r)
		}
		switch {
		case r.Role == "Owner" && r.State == "unconfirmed":
			sawUnconfirmed = true
		case r.Confirmed:
			sawConfirmed = true
		}
	}
	if !sawUnconfirmed {
		t.Errorf("the row from the record must carry confirmed:false and state:unconfirmed:\n%s", out)
	}
	if len(roles) > 1 && !sawConfirmed {
		t.Errorf("no row was reported as confirmed:\n%s", out)
	}
}

// TestStatusJSONCarriesTheDisambiguatedScopeLabel: scopeName is the bare
// display name Azure returns, and several management groups share one. The
// table has always printed the group's own name alongside it; scopeLabel is
// that same string, so a caller does not have to rebuild it from scope.
func TestStatusJSONCarriesTheDisambiguatedScopeLabel(t *testing.T) {
	elig, active, _ := lagFixtures(t)
	f := &fakeARM{
		t: t, eligibilities: []armclient.Eligibility{elig},
		activated: []armclient.Assignment{active},
	}
	f.install()

	out, _, err := runCmd(t, "status", "-c", "contoso", "-o", "json")
	if err != nil {
		t.Fatalf("status -o json: %v", err)
	}
	var doc struct {
		Roles []struct {
			ScopeName  string `json:"scopeName"`
			ScopeLabel string `json:"scopeLabel"`
		} `json:"roles"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if len(doc.Roles) != 1 {
		t.Fatalf("got %d roles, want 1:\n%s", len(doc.Roles), out)
	}
	if doc.Roles[0].ScopeName != "Contoso landing zones" {
		t.Errorf("scopeName changed: %q", doc.Roles[0].ScopeName)
	}
	want := "Contoso landing zones (" + scopeLeaf(lagScope) + ")"
	if doc.Roles[0].ScopeLabel != want {
		t.Errorf("scopeLabel = %q, want %q — the table's own wording:\n%s", doc.Roles[0].ScopeLabel, want, out)
	}
}

// TestStatusJSONCarriesKeysAndUntil: a script must be able to read a row from
// `status -o json` and hand it straight to `down --key`, and one name for the
// end time must work across every command.
func TestStatusJSONCarriesKeysAndUntil(t *testing.T) {
	elig, active, _ := lagFixtures(t)
	f := &fakeARM{
		t: t, eligibilities: []armclient.Eligibility{elig},
		activated: []armclient.Assignment{active},
	}
	f.install()

	out, _, err := runCmd(t, "status", "-c", "contoso", "-o", "json")
	if err != nil {
		t.Fatalf("status -o json: %v", err)
	}
	var doc struct {
		Roles []struct {
			Key   string     `json:"key"`
			Until *time.Time `json:"until"`
			End   *time.Time `json:"endDateTime"`
		} `json:"roles"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if len(doc.Roles) != 1 {
		t.Fatalf("got %d roles, want 1:\n%s", len(doc.Roles), out)
	}
	if doc.Roles[0].Key == "" {
		t.Errorf("a status row must carry the key `down --key` takes:\n%s", out)
	}
	if doc.Roles[0].Until == nil {
		t.Errorf("the end time must be called `until`, as everywhere else:\n%s", out)
	}
	if doc.Roles[0].End != nil {
		t.Errorf("endDateTime should be gone, replaced by until:\n%s", out)
	}
}

// deltaRow builds one activation row for the delta tests.
func deltaRow(role, leaf string) activeRow {
	scope := "/providers/Microsoft.Management/managementGroups/" + leaf
	a := armclient.Assignment{ID: "/instances/" + role + leaf}
	a.Properties.AssignmentType = "Activated"
	a.Properties.Scope = scope
	a.Properties.RoleDefinitionID = scope + "/providers/Microsoft.Authorization/roleDefinitions/" + costGUID
	a.Properties.ExpandedProperties.RoleDefinition = armclient.Named{DisplayName: role}
	a.Properties.ExpandedProperties.Scope = armclient.Named{
		DisplayName: "Contoso landing zones", Type: "managementgroup", ID: scope,
	}
	return activeRow{Context: "contoso", Assignment: a}
}

// TestReportStatusDelta covers the printer behind "nothing is ever dropped in
// silence": what it says when Azure agrees, when it knows about a role this
// machine did not, and when a role this machine recorded is gone.
func TestReportStatusDelta(t *testing.T) {
	held := deltaRow("Cost Management Contributor", "contoso-prod")
	elsewhere := deltaRow("Owner", "contoso-qa")

	cases := []struct {
		name        string
		local       localRecord
		active      []activeRow
		unconfirmed []activationScope
		want        []string
		absent      []string
	}{
		{
			name:   "agreement is one quiet line",
			local:  localRecord{rows: []activeRow{held}},
			active: []activeRow{held},
			want:   []string{"confirmed 1 against Azure"},
			absent: []string{"no longer held", "activated elsewhere", "again for the corrected table"},
		},
		{
			name:   "a role activated in the portal is named",
			local:  localRecord{},
			active: []activeRow{elsewhere},
			want:   []string{"+1 activated elsewhere", "Owner @ Contoso landing zones (contoso-qa)"},
			absent: []string{"no longer held"},
		},
		{
			name:   "a role that is gone is named, in the one wording",
			local:  localRecord{rows: []activeRow{held}},
			active: nil,
			want: []string{
				"1 role(s) this machine had activated are no longer held",
				"Cost Management Contributor @ Contoso landing zones (contoso-prod)",
				"run `pimctl status` again for the corrected table",
			},
		},
		{
			name:        "an unread scope is not a loss",
			local:       localRecord{rows: []activeRow{held}},
			active:      nil,
			unconfirmed: []activationScope{{Context: held.Context, ID: held.Assignment.Properties.Scope}},
			want:        []string{"unconfirmed (slow ARM)", "Contoso landing zones (contoso-prod)"},
			absent:      []string{"no longer held"},
		},
		{
			name: "a role given up here is not someone else's activation",
			local: localRecord{
				revoked: map[string]recordEntry{
					activeSelectionKey(held): {Status: recordRevoked, WrittenAt: time.Now()},
				},
			},
			active: []activeRow{held},
			want:   []string{"confirmed 0 against Azure"},
			absent: []string{"activated elsewhere", "no longer held"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := NewRootCmd()
			var errOut bytes.Buffer
			cmd.SetOut(io.Discard)
			cmd.SetErr(&errOut)
			rc := &runContext{}
			for _, r := range append(tc.active, tc.local.rows...) {
				rc.names.learn(r.Assignment.Properties.Scope, r.Assignment.ScopeName())
			}

			reportStatusDelta(cmd, rc, tc.local, tc.active, tc.unconfirmed)

			for _, want := range tc.want {
				if !strings.Contains(errOut.String(), want) {
					t.Errorf("output is missing %q:\n%s", want, errOut.String())
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(errOut.String(), absent) {
					t.Errorf("output should not contain %q:\n%s", absent, errOut.String())
				}
			}
			// The delta is advice about the table, so it never lands on stdout
			// where a `-o json` consumer would have to parse around it.
			if errOut.Len() == 0 {
				t.Error("the delta printed nothing at all")
			}
		})
	}
}
