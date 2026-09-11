// Project status tests distinguish absent exact activations from incomplete
// Azure answers and broader roles. They do not assert application readiness.

package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/larsakerlund/pimctl/internal/armclient"
	"github.com/larsakerlund/pimctl/internal/cache"
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
