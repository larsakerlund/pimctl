// Project status tests distinguish absent exact activations from incomplete
// Azure answers and broader roles. They do not assert application readiness.

package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/larsakerlund/pimctl/internal/armclient"
	"github.com/larsakerlund/pimctl/internal/azauth"
	"github.com/larsakerlund/pimctl/internal/cache"
	"github.com/larsakerlund/pimctl/internal/config"
	"github.com/larsakerlund/pimctl/internal/store"
)

func TestProjectStatusReportsEveryMissingRequirement(t *testing.T) {
	f := &fakeARM{t: t}
	path := installProject(t, f)
	writeProject(t, path)
	out, stderr, err := runCmd(t, "status", "--project", "-c", "contoso", "-o", "json")
	if err != nil {
		t.Fatalf("status: %v\n%s\n%s", err, out, stderr)
	}
	if !strings.Contains(out, "not active") || !strings.Contains(out, testProjectScope) {
		t.Fatalf("missing requirements omitted: %s", out)
	}
}

func TestProjectStatusBroaderRoleDoesNotSatisfyExactTarget(t *testing.T) {
	a := armclient.Assignment{}
	a.Properties.Scope = testProjectSubscription
	a.Properties.RoleDefinitionID = testProjectSubscription + "/providers/Microsoft.Authorization/roleDefinitions/" + testProjectRole
	a.Properties.AssignmentType = "Activated"
	f := &fakeARM{t: t, activated: []armclient.Assignment{a}}
	path := installProject(t, f)
	writeProject(t, path)
	out, stderr, err := runCmd(t, "status", "--project", "-c", "contoso")
	if err != nil {
		t.Fatalf("status: %v\n%s\n%s", err, out, stderr)
	}
	if !strings.Contains(out, "not active") || !strings.Contains(stderr, "Broader activation") {
		t.Fatalf("broader role misrepresented: %s\n%s", out, stderr)
	}
}

func TestProjectStatusSlowScopeIsUnknownAndFails(t *testing.T) {
	f := &fakeARM{t: t, activeDelay: 30 * time.Millisecond}
	path := installProject(t, f)
	writeProject(t, path)
	tm := defaultTimeouts()
	tm.scopeSoftDeadline = time.Millisecond
	tm.waitScope = time.Millisecond
	installTimeouts(t, tm)
	out, _, err := runCmd(t, "status", "--project", "-c", "contoso", "-o", "json")
	if err == nil || !strings.Contains(out, `"state": "unknown"`) || strings.Contains(out, "not active") {
		t.Fatalf("slow scope: %v\n%s", err, out)
	}
}

func TestProjectStatusReportsKnownManagementGroupAncestorInJSONMode(t *testing.T) {
	parent := "/providers/Microsoft.Management/managementGroups/platform"
	a := armclient.Assignment{
		Properties: armclient.AssignmentProperties{
			Scope:            parent,
			RoleDefinitionID: parent + "/providers/Microsoft.Authorization/roleDefinitions/" + testProjectRole,
			AssignmentType:   "Activated",
		},
	}
	f := &fakeARM{t: t, activated: []armclient.Assignment{a}}
	path := installProject(t, f)
	writeProject(t, path)
	e := mkElig("Reader", testProjectRole, parent, "Platform", "ManagementGroup")
	cache.WriteScoped(
		store.Owner{Context: "contoso", TenantID: testProjectTenant, PrincipalID: "oid-1"},
		testProjectScope,
		[]armclient.Eligibility{e},
	)
	out, stderr, err := runCmd(t, "status", "--project", "-c", "contoso", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "Broader activation") || !strings.Contains(stderr, parent) ||
		!strings.Contains(out, "not active") {
		t.Fatalf("ancestor omitted or treated as exact target: %s\n%s", out, stderr)
	}
}

// projectDeltaRow is the exact activation a one-role test project requires,
// ending at until and in the given state.
func projectDeltaRow(until time.Time, state rowState) activeRow {
	a := armclient.Assignment{ID: "/instances/dev-reader"}
	a.Properties.AssignmentType = "Activated"
	a.Properties.Scope = testProjectScope
	a.Properties.RoleDefinitionID = testProjectScope + "/providers/Microsoft.Authorization/roleDefinitions/" + testProjectRole
	a.Properties.EndDateTime = &until
	a.Properties.ExpandedProperties.RoleDefinition = armclient.Named{DisplayName: "Reader"}
	a.Properties.ExpandedProperties.Scope = armclient.Named{
		DisplayName: "dev",
		Type:        "resourcegroup",
		ID:          testProjectScope,
	}
	return activeRow{Context: "contoso", Assignment: a, State: state}
}

