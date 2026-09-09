// Tests for shell completion: that every source is local, that a broken one
// completes to nothing rather than erroring at a TAB press, and that the
// descriptions cobra shows are the ones a reader would want.

package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/larsakerlund/pimctl/internal/azauth"
	"github.com/larsakerlund/pimctl/internal/cache"
	"github.com/larsakerlund/pimctl/internal/config"
)

func TestRegisterCompletionsWiresTheFlagsThatHaveSources(t *testing.T) {
	root := NewRootCmd()

	// Every command that takes a preset or a key completes it, and the two
	// verbs that take a positional preset complete that too.
	for _, name := range []string{"up", "down", "activate", "deactivate"} {
		cmd, _, err := root.Find([]string{name})
		if err != nil {
			t.Fatalf("finding %s: %v", name, err)
		}
		if cmd.ValidArgsFunction == nil {
			t.Errorf("%s does not complete its positional preset", name)
		}
	}
	for _, name := range []string{"show", "delete"} {
		cmd, _, err := root.Find([]string{"preset", name})
		if err != nil {
			t.Fatalf("finding preset %s: %v", name, err)
		}
		if cmd.ValidArgsFunction == nil {
			t.Errorf("preset %s does not complete preset names", name)
		}
	}
	// A command with no preset flag must not have grown one by accident.
	ls, _, err := root.Find([]string{"ls"})
	if err != nil {
		t.Fatal(err)
	}
	if ls.Flags().Lookup("preset") != nil {
		t.Error("ls has no preset selection and should not offer the flag")
	}
}

func TestCompleteContexts(t *testing.T) {
	// This is the with-cloudctx case; the without case is in nocloudctx_test.go.
	installCloudctxPresence(t, true)
	installFakeRunner(t, []string{"contoso", "globex"})
	got, directive := completeContexts(nil, nil, "")
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("directive = %v; completing a context must not offer files", directive)
	}
	if len(got) != 2 || got[0] != "contoso" || got[1] != "globex" {
		t.Errorf("completions = %v, want the two context names", got)
	}

	// cloudctx not installed, or refusing: complete to nothing. A TAB press is
	// not the place to report an error.
	prev := azauth.DefaultRunner
	azauth.DefaultRunner = func(string, ...string) ([]byte, []byte, error) {
		return nil, nil, errors.New("no cloudctx here")
	}
	t.Cleanup(func() { azauth.DefaultRunner = prev })
	if got, _ := completeContexts(nil, nil, ""); got != nil {
		t.Errorf("a missing cloudctx should complete to nothing, got %v", got)
	}
}

func TestCompletePresets(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// No presets file at all is a clean empty completion.
	if got, _ := completePresets(nil, nil, ""); len(got) != 0 {
		t.Errorf("an empty config should complete to nothing, got %v", got)
	}

	ps := config.Presets{}
	ps.Set("daily", []config.PresetEntry{
		{Context: "contoso", Scope: "/s1", RoleDefinitionID: "/r1", RoleName: "Owner"},
		{Context: "contoso", Scope: "/s2", RoleDefinitionID: "/r2", RoleName: "Reader"},
	})
	ps.Set("one", []config.PresetEntry{
		{Context: "globex", Scope: "/s3", RoleDefinitionID: "/r3", RoleName: "Owner"},
	})
	if err := config.SavePresets(&ps); err != nil {
		t.Fatal(err)
	}

	got, directive := completePresets(nil, nil, "")
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("directive = %v", directive)
	}
	if len(got) != 2 {
		t.Fatalf("completions = %v, want two presets", got)
	}
	// cobra splits "name\tdescription", and the description says which
	// contexts and how many roles — the two things you need to pick between
	// presets without opening the file.
	if !strings.HasPrefix(got[0], "daily\t") || !strings.Contains(got[0], "contoso") ||
		!strings.Contains(got[0], "2 roles") {
		t.Errorf("daily completion = %q", got[0])
	}
	if !strings.Contains(got[1], "1 role") || strings.Contains(got[1], "1 roles") {
		t.Errorf("a one-role preset should not say \"1 roles\": %q", got[1])
	}
}

func TestCompletePresetArgStopsAfterTheFirst(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ps := config.Presets{}
	ps.Set("daily", []config.PresetEntry{{Context: "contoso", RoleName: "Owner"}})
	if err := config.SavePresets(&ps); err != nil {
		t.Fatal(err)
	}
	if got, _ := completePresetArg(nil, nil, ""); len(got) != 1 {
		t.Errorf("the first argument should complete presets, got %v", got)
	}
	// `pimctl up daily <TAB>` takes no second preset.
	if got, _ := completePresetArg(nil, []string{"daily"}, ""); got != nil {
		t.Errorf("a second positional should complete to nothing, got %v", got)
	}
}

func TestCompleteKeysReadsOnlyTheCache(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv(envContext, "")
	opts := &globalOpts{contexts: []string{"contoso"}}
	// Any exec at all would be a network call or a token mint behind a TAB.
	installFakeRunner(t, nil)

	// A cold cache completes to nothing rather than filling it.
	if got, _ := completeKeys(opts); len(got) != 0 {
		t.Errorf("a cold cache should complete to nothing, got %v", got)
	}

	cache.Write("contoso", twoLowImpactRoles())
	got, directive := completeKeys(opts)
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("directive = %v", directive)
	}
	if len(got) != 2 {
		t.Fatalf("completions = %v, want one per cached role", got)
	}
	// "key\trole @ scope" — the key is what --key takes, the rest is what
	// makes it possible to choose between two of them.
	key, label, found := strings.Cut(got[0], "\t")
	if !found || len(key) != selectionKeyLen {
		t.Errorf("completion %q does not lead with the selection key", got[0])
	}
	if !strings.Contains(label, "@") {
		t.Errorf("completion %q does not name the scope", got[0])
	}

	// --bare-az has no context to read a cache for.
	opts = &globalOpts{bareAz: bareAzOn}
	if got, _ := completeKeys(opts); got != nil {
		t.Errorf("--bare-az should complete no keys, got %v", got)
	}
}
