// Tests for scope labelling: the display name three management groups share,
// the leaf id that tells them apart, and the collisions truncation invents
// that the labeler alone cannot see.

package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/larsakerlund/pimctl/internal/armclient"
)

// TestManagementGroupsAlwaysCarryTheirName pins the rule that a management
// group is identified even when nothing on screen collides with it — which is
// what made `status` and `deactivate` unable to tell contoso-test from contoso-prod.
func TestManagementGroupsAlwaysCarryTheirName(t *testing.T) {
	only := newScopeLabeler([]scopeRef{
		{Name: "Contoso landing zones", ID: "/providers/Microsoft.Management/managementGroups/contoso-test"},
	})
	got := only.Label("Contoso landing zones", "/providers/Microsoft.Management/managementGroups/contoso-test")
	if got != "Contoso landing zones (contoso-test)" {
		t.Errorf("a lone management group must still be identified, got %q", got)
	}

	// A lone subscription stays clean: its name is normally unique and the id
	// is noise.
	sub := newScopeLabeler([]scopeRef{
		{Name: "Contoso QA", ID: "/subscriptions/22222222-0000-0000-0000-0000000000023"},
	})
	if got := sub.Label("Contoso QA", "/subscriptions/22222222-0000-0000-0000-0000000000023"); got != "Contoso QA" {
		t.Errorf("an unambiguous subscription should not grow an id, got %q", got)
	}
}

// TestScopeLabelerDisambiguates pins the rule that decides whether a scope's
// leaf id is appended to its display name: a name covering more than one real
// scope always gets one, a name repeated across rows that all point at the same
// scope never does.
func TestScopeLabelerDisambiguates(t *testing.T) {
	// Three different management groups really do share the display name
	// "Contoso landing zones" in this tenant, so the label must carry the leaf id.
	refs := []scopeRef{
		{Name: "Contoso landing zones", ID: "/providers/Microsoft.Management/managementGroups/contoso-prod"},
		{Name: "Contoso landing zones", ID: "/providers/Microsoft.Management/managementGroups/contoso-qa"},
		{Name: "Contoso landing zones", ID: "/providers/Microsoft.Management/managementGroups/contoso-test"},
		{Name: "Contoso Identity", ID: "/subscriptions/66666666-6666-6666-6666-666666666666"},
	}
	labeler := newScopeLabeler(refs)
	want := []string{
		"Contoso landing zones (contoso-prod)",
		"Contoso landing zones (contoso-qa)",
		"Contoso landing zones (contoso-test)",
		"Contoso Identity",
	}
	for i, ref := range refs {
		if got := labeler.Label(ref.Name, ref.ID); got != want[i] {
			t.Errorf("label %d = %q, want %q", i, got, want[i])
		}
	}

	// The same scope repeated across several roles is not ambiguous: one
	// subscription with three eligible roles must not grow an id suffix.
	refs = []scopeRef{
		{Name: "Contoso QA", ID: "/subscriptions/22222222-0000-0000-0000-0000000000023"},
		{Name: "Contoso QA", ID: "/subscriptions/22222222-0000-0000-0000-0000000000023"},
		{Name: "Contoso QA", ID: "/subscriptions/22222222-0000-0000-0000-0000000000023"},
	}
	labeler = newScopeLabeler(refs)
	for i, ref := range refs {
		if label := labeler.Label(ref.Name, ref.ID); label != "Contoso QA" {
			t.Errorf("label %d = %q; one scope repeated across roles is not ambiguous", i, label)
		}
	}

	// Duplicated subscription names get the shortened subscription id.
	refs = []scopeRef{
		{Name: "Shared", ID: "/subscriptions/66666666-6666-6666-6666-666666666666"},
		{Name: "Shared", ID: "/subscriptions/44444444-4444-4444-4444-444444444444"},
	}
	labeler = newScopeLabeler(refs)
	if got := []string{
		labeler.Label(refs[0].Name, refs[0].ID),
		labeler.Label(refs[1].Name, refs[1].ID),
	}; got[0] != "Shared (66666666)" || got[1] != "Shared (44444444)" {
		t.Errorf("subscription labels = %v", got)
	}
}

