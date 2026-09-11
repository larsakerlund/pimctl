// Tests for row identity and the operations over a slice of rows: matching
// activations, the stable selection key, deduping a role granted twice,
// sorting, and the preset round-trip.

package cli

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/larsakerlund/pimctl/internal/armclient"
	"github.com/larsakerlund/pimctl/internal/config"
)

const (
	contribGUID = "b24988ac-6180-42a0-ab88-20f7382dd24c"
	costGUID    = "434105ed-43f6-45c7-a02f-909b2ba83430"
	subScope    = "/subscriptions/66666666-6666-6666-6666-666666666666"
	mgScope     = "/providers/Microsoft.Management/managementGroups/contoso-prod"
)

func mkRow(ctx, role, roleGUID, scope, scopeName, scopeType string) row {
	e := armclient.Eligibility{}
	e.Properties.Scope = scope
	e.Properties.RoleDefinitionID = scope + "/providers/Microsoft.Authorization/roleDefinitions/" + roleGUID
	e.Properties.RoleEligibilityScheduleID = scope + "/providers/Microsoft.Authorization/roleEligibilitySchedules/" + roleGUID
	e.Properties.MemberType = "Group"
	e.Properties.ExpandedProperties.RoleDefinition = armclient.Named{DisplayName: role}
	e.Properties.ExpandedProperties.Scope = armclient.Named{DisplayName: scopeName, Type: scopeType, ID: scope}
	return row{Context: ctx, Elig: e}
}

func mkActivated(role, roleGUID, scope, scopeName string, end time.Time) armclient.Assignment {
	a := armclient.Assignment{}
	a.Properties.AssignmentType = "Activated"
	a.Properties.Scope = scope
	a.Properties.RoleDefinitionID = "/providers/Microsoft.Authorization/roleDefinitions/" + roleGUID
	a.Properties.EndDateTime = &end
	a.Properties.ExpandedProperties.RoleDefinition = armclient.Named{DisplayName: role}
	a.Properties.ExpandedProperties.Scope = armclient.Named{DisplayName: scopeName, Type: "subscription"}
	return a
}

func TestMatchActivations(t *testing.T) {
	rows := []row{
		mkRow("contoso", "Contributor", contribGUID, subScope, "Contoso Identity", "subscription"),
		mkRow("contoso", "Cost Management Contributor", costGUID, mgScope, "Contoso landing zones", "managementgroup"),
	}
	end := time.Now().Add(time.Hour)
	active := []armclient.Assignment{
		mkActivated("Cost Management Contributor", costGUID, mgScope, "Contoso landing zones", end),
		// A permanent assignment must never count as an activation.
		func() armclient.Assignment {
			a := mkActivated("Contributor", contribGUID, subScope, "Contoso Identity", end)
			a.Properties.AssignmentType = "Assigned"
			return a
		}(),
	}
	contextual := make([]activeRow, 0, len(active))
	for _, a := range active {
		contextual = append(contextual, activeRow{Context: "contoso", Assignment: a})
	}
	rows = matchActivations(rows, contextual)
	if rows[0].IsActive() {
		t.Error("a permanently Assigned role must not be reported as activated")
	}
	if !rows[1].IsActive() {
		t.Fatal("the Activated role was not matched to its eligibility")
	}
	if got := rows[1].ActiveUntil(); got == nil || !got.Equal(end) {
		t.Errorf("ActiveUntil = %v, want %v", got, end)
	}
}

func TestPresetRoundTripThroughSelection(t *testing.T) {
	rows := []row{
		mkRow("contoso", "Contributor", contribGUID, subScope, "Contoso Identity", "subscription"),
		mkRow("contoso", "Cost Management Contributor", costGUID, mgScope, "Contoso landing zones", "managementgroup"),
	}
	entries := toPresetEntries(rows)
	if len(entries) != 2 {
		t.Fatalf("ToPresetEntries produced %d entries", len(entries))
	}
	if entries[0].RoleDefinitionID == "" || entries[0].Scope == "" || entries[0].Context != "contoso" {
		t.Fatalf("preset entry is missing identity fields: %+v", entries[0])
	}

	selected, missing := applyPreset(rows, entries)
	if len(selected) != 2 || len(missing) != 0 {
		t.Fatalf("round trip selected %d, missing %d", len(selected), len(missing))
	}

	// A role that is no longer eligible is reported, not fatal.
	stale := slices.Concat(entries, []config.PresetEntry{{
		Context:          "contoso",
		Scope:            "/subscriptions/gone",
		RoleDefinitionID: "/subscriptions/gone/providers/Microsoft.Authorization/roleDefinitions/" + contribGUID,
		RoleName:         "Owner",
		ScopeName:        "Retired sub",
	}})
	selected, missing = applyPreset(rows, stale)
	if len(selected) != 2 {
		t.Fatalf("still-eligible roles dropped: %d selected", len(selected))
	}
	if len(missing) != 1 || !strings.Contains(missing[0], "Owner") || !strings.Contains(missing[0], "Retired sub") {
		t.Fatalf("stale entry not reported usefully: %v", missing)
	}

	// A preset saved against a different context must not match.
	crossCtx := []config.PresetEntry{{
		Context:          "globex",
		Scope:            subScope,
		RoleDefinitionID: entries[0].RoleDefinitionID,
		RoleName:         "Contributor",
	}}
	selected, missing = applyPreset(rows, crossCtx)
	if len(selected) != 0 || len(missing) != 1 {
		t.Fatalf("preset entries must be context-scoped: %d selected, %d missing", len(selected), len(missing))
	}
}

