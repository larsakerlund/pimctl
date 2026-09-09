// Covers render.go and the tables built on it: that every command's output
// fits the console budget at its worst case, that a scope is never rendered
// ambiguously, that the plan table stays off stdout under -o json, and that
// picker labels are unique.

package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/larsakerlund/pimctl/internal/armclient"
)

// assertWidth enforces the spec's 120-column budget, counting runes rather than
// bytes so the ellipsis and bullet characters do not inflate the measurement.
func assertWidth(t *testing.T, out string) {
	t.Helper()
	for line := range strings.SplitSeq(out, "\n") {
		if n := len([]rune(line)); n > 120 {
			t.Errorf("line exceeds 120 columns (%d):\n%s", n, line)
		}
	}
}

// worstCaseRows uses the longest role name, scope name and scope id this tenant
// actually produces, plus the duplicated management-group name, so the width
// check is not passing on conveniently short fake data.
func worstCaseRows() []armclient.Eligibility {
	return []armclient.Eligibility{
		mkElig("Role Based Access Control Administrator", "f58310d9-a9f6-439a-9e8d-f62e7b41a168",
			"/subscriptions/55555555-5555-5555-5555-555555555555",
			"TEST - CONTOSO MONITORING - AZ - CONTOSO", "subscription"),
		mkElig(
			"Storage Blob Data Contributor",
			"ba92f5b4-2d11-453d-a403-e96b0029c9fe",
			"/providers/Microsoft.Management/managementGroups/contoso-prod",
			"Contoso landing zones",
			"managementgroup",
		),
		mkElig(
			"Storage Blob Data Contributor",
			"ba92f5b4-2d11-453d-a403-e96b0029c9fe",
			"/providers/Microsoft.Management/managementGroups/contoso-test",
			"Contoso landing zones",
			"managementgroup",
		),
	}
}

// TestListTableFitsInAConsoleAtWorstCase pins the spec's 120-column budget
// against the tenant's longest real values, including the ACTIVE column, which
// only appears once something is activated.
func TestListTableFitsInAConsoleAtWorstCase(t *testing.T) {
	elig := worstCaseRows()
	end := time.Now().Add(time.Hour)
	active := make([]armclient.Assignment, 0, len(elig))
	for _, e := range elig {
		a := armclient.Assignment{}
		a.Properties.AssignmentType = "Activated"
		a.Properties.Scope = e.Properties.Scope
		a.Properties.RoleDefinitionID = e.Properties.RoleDefinitionID
		a.Properties.EndDateTime = &end
		a.Properties.ExpandedProperties = e.Properties.ExpandedProperties
		active = append(active, a)
	}

	f := &fakeARM{t: t, eligibilities: elig, activated: active}
	f.install()

	// --with-active, so the ACTIVE column is actually filled: `ls` fills it
	// from the local record otherwise, and a table measured with that column
	// empty is not the widest table the command can print. The row that went
	// two columns over the budget in a real tenant was exactly this one.
	out, _, err := runCmd(t, "list", "-c", "contoso", "--with-active")
	if err != nil {
		t.Fatalf("list --with-active: %v", err)
	}
	if !strings.Contains(out, "until ") {
		t.Fatalf("the ACTIVE column is empty, so this measures the wrong table:\n%s", out)
	}
	assertWidth(t, out)

	out, _, err = runCmd(t, "list", "-c", "contoso")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	assertWidth(t, out)

	out, _, err = runCmd(t, "status", "-c", "contoso")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	assertWidth(t, out)
}

// TestActivateTablesNameTheScopeUnambiguously pins finding D: with three
// management groups sharing a display name, the confirmation and result tables
// must say which one — even when the selection only covers one of them.
func TestActivateTablesNameTheScopeUnambiguously(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: worstCaseRows()}
	f.install()

	// Select a single scope: the ambiguity is invisible within this selection,
	// but the tenant still has three "Contoso landing zones".
	out, _, err := runCmd(t, "activate", "-c", "contoso",
		"--role", "Storage Blob Data Contributor", "--scope", "contoso-test", "-j", "x", "-y")
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	if !strings.Contains(out, "contoso-test") {
		t.Errorf("the tables do not say which management group was elevated:\n%s", out)
	}
	if strings.Contains(out, "Contoso landing zones  ") && !strings.Contains(out, "(contoso-test)") {
		t.Errorf("bare ambiguous scope name in the tables:\n%s", out)
	}
	assertWidth(t, out)
}

// TestPlanTableStaysOffStdoutUnderJSON pins LOW-18.
func TestPlanTableStaysOffStdoutUnderJSON(t *testing.T) {
	cmd := NewRootCmd()
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)

	if w := planWriter(cmd, &globalOpts{output: outputJSON}); w != cmd.ErrOrStderr() {
		t.Error("under -o json the confirmation table must go to stderr so stdout stays parseable")
	}
	if w := planWriter(cmd, &globalOpts{output: "table"}); w != cmd.OutOrStdout() {
		t.Error("under -o table the confirmation table belongs on stdout")
	}
}

