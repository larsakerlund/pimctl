// Covers scope.go: ISO-8601 durations both ways, role definition GUIDs and
// scope re-qualification, scope-type normalisation and middle truncation. All
// of it is pure string work, so there is no HTTP server here.

package armclient

import (
	"testing"
	"time"
)

func TestParseISODuration(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"PT1H", time.Hour},
		{"PT4H", 4 * time.Hour},
		{"PT10H", 10 * time.Hour},
		{"PT1H30M", 90 * time.Minute},
		{"PT30M", 30 * time.Minute},
		{"PT8H", 8 * time.Hour},
		{"P1D", 24 * time.Hour},
		{"PT2H30M15S", 2*time.Hour + 30*time.Minute + 15*time.Second},
		{"pt1h", time.Hour},
	}
	for _, c := range cases {
		got, err := ParseISODuration(c.in)
		if err != nil {
			t.Errorf("ParseISODuration(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseISODuration(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"", "4H", "P", "PT", "P1Y", "P2M", "banana"} {
		if _, err := ParseISODuration(bad); err == nil {
			t.Errorf("ParseISODuration(%q) should have failed", bad)
		}
	}
}

func TestFormatISODuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{time.Hour, "PT1H"},
		{4 * time.Hour, "PT4H"},
		{90 * time.Minute, "PT1H30M"},
		{30 * time.Minute, "PT30M"},
		{0, "PT0S"},
		{45 * time.Second, "PT45S"},
	}
	for _, c := range cases {
		if got := FormatISODuration(c.in); got != c.want {
			t.Errorf("FormatISODuration(%v) = %q, want %q", c.in, got, c.want)
		}
	}
	// Round-trip every duration a PIM policy can express.
	for _, iso := range []string{"PT1H", "PT1H30M", "PT4H", "PT10H", "PT30M"} {
		d, err := ParseISODuration(iso)
		if err != nil {
			t.Fatal(err)
		}
		if got := FormatISODuration(d); got != iso {
			t.Errorf("round trip %q -> %v -> %q", iso, d, got)
		}
	}
}

func TestRoleDefinitionGUID(t *testing.T) {
	sub := "/subscriptions/66666666-6666-6666-6666-666666666666/providers/Microsoft.Authorization/roleDefinitions/b24988ac-6180-42a0-ab88-20f7382dd24c"
	mg := "/providers/Microsoft.Authorization/roleDefinitions/b24988ac-6180-42a0-ab88-20f7382dd24c"
	want := "b24988ac-6180-42a0-ab88-20f7382dd24c"
	if got := RoleDefinitionGUID(sub); got != want {
		t.Errorf("subscription-scoped: got %q", got)
	}
	if got := RoleDefinitionGUID(mg); got != want {
		t.Errorf("management-group-scoped: got %q", got)
	}
}

func TestQualifyRoleDefinitionID(t *testing.T) {
	mgScope := "/providers/Microsoft.Management/managementGroups/contoso-prod"
	mgRoleDef := "/providers/Microsoft.Authorization/roleDefinitions/b24988ac-6180-42a0-ab88-20f7382dd24c"
	subScope := "/subscriptions/44444444-4444-4444-4444-444444444444"

	// Activating at the eligibility's own scope keeps ARM's stored form. This
	// is what the tenant's real management-group activation carried.
	if got := QualifyRoleDefinitionID(mgScope, mgScope, mgRoleDef); got != mgRoleDef {
		t.Errorf("same-scope activation rewrote the id: %q", got)
	}

	// Activating at a narrower scope re-qualifies the id against that scope,
	// which is exactly what the 14 subscription-scoped requests did with an
	// eligibility held at the management group.
	want := subScope + "/providers/Microsoft.Authorization/roleDefinitions/b24988ac-6180-42a0-ab88-20f7382dd24c"
	if got := QualifyRoleDefinitionID(subScope, mgScope, mgRoleDef); got != want {
		t.Errorf("narrowed activation:\n got  %q\n want %q", got, want)
	}
}

func TestNormalizeScopeType(t *testing.T) {
	cases := []struct{ armType, scope, want string }{
		{"subscription", "", "Subscription"},
		{"managementgroup", "", "ManagementGroup"},
		{"resourcegroup", "", "ResourceGroup"},
		{"resource", "", "Resource"},
		{"", "/subscriptions/abc", "Subscription"},
		{"", "/providers/Microsoft.Management/managementGroups/contoso-prod", "ManagementGroup"},
		{"", "/subscriptions/abc/resourceGroups/rg1", "ResourceGroup"},
		{"", "/subscriptions/abc/resourceGroups/rg1/providers/Microsoft.Storage/storageAccounts/sa", "Resource"},
	}
	for _, c := range cases {
		if got := NormalizeScopeType(c.armType, c.scope); got != c.want {
			t.Errorf("NormalizeScopeType(%q, %q) = %q, want %q", c.armType, c.scope, got, c.want)
		}
	}
}

func TestTruncateMiddle(t *testing.T) {
	if got := TruncateMiddle("short", 20); got != "short" {
		t.Errorf("got %q", got)
	}
	long := "/subscriptions/66666666-6666-6666-6666-666666666666"
	got := TruncateMiddle(long, 22)
	if len([]rune(got)) != 22 {
		t.Errorf("TruncateMiddle produced %d runes: %q", len([]rune(got)), got)
	}
	if got[:5] != "/subs" {
		t.Errorf("head not preserved: %q", got)
	}
}
