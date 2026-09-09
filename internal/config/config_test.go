// Tests for config.go: that $XDG_CONFIG_HOME is honoured, that presets and
// state survive a round trip, that both files are written owner-only, and that
// concurrent saves cannot leave a corrupt file behind.

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestDirRespectsXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg-test")
	d, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if d != "/tmp/xdg-test/pimctl" {
		t.Fatalf("Dir() = %q", d)
	}

	t.Setenv("XDG_CONFIG_HOME", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolving the home directory: %v", err)
	}
	d, err = Dir()
	if err != nil {
		t.Fatal(err)
	}
	if d != filepath.Join(home, ".config", "pimctl") {
		t.Fatalf("Dir() without XDG = %q", d)
	}
}

// assertPreset checks that name holds exactly want, entry by entry.
func assertPreset(t *testing.T, ps *Presets, name string, want []PresetEntry) {
	t.Helper()
	got, ok := ps.Get(name)
	if !ok {
		t.Fatal("preset missing after reload")
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d:\n got  %+v\n want %+v", i, got[i], want[i])
		}
	}
}

func TestPresetRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	ps, err := LoadPresets()
	if err != nil {
		t.Fatalf("LoadPresets on a fresh config dir: %v", err)
	}
	if len(ps.Names()) != 0 {
		t.Fatalf("a missing presets file should be an empty set, got %v", ps.Names())
	}

	want := []PresetEntry{
		{
			Context:          "contoso",
			Scope:            "/providers/Microsoft.Management/managementGroups/contoso-prod",
			RoleDefinitionID: "/providers/Microsoft.Authorization/roleDefinitions/434105ed-43f6-45c7-a02f-909b2ba83430",
			RoleName:         "Cost Management Contributor",
			ScopeName:        "Contoso landing zones",
		},
		{
			Context:          "contoso",
			Scope:            "/providers/Microsoft.Management/managementGroups/contoso-qa",
			RoleDefinitionID: "/providers/Microsoft.Authorization/roleDefinitions/36243c78-bf99-498c-9df9-86d9f8d28608",
			RoleName:         "Resource Policy Contributor",
			ScopeName:        "QA",
		},
	}
	ps.Set("lowimpact", want)
	if err = SavePresets(ps); err != nil {
		t.Fatalf("SavePresets: %v", err)
	}

	reloaded, err := LoadPresets()
	if err != nil {
		t.Fatalf("LoadPresets: %v", err)
	}
	assertPreset(t, reloaded, "lowimpact", want)

	if names := reloaded.Names(); len(names) != 1 || names[0] != "lowimpact" {
		t.Errorf("Names() = %v", names)
	}
	if !reloaded.Delete("lowimpact") {
		t.Error("Delete should report that the preset existed")
	}
	if reloaded.Delete("lowimpact") {
		t.Error("deleting twice should report false")
	}
	if err = SavePresets(reloaded); err != nil {
		t.Fatal(err)
	}
	final, err := LoadPresets()
	if err != nil {
		t.Fatalf("LoadPresets: %v", err)
	}
	if _, ok := final.Get("lowimpact"); ok {
		t.Error("preset still present after delete")
	}
}

func TestStateRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	s, err := LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if s.LastJustification != "" {
		t.Fatalf("fresh state should be empty, got %q", s.LastJustification)
	}
	s.LastJustification = "Deploying the landing zone"
	if err = SaveState(s); err != nil {
		t.Fatal(err)
	}
	again, err := LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if again.LastJustification != "Deploying the landing zone" {
		t.Fatalf("LastJustification = %q", again.LastJustification)
	}
}

func TestConfigFilesArePrivate(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	ps, err := LoadPresets()
	if err != nil {
		t.Fatalf("LoadPresets: %v", err)
	}
	ps.Set("x", nil)
	if err = SavePresets(ps); err != nil {
		t.Fatal(err)
	}
	p, err := PresetsPath()
	if err != nil {
		t.Fatalf("PresetsPath: %v", err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("presets.json mode = %v, want 0600", fi.Mode().Perm())
	}
}

// TestConcurrentSavesDoNotCorrupt covers the fixed-temp-filename race: two
// pimctl runs finishing at once must not leave a truncated presets.json.
func TestConcurrentSavesDoNotCorrupt(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	entries := make([]PresetEntry, 40)
	for i := range entries {
		entries[i] = PresetEntry{
			Context:          "contoso",
			Scope:            "/subscriptions/1111",
			RoleDefinitionID: "/providers/Microsoft.Authorization/roleDefinitions/aaaa",
			RoleName:         "Contributor",
			ScopeName:        "a reasonably long scope display name to make the file bigger",
		}
	}

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			ps := &Presets{Presets: map[string][]PresetEntry{fmt.Sprintf("p%d", n): entries}}
			if err := SavePresets(ps); err != nil {
				t.Errorf("SavePresets: %v", err)
			}
		}(i)
	}
	wg.Wait()

	// Whichever writer won, the file must be complete and parseable.
	got, err := LoadPresets()
	if err != nil {
		t.Fatalf("presets.json is corrupt after concurrent writes: %v", err)
	}
	if len(got.Names()) != 1 {
		t.Fatalf("expected exactly one preset from the winning write, got %v", got.Names())
	}
	if n := len(got.Presets[got.Names()[0]]); n != len(entries) {
		t.Fatalf("preset was truncated: %d entries, want %d", n, len(entries))
	}

	// No temp files should be left behind.
	dir, err := Dir()
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	for _, f := range files {
		if strings.Contains(f.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", f.Name())
		}
	}
}