// TestReportProjectDeltaNamesExpiryChanges: after the instant table, the
// reconciliation says one line per requirement whose state or expiry moved and
// nothing for one that stayed put — the same expiry through a different
// pointer is not a change.
func TestReportProjectDeltaNamesExpiryChanges(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	p := &config.Project{
		Tenant: testProjectTenant,
		Roles:  []config.ProjectRole{{RoleDefinitionID: testProjectRole, Scope: testProjectScope}},
	}
	s := &session{
		Context: "contoso",
		Token:   &azauth.Token{Context: "contoso", TenantID: testProjectTenant, PrincipalID: "oid-1"},
	}
	t1 := time.Now().Add(time.Hour).Round(time.Second)
	t2 := t1.Add(time.Hour)
	t1Again := t1.In(time.UTC)

	cases := []struct {
		name   string
		before []activeRow
		after  []activeRow
		want   []string
		silent bool
	}{
		{
			name:  "a requirement that became active",
			after: []activeRow{projectDeltaRow(t1, RowConfirmed)},
			want:  []string{"Project status: Reader at " + testProjectScope, "· active ·", "remaining"},
		},
		{
			name:   "an expiry that moved",
			before: []activeRow{projectDeltaRow(t1, RowConfirmed)},
			after:  []activeRow{projectDeltaRow(t2, RowConfirmed)},
			want:   []string{"Project status: Reader at " + testProjectScope, "· active ·"},
		},
		{
			name:   "a record row Azure then confirmed",
			before: []activeRow{projectDeltaRow(t1, RowUnconfirmed)},
			after:  []activeRow{projectDeltaRow(t1, RowConfirmed)},
			want:   []string{"· active ·"},
		},
		{
			name:   "a requirement that went away",
			before: []activeRow{projectDeltaRow(t1, RowConfirmed)},
			want:   []string{"· not active ·"},
		},
		{
			name:   "the same expiry through another pointer",
			before: []activeRow{projectDeltaRow(t1, RowConfirmed)},
			after:  []activeRow{projectDeltaRow(t1Again, RowConfirmed)},
			silent: true,
		},
		{
			name:   "nothing held before or after",
			silent: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := NewRootCmd()
			var errOut bytes.Buffer
			cmd.SetOut(io.Discard)
			cmd.SetErr(&errOut)

			reportProjectDelta(cmd, p, s, tc.before, nil, tc.after, nil)

			got := errOut.String()
			if tc.silent {
				if got != "" {
					t.Errorf("an unchanged requirement must print nothing, got:\n%s", got)
				}
				return
			}
			if strings.Count(got, "Project status:") != 1 {
				t.Errorf("want exactly one line for the one requirement, got:\n%s", got)
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("output is missing %q:\n%s", want, got)
				}
			}
		})
	}
}

// TestEqualProjectExpiry pins the comparison behind the delta: instants, not
// pointers, and nil only equal to nil.
func TestEqualProjectExpiry(t *testing.T) {
	now := time.Now().Round(time.Second)
	later := now.Add(time.Second)
	sameElsewhere := now.In(time.UTC)
	cases := []struct {
		name string
		a, b *time.Time
		want bool
	}{
		{"both nil", nil, nil, true},
		{"nil against a time", nil, &now, false},
		{"a time against nil", &now, nil, false},
		{"the same pointer", &now, &now, true},
		{"two pointers to one instant", &now, &sameElsewhere, true},
		{"one second apart", &now, &later, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := equalProjectExpiry(tc.a, tc.b); got != tc.want {
				t.Errorf("equalProjectExpiry() = %v, want %v", got, tc.want)
			}
			if got := equalProjectExpiry(tc.b, tc.a); got != tc.want {
				t.Errorf("equalProjectExpiry() is not symmetric: reversed = %v, want %v", got, tc.want)
			}
		})
	}
}
