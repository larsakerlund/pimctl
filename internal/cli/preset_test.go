// Tests for the preset round-trip through a real run: --save-preset writing
// the file, a bare PRESET argument replaying it, and what a saved role that is
// no longer eligible does to the run.

package cli

import (
	"strings"
	"testing"

	"github.com/larsakerlund/pimctl/internal/config"
)

func TestPresetSaveLoadAndActivate(t *testing.T) {
	// install() isolates XDG_CONFIG_HOME for every test.
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()}
	f.install()

	if _, _, err := runCmd(
		t,
		"activate",
		"-c",
		"contoso",
		"--all",
		"-j",
		"x",
		"--save-preset",
		"lowimpact",
		"-y",
	); err != nil {
		t.Fatalf("activate --save-preset: %v", err)
	}

	out, _, err := runCmd(t, "preset", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "lowimpact") || !strings.Contains(out, "2") {
		t.Errorf("preset list:\n%s", out)
	}

	out, _, err = runCmd(t, "preset", "show", "lowimpact")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Cost Management Contributor") || !strings.Contains(out, "Resource Policy Contributor") {
		t.Errorf("preset show:\n%s", out)
	}

	f.resetPuts()
	if _, _, err := runCmd(t, "activate", "-c", "contoso", "--preset", "lowimpact", "-j", "x", "-y"); err != nil {
		t.Fatalf("activate --preset: %v", err)
	}
	if len(f.putBodies()) != 2 {
		t.Fatalf("preset activation sent %d requests, want 2", len(f.putBodies()))
	}

	if _, _, err := runCmd(t, "preset", "delete", "lowimpact"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCmd(t, "preset", "show", "lowimpact"); err == nil {
		t.Error("showing a deleted preset should fail")
	}
}

func TestPresetSkipsRolesThatAreNoLongerEligible(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()}
	f.install()
	if _, _, err := runCmd(t, "activate", "-c", "contoso", "--all", "-j", "x", "--save-preset", "p", "-y"); err != nil {
		t.Fatal(err)
	}

	// One role loses its eligibility between runs. --refresh because the
	// listing is cached for five minutes.
	f.eligibilities = f.eligibilities[:1]
	f.resetPuts()
	_, errOut, err := runCmd(t, "activate", "-c", "contoso", "--preset", "p", "-j", "x", "-y", "--refresh")
	if err != nil {
		t.Fatalf("a stale preset entry must not be fatal: %v", err)
	}
	if len(f.putBodies()) != 1 {
		t.Fatalf("sent %d requests, want 1", len(f.putBodies()))
	}
	if !strings.Contains(errOut, "no longer eligible") || !strings.Contains(errOut, "Resource Policy Contributor") {
		t.Errorf("the skipped role was not reported: %q", errOut)
	}
}

// TestPresetShowLabelsTheScope: `preset show` is where you check what a preset
// will elevate before running it, so the scope has to read the way it reads in
// every other table. Three management groups here are called "Azure landing
// zones"; the bare display name does not say which one.
func TestPresetShowLabelsTheScope(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	installFakeRunner(t, []string{"contoso"})

	ps := config.Presets{}
	ps.Set("daily", []config.PresetEntry{
		{
			Context: "contoso", RoleName: "Cost Management Contributor",
			ScopeName: "Contoso landing zones",
			Scope:     "/providers/Microsoft.Management/managementGroups/contoso-test",
		},
		{
			Context: "contoso", RoleName: "Reader",
			ScopeName: "Contoso QA",
			Scope:     "/subscriptions/22222222-0000-0000-0000-000000000002",
		},
	})
	if err := config.SavePresets(&ps); err != nil {
		t.Fatal(err)
	}

	out, _, err := runCmd(t, "preset", "show", "daily")
	if err != nil {
		t.Fatalf("preset show: %v", err)
	}
	// A management group always carries its own name.
	if !strings.Contains(out, "Contoso landing zones (contoso-test)") {
		t.Errorf("the management group is not disambiguated:\n%s", out)
	}
	// A subscription with an unambiguous name does not grow an id in the label,
	// and the SCOPE ID column still carries the full id for both.
	if !strings.Contains(out, "Contoso QA\t") && !strings.Contains(out, "Contoso QA ") {
		t.Errorf("the subscription label changed shape:\n%s", out)
	}
	if !strings.Contains(out, "/subscriptions/22222222-0000-0000-0000-000000000002") {
		t.Errorf("the full scope id is missing:\n%s", out)
	}
}