func TestScopeLeaf(t *testing.T) {
	cases := map[string]string{
		"/providers/Microsoft.Management/managementGroups/contoso-prod": "contoso-prod",
		"/subscriptions/66666666-6666-6666-6666-666666666666":           "66666666",
		"/subscriptions/abc/resourceGroups/rg-prod":                     "rg-prod",
		"": "",
	}
	for in, want := range cases {
		if got := scopeLeaf(in); got != want {
			t.Errorf("ScopeLeaf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTruncatedScopeCellsDisambiguateAfterTruncation(t *testing.T) {
	// Two distinct subscriptions whose names differ only past the cut.
	rows := []row{
		mkRow("contoso", "Owner", "o", "/subscriptions/11111111-1111-1111-1111-111111111111",
			"DEV - AGENTIC AI PLATFORM - AZ - CONTOSO", "subscription"),
		mkRow("contoso", "Owner", "o", "/subscriptions/22222222-2222-2222-2222-222222222222",
			"DEV - AGENTIC ALERTS PLATFORM - AZ - CONTOSO", "subscription"),
	}
	scopes := scopeLabelerForRows(rows)
	cells := truncatedScopeCells(rows, scopes, 28)
	if cells[0] == cells[1] {
		t.Fatalf("two different subscriptions rendered identically: %q", cells[0])
	}
	for i, c := range cells {
		if len([]rune(c)) > 28 {
			t.Errorf("cell %d is %d runes, over the column width", i, len([]rune(c)))
		}
	}

	// Rows that are genuinely the same scope must not grow a suffix.
	same := []row{rows[0], rows[0]}
	cells = truncatedScopeCells(same, scopeLabelerForRows(same), 28)
	if strings.Contains(cells[0], "(") {
		t.Errorf("one scope repeated should not be disambiguated: %q", cells[0])
	}
}

func TestItemLabel(t *testing.T) {
	elig := mkRow(
		"contoso",
		"Cost Management Contributor",
		costGUID,
		mgScope,
		"Contoso landing zones",
		"managementgroup",
	)
	plain := newScopeLabeler(nil)

	// A management group always carries its own name: display names are reused
	// across the hierarchy, so "Contoso landing zones" alone never identifies one.
	got := itemLabel(elig, false, plain)
	want := "Cost Management Contributor @ Contoso landing zones (contoso-prod) (ManagementGroup)"
	if got != want {
		t.Errorf("single-context label:\n got  %q\n want %q", got, want)
	}

	got = itemLabel(elig, true, plain)
	if !strings.HasPrefix(got, "[contoso] ") {
		t.Errorf("multi-context label must lead with the context: %q", got)
	}

	// An already-active role stays selectable but is marked, with its end time.
	end := time.Date(2026, 9, 4, 15, 30, 0, 0, time.Local)
	elig.Active = &armclient.Assignment{}
	elig.Active.Properties.AssignmentType = "Activated"
	elig.Active.Properties.EndDateTime = &end
	got = itemLabel(elig, false, plain)
	// The full date, not a bare "15:30": one format everywhere, and a role that
	// expires after midnight must not read as if it expires this afternoon.
	if !strings.Contains(got, "• ACTIVE until 2026-09-04 15:30") {
		t.Errorf("active marker missing or misformatted: %q", got)
	}
	if !strings.HasPrefix(got, "Cost Management Contributor") {
		t.Errorf("role name must stay at the front so type-to-filter works: %q", got)
	}

	// Every field a user might filter on has to be on the line.
	for _, needle := range []string{"Cost Management Contributor", "Contoso landing zones", "ManagementGroup"} {
		if !strings.Contains(got, needle) {
			t.Errorf("label %q is missing filterable text %q", got, needle)
		}
	}
}