func TestSortRowsIsStableAcrossRuns(t *testing.T) {
	rows := []row{
		mkRow("contoso", "Owner", "o", subScope, "Zeta", "subscription"),
		mkRow("contoso", "Contributor", contribGUID, mgScope, "Alpha", "managementgroup"),
		mkRow("azure2", "Contributor", contribGUID, subScope, "Beta", "subscription"),
	}
	sortRows(rows)
	if rows[0].Context != "azure2" {
		t.Errorf("contexts should sort first: %v", rows[0].Context)
	}
	if rows[1].Elig.RoleName() != "Contributor" || rows[2].Elig.RoleName() != "Owner" {
		t.Errorf("roles out of order: %q %q", rows[1].Elig.RoleName(), rows[2].Elig.RoleName())
	}
}

func TestFormatRemaining(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		end  *time.Time
		want string
	}{
		{nil, "-"},
		{new(now.Add(-time.Minute)), "expired"},
		{new(now.Add(58 * time.Minute)), "58m"},
		{new(now.Add(3*time.Hour + 12*time.Minute)), "3h12m"},
		{new(now.Add(4 * time.Hour)), "4h00m"},
	}
	for _, c := range cases {
		if got := formatRemaining(c.end, now); got != c.want {
			t.Errorf("FormatRemaining(%v) = %q, want %q", c.end, got, c.want)
		}
	}
}

// TestSelectionKeyIsStable: the key must not move when the tenant's list does,
// or a script that round-trips list -> up would act on the wrong role.
func TestSelectionKeyIsStable(t *testing.T) {
	rows := tenantShapedRows()
	before := rows[3].SelectionKey()

	// Same role and scope, different position and different neighbours.
	reordered := []row{rows[5], rows[3], rows[0]}
	if got := reordered[1].SelectionKey(); got != before {
		t.Fatalf("key changed with position: %q -> %q", before, got)
	}

	// A different scope must produce a different key.
	other := mkRow("contoso", "Resource Policy Contributor", "36243c78",
		"/providers/Microsoft.Management/managementGroups/contoso-test", "Contoso landing zones", "managementgroup")
	if other.SelectionKey() == before {
		t.Fatal("two different scopes produced the same key")
	}
	// So must a different context.
	otherCtx := mkRow("globex", "Resource Policy Contributor", "36243c78",
		"/providers/Microsoft.Management/managementGroups/contoso-qa", "Contoso landing zones", "managementgroup")
	if otherCtx.SelectionKey() == before {
		t.Fatal("two different contexts produced the same key")
	}
	if len(before) != 8 {
		t.Errorf("key should be short enough for a table column, got %d chars", len(before))
	}
}

// TestDedupeRows covers the same role+scope arriving twice, once direct and
// once via a group — which previously produced two PUTs for one activation.
func TestDedupeRows(t *testing.T) {
	scope := "/providers/Microsoft.Management/managementGroups/contoso-prod"
	group := mkRow("contoso", "Contributor", contribGUID, scope, "Contoso landing zones", "managementgroup")
	group.Elig.Properties.MemberType = "Group"
	direct := mkRow("contoso", "Contributor", contribGUID, scope, "Contoso landing zones", "managementgroup")
	direct.Elig.Properties.MemberType = "Direct"
	other := mkRow("contoso", "Owner", "8e3af657", scope, "Contoso landing zones", "managementgroup")

	got := dedupeRows([]row{group, direct, other})
	if len(got) != 2 {
		t.Fatalf("expected 2 rows after folding the duplicate, got %d", len(got))
	}
	var folded *row
	for i := range got {
		if got[i].Elig.RoleName() == "Contributor" {
			folded = &got[i]
		}
	}
	if folded == nil {
		t.Fatal("the Contributor row disappeared")
	}
	if folded.Elig.Properties.MemberType != "Direct" {
		t.Errorf("the Direct eligibility should be the one kept, got %q", folded.Elig.Properties.MemberType)
	}
	if len(folded.AlsoVia) != 1 || folded.AlsoVia[0] != "Group" {
		t.Errorf("the folded-away membership should be recorded, got %v", folded.AlsoVia)
	}

	// Order independence: the Direct one wins whichever way round they arrive.
	got = dedupeRows([]row{direct, group, other})
	for _, r := range got {
		if r.Elig.RoleName() == "Contributor" && r.Elig.Properties.MemberType != "Direct" {
			t.Errorf("kept %q when Direct came first", r.Elig.Properties.MemberType)
		}
	}
}
