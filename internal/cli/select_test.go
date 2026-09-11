// Tests for --role, --scope and --key resolution, on rows shaped like the real
// tenant where "Contributor" is a substring of four different role names. The
// interactive picker is not tested here; it needs a pty.

package cli

import (
	"strings"
	"testing"
)

// tenantShapedRows mirrors the real blast-radius problem: "Contributor" is a
// substring of three other role names.
func tenantShapedRows() []row {
	mk := func(role, guid, leaf string) row {
		return mkRow(
			"contoso",
			role,
			guid,
			"/providers/Microsoft.Management/managementGroups/"+leaf,
			"Contoso landing zones",
			"managementgroup",
		)
	}
	return []row{
		mk("Contributor", contribGUID, "contoso-prod"),
		mk("Contributor", contribGUID, "contoso-qa"),
		mk("Cost Management Contributor", costGUID, "contoso-prod"),
		mk("Resource Policy Contributor", "36243c78", "contoso-qa"),
		mk("Storage Blob Data Contributor", "ba92f5b4", "contoso-test"),
		mk("Owner", "8e3af657", "contoso-test"),
	}
}

// TestSelectByNamePrefersExactMatch is the fix for a bare --role Contributor
// firing at every role whose name merely contains it.
func TestSelectByNamePrefersExactMatch(t *testing.T) {
	rows := tenantShapedRows()

	got, report := selectByName(rows, []string{"Contributor"}, nil)
	if len(got) != 2 {
		t.Fatalf("exact match should select the 2 roles actually named Contributor, got %d: %v", len(got), report)
	}
	for _, r := range got {
		if r.Elig.RoleName() != "Contributor" {
			t.Errorf("selected %q, which is not exactly Contributor", r.Elig.RoleName())
		}
	}
	if !strings.Contains(report, "matched 2 of 6") || !strings.Contains(report, "exact") {
		t.Errorf("report = %q", report)
	}

	// Case-insensitive.
	if got, _ := selectByName(rows, []string{"cOnTrIbUtOr"}, nil); len(got) != 2 {
		t.Errorf("exact matching must be case-insensitive, got %d", len(got))
	}
}

func TestSelectByNameFallsBackToSubstring(t *testing.T) {
	rows := tenantShapedRows()
	got, report := selectByName(rows, []string{"Storage Blob"}, nil)
	if len(got) != 1 || got[0].Elig.RoleName() != "Storage Blob Data Contributor" {
		t.Fatalf("substring fallback selected %d rows", len(got))
	}
	if !strings.Contains(report, "substring") {
		t.Errorf("the user must be told it was a substring match: %q", report)
	}
	if !strings.Contains(report, "matched 1 of 6") {
		t.Errorf("report = %q", report)
	}
}

func TestSelectByNameCombinesWithScope(t *testing.T) {
	rows := tenantShapedRows()
	got, _ := selectByName(rows, []string{"Contributor"}, []string{"contoso-qa"})
	if len(got) != 1 || got[0].Elig.Properties.Scope != "/providers/Microsoft.Management/managementGroups/contoso-qa" {
		t.Fatalf("scope should narrow the exact match, got %d rows", len(got))
	}

	// Scope alone selects everything at that scope.
	got, report := selectByName(rows, nil, []string{"contoso-prod"})
	if len(got) != 2 {
		t.Fatalf("scope-only selected %d rows, want 2", len(got))
	}
	if !strings.Contains(report, "by scope") {
		t.Errorf("report = %q", report)
	}
}

