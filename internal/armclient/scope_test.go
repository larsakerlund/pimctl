// Covers scope.go: ISO-8601 durations both ways, role definition GUIDs and
// scope re-qualification, scope-type normalisation and middle truncation. All
// of it is pure string work, so there is no HTTP server here.

package armclient

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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

func TestParseISODurationRejectsAnythingBeyondTheBound(t *testing.T) {
	// A component that would wrap the multiply, more digits than an int64
	// holds, one that merely exceeds the bound, and a total that exceeds it
	// across components: all the same error, naming the bound.
	for _, bad := range []string{
		"PT9223372036854775807H",
		"PT99999999999999999999H",
		"P1001D",
		"P1000DT1S",
		"PT24001H",
	} {
		d, err := ParseISODuration(bad)
		if err == nil {
			t.Errorf("ParseISODuration(%q) = %v, want an error", bad, d)
			continue
		}
		if !strings.Contains(err.Error(), "1000 days") {
			t.Errorf("ParseISODuration(%q) error should name the bound: %v", bad, err)
		}
	}
	// The bound itself is inclusive.
	for _, ok := range []string{"P1000D", "PT24000H", "P999DT24H"} {
		d, err := ParseISODuration(ok)
		if err != nil || d != MaxISODuration {
			t.Errorf("ParseISODuration(%q) = %v, %v; want exactly the bound", ok, d, err)
		}
	}
}

// FuzzParseISODuration pins the two properties every accepted duration has:
// it lies within [0, MaxISODuration], and rendering it with FormatISODuration
// and parsing it again yields the same value. Anything else must be an error,
// never a panic.
func FuzzParseISODuration(f *testing.F) {
	for _, seed := range []string{
		"PT1H", "PT4H", "PT10H", "PT1H30M", "PT30M", "PT8H", "P1D", "PT2H30M15S", "pt1h",
		"", "4H", "P", "PT", "P1Y", "P2M", "banana",
		"PT9223372036854775807H", "P1001D", "P1000D", "PT0S", " PT1H ",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		d, err := ParseISODuration(in)
		if err != nil {
			return
		}
		if d < 0 || d > MaxISODuration {
			t.Fatalf("ParseISODuration(%q) = %v, outside [0, %v]", in, d, MaxISODuration)
		}
		iso := FormatISODuration(d)
		back, err := ParseISODuration(iso)
		if err != nil {
			t.Fatalf(
				"ParseISODuration(%q) = %v, but FormatISODuration gave %q which does not parse: %v",
				in,
				d,
				iso,
				err,
			)
		}
		if back != d {
			t.Fatalf("round trip %q -> %v -> %q -> %v", in, d, iso, back)
		}
	})
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

func TestTruncateMiddleCutsOnRunesNotBytes(t *testing.T) {
	// A Swedish name is longer in bytes than in runes, and an emoji is four
	// bytes for one rune: the width is in runes, both ends are whole
	// characters, and a name that fits in runes is left alone even when its
	// byte length would not.
	cases := []struct {
		in    string
		width int
		want  string
	}{
		{"Kostnadshanteringsdeltagare för Sjöfartsverket", 20, "Kostnadsh…artsverket"},
		{"Prenumeration för Åkerlund", 26, "Prenumeration för Åkerlund"},
		{"Sjöfart 🚢 Östersjön 🌊", 10, "Sjöf…jön 🌊"},
		{"ÅÄÖåäö", 6, "ÅÄÖåäö"},
		{"ÅÄÖåäöÅÄÖ", 5, "ÅÄ…ÄÖ"},
	}
	for _, c := range cases {
		got := TruncateMiddle(c.in, c.width)
		if got != c.want {
			t.Errorf("TruncateMiddle(%q, %d) = %q, want %q", c.in, c.width, got, c.want)
		}
		if !utf8.ValidString(got) {
			t.Errorf("TruncateMiddle(%q, %d) = %q cut a rune in half", c.in, c.width, got)
		}
		if n := utf8.RuneCountInString(got); n > c.width {
			t.Errorf("TruncateMiddle(%q, %d) is %d runes wide", c.in, c.width, n)
		}
	}
}