// TestAllTablesFitTheConsoleBudget sweeps every table with the tenant's longest
// real values plus the longest error detail pimctl can produce.
func TestAllTablesFitTheConsoleBudget(t *testing.T) {
	elig := worstCaseRows()
	f := &fakeARM{t: t, eligibilities: elig, maxDuration: "PT1H"}
	// The longest detail string in the codebase: the propagation explanation.
	f.setPutErr(func(string) (int, string) {
		return 400, `{"error":{"code":"RoleAssignmentDoesNotExist","message":"The Role assignment does not exist."}}`
	})
	f.install()

	// Plan + results, with a clamp note and a long failure detail. Every PUT is
	// stubbed to fail, so the command itself must report an error.
	out, _, err := runCmd(t, "activate", "-c", "contoso", "--all", "--hours", "9", "-j", "x", "-y")
	if err == nil {
		t.Fatal("activate should fail when every request is rejected")
	}
	assertWidth(t, out)
	if !strings.Contains(out, "capped to the policy maximum PT1H") {
		t.Errorf("the clamp note was lost when notes moved off the table:\n%s", out)
	}

	// Deactivate plan + results.
	end := time.Now().Add(time.Hour)
	active := make([]armclient.Assignment, 0, len(elig))
	for _, e := range elig {
		a := armclient.Assignment{}
		a.Properties.AssignmentType = "Activated"
		a.Properties.Scope = e.Properties.Scope
		a.Properties.RoleDefinitionID = e.Properties.RoleDefinitionID
		a.Properties.EndDateTime = &end
		a.Properties.ExpandedProperties = e.Properties.ExpandedProperties
		active = append(active, a)
	}
	f2 := &fakeARM{t: t, eligibilities: elig, activated: active, putStatus: "Revoked"}
	f2.install()
	out, _, err = runCmd(t, "deactivate", "-c", "contoso", "--all", "-y")
	if err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	assertWidth(t, out)
}

// TestMultiContextTablesFitToo: the CONTEXT column only appears when it earns
// its width, and the table still fits when it does.
func TestMultiContextTablesFitToo(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: worstCaseRows()}
	f.installContexts([]string{"contoso", "another-long-context"}, nil)

	out, _, err := runCmd(t, "list", "--all-contexts")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	assertWidth(t, out)
	if !strings.Contains(out, "CONTEXT") {
		t.Error("the CONTEXT column should appear when the rows span contexts")
	}
}

func TestSingleContextTableOmitsTheContextColumn(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()}
	f.install()
	out, _, err := runCmd(t, "list", "-c", "contoso")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "CONTEXT") {
		t.Errorf("the CONTEXT column wastes width when there is only one context:\n%s", out)
	}
}

// TestPickerLabelsAreUnique pins finding B's root cause: huh identifies an
// option by its label and toggles every option sharing one, so two rows with
// the same label would toggle together — selecting a scope the user never
// pointed at.
func TestPickerLabelsAreUnique(t *testing.T) {
	// Three management groups sharing a display name, as in the real tenant.
	rows := []row{
		mkRow(
			"contoso",
			"Contributor",
			contribGUID,
			"/providers/Microsoft.Management/managementGroups/contoso-prod",
			"Contoso landing zones",
			"managementgroup",
		),
		mkRow(
			"contoso",
			"Contributor",
			contribGUID,
			"/providers/Microsoft.Management/managementGroups/contoso-qa",
			"Contoso landing zones",
			"managementgroup",
		),
		mkRow(
			"contoso",
			"Contributor",
			contribGUID,
			"/providers/Microsoft.Management/managementGroups/contoso-test",
			"Contoso landing zones",
			"managementgroup",
		),
	}
	labels := itemLabels(rows, false, scopeLabelerForRows(rows))
	seen := map[string]bool{}
	for i, l := range labels {
		if seen[l] {
			t.Fatalf("label %d is a duplicate (%q) — huh would toggle both rows at once", i, l)
		}
		seen[l] = true
	}
	for _, want := range []string{"contoso-prod", "contoso-qa", "contoso-test"} {
		found := false
		for _, l := range labels {
			if strings.Contains(l, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("no label identifies %s: %v", want, labels)
		}
	}

	// Even when the scope cannot disambiguate them, labels must stay unique.
	same := []row{
		mkRow("contoso", "Contributor", contribGUID, mgScope, "Same", "managementgroup"),
		mkRow("contoso", "Contributor", contribGUID, mgScope, "Same", "managementgroup"),
	}
	labels = itemLabels(same, false, scopeLabelerForRows(same))
	if labels[0] == labels[1] {
		t.Fatalf("identical rows produced identical labels: %q", labels[0])
	}
}

// TestDeactivateTablesIdentifyTheManagementGroup pins finding (2).
func TestDeactivateTablesIdentifyTheManagementGroup(t *testing.T) {
	end := time.Now().Add(time.Hour)
	active := make([]armclient.Assignment, 0, 1)
	for _, leaf := range []string{"contoso-test"} {
		a := armclient.Assignment{ID: "/" + leaf}
		a.Properties.AssignmentType = "Activated"
		a.Properties.Scope = "/providers/Microsoft.Management/managementGroups/" + leaf
		a.Properties.RoleDefinitionID = "/providers/Microsoft.Authorization/roleDefinitions/434105ed"
		a.Properties.EndDateTime = &end
		a.Properties.ExpandedProperties.RoleDefinition = armclient.Named{DisplayName: "Cost Management Contributor"}
		a.Properties.ExpandedProperties.Scope = armclient.Named{
			DisplayName: "Contoso landing zones",
			Type:        "managementgroup",
		}
		active = append(active, a)
	}
	f := &fakeARM{t: t, activated: active, putStatus: "Revoked"}
	f.install()

	// Only one row on screen, so nothing "collides" — the old labeler would
	// have printed a bare "Contoso landing zones".
	out, _, err := runCmd(t, "status", "-c", "contoso")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "contoso-test") {
		t.Errorf("status must say which management group:\n%s", out)
	}

	out, _, err = runCmd(t, "down", "-c", "contoso", "-y")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "contoso-test") {
		t.Errorf("down must say which management group:\n%s", out)
	}
}