func TestSelectByKeys(t *testing.T) {
	rows := tenantShapedRows()
	key := rows[3].SelectionKey()

	got, err := selectByKeys(rows, []string{key})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Elig.RoleName() != "Resource Policy Contributor" {
		t.Fatalf("key selected the wrong row: %v", got)
	}

	// A prefix works, and is case-insensitive.
	if got, err := selectByKeys(rows, []string{strings.ToUpper(key[:5])}); err != nil || len(got) != 1 {
		t.Fatalf("prefix selection failed: %v %v", got, err)
	}
	if _, err := selectByKeys(rows, []string{"abc"}); err == nil {
		t.Error("a key shorter than 4 characters should be refused")
	}
	if _, err := selectByKeys(rows, []string{"zzzzzzzz"}); err == nil {
		t.Error("an unknown key should be an error, not an empty selection")
	}

	// One row named twice — the full key and a prefix of it — is one role.
	// Without this the run fires two concurrent PUTs for the same activation.
	deduped, dedupeErr := selectByKeys(rows, []string{key, key[:5], strings.ToUpper(key)})
	if dedupeErr != nil {
		t.Fatal(dedupeErr)
	}
	if len(deduped) != 1 {
		t.Errorf("naming one row three ways selected %d rows, want 1", len(deduped))
	}

	// Distinct rows are still all selected.
	both, bothErr := selectByKeys(rows, []string{key, rows[0].SelectionKey()})
	if bothErr != nil || len(both) != 2 {
		t.Fatalf("two distinct keys selected %v (%v), want 2 rows", both, bothErr)
	}
}

func TestFilterRows(t *testing.T) {
	rows := []row{
		mkRow("contoso", "Contributor", contribGUID, subScope, "Contoso Identity", "subscription"),
		mkRow("contoso", "Cost Management Contributor", costGUID, mgScope, "Contoso landing zones", "managementgroup"),
		mkRow(
			"contoso",
			"Owner",
			"8e3af657-a8ff-443c-a75c-2fe8c4bcb635",
			subScope,
			"Contoso Identity",
			"subscription",
		),
	}

	// Substring matching is case-insensitive, so "contributor" catches both
	// Contributor and Cost Management Contributor.
	got := filterRows(rows, []string{"contributor"}, nil)
	if len(got) != 2 {
		t.Fatalf("role filter matched %d rows, want 2", len(got))
	}

	got = filterRows(rows, []string{"Cost Management Contributor"}, nil)
	if len(got) != 1 || got[0].Elig.RoleName() != "Cost Management Contributor" {
		t.Fatalf("exact-name filter matched %d rows", len(got))
	}

	// Repeated --role flags are OR-ed.
	got = filterRows(rows, []string{"Owner", "Cost Management"}, nil)
	if len(got) != 2 {
		t.Fatalf("two role filters matched %d rows, want 2", len(got))
	}

	// Scope filters match the id or the display name.
	got = filterRows(rows, nil, []string{"contoso-prod"})
	if len(got) != 1 || got[0].Elig.Properties.Scope != mgScope {
		t.Fatalf("scope-id filter matched %d rows", len(got))
	}
	got = filterRows(rows, nil, []string{"contoso identity"})
	if len(got) != 2 {
		t.Fatalf("scope-name filter matched %d rows, want 2", len(got))
	}

	// Role and scope filters are AND-ed with each other.
	got = filterRows(rows, []string{"contributor"}, []string{"contoso-prod"})
	if len(got) != 1 || got[0].Elig.RoleName() != "Cost Management Contributor" {
		t.Fatalf("combined filter matched %d rows", len(got))
	}

	// No filters means everything.
	if len(filterRows(rows, nil, nil)) != 3 {
		t.Error("an empty filter set must match every row")
	}
}

func TestLegacySelectionKeysRejectContextAmbiguity(t *testing.T) {
	upper := mkRow("Prod", "Contributor", contribGUID, mgScope, "Production", "managementgroup")
	lower := upper
	lower.Context = "prod"
	legacy := lower.SelectionKey()
	selected, err := selectByKeys([]row{upper}, []string{legacy})
	if err != nil || len(selected) != 1 || selected[0].Context != "Prod" {
		t.Fatalf("unambiguous legacy key failed: %v", err)
	}
	if _, err = selectByKeys([]row{upper, lower}, []string{legacy}); err == nil {
		t.Fatal("legacy key silently crossed case-distinct contexts")
	}
	selected, err = selectByKeys([]row{upper, lower}, []string{upper.SelectionKey()})
	if err != nil || len(selected) != 1 || selected[0].Context != "Prod" {
		t.Fatalf("current key failed: %v", err)
	}
}
