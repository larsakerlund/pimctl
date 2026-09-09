// Covers context.go: the precedence between -c, --all-contexts, --bare-az, a
// preset's own contexts and $CLOUDCTX_CONTEXT, the combinations that are
// refused, and the one-line notices that say which was chosen.

package cli

import (
	"strings"
	"testing"

	"github.com/larsakerlund/pimctl/internal/config"
)

func envFunc(vals map[string]string) func(string) string {
	return func(k string) string { return vals[k] }
}

func TestResolveContextsPrecedence(t *testing.T) {
	env := envFunc(map[string]string{envContext: "from-env"})

	for _, c := range []struct {
		name       string
		flags      []string
		presets    []string
		bare       bareAzMode
		env        func(string) string
		wantName   string
		wantSource contextSource
	}{
		{
			name:  "-c beats everything",
			flags: []string{"flagged"}, presets: []string{"preset-ctx"}, env: env,
			wantName: "flagged", wantSource: SourceFlag,
		},
		{
			// Opening anything else would match none of the preset's entries.
			name:    "a preset's contexts beat the environment",
			presets: []string{"preset-ctx"}, env: env,
			wantName: "preset-ctx", wantSource: SourcePreset,
		},
		{
			name:     "the environment is the fallback",
			env:      env,
			wantName: "from-env", wantSource: SourceEnv,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolveContexts(c.flags, false, c.bare, c.presets, c.env)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Names) != 1 || got.Names[0] != c.wantName || got.Source != c.wantSource {
				t.Fatalf("%s: %+v", c.name, got)
			}
		})
	}

	// --bare-az is explicit and never implied, and names nothing.
	got, err := resolveContexts(nil, false, bareAzOn, nil, envFunc(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Bare || len(got.Names) != 0 {
		t.Fatalf("--bare-az: %+v", got)
	}
}

// TestResolveContextsFallsBackToAzLogin: with nothing set, pimctl uses the
// shared `az login` rather than refusing. This reverses an earlier decision and
// is deliberate — see the ResolveContexts doc comment. The safeguard is that
// the tenant and user are announced on stderr, which TestAzLoginIsAnnounced
// and DescribeAzLogin cover.
func TestResolveContextsFallsBackToAzLogin(t *testing.T) {
	got, err := resolveContexts(nil, false, bareAzUnset, nil, envFunc(nil))
	if err != nil {
		t.Fatalf("with no context anywhere, pimctl should use az login: %v", err)
	}
	if !got.Bare {
		t.Fatal("expected the shared az login")
	}
	if got.Source != SourceAzLogin {
		t.Errorf("source = %q, want %q", got.Source, SourceAzLogin)
	}
}

func TestDescribeAzLogin(t *testing.T) {
	got := describeAzLogin("11111111-2222-3333-4444-555555555555", "ada.lovelace@contoso.example")
	want := "using az login: tenant 11111111-2222-3333-4444-555555555555 (ada.lovelace@contoso.example)"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	// A token without a upn claim still names the tenant.
	if got := describeAzLogin("tid", ""); got != "using az login: tenant tid" {
		t.Errorf("got %q", got)
	}
	if got := describeAzLogin("", ""); !strings.Contains(got, "unknown") {
		t.Errorf("got %q", got)
	}
}

func TestResolveContextsRejectsConflicts(t *testing.T) {
	env := envFunc(nil)
	if _, err := resolveContexts([]string{"a"}, true, bareAzUnset, nil, env); err == nil {
		t.Error("-c with --all-contexts should be rejected")
	}
	if _, err := resolveContexts([]string{"a"}, false, bareAzOn, nil, env); err == nil {
		t.Error("-c with --bare-az should be rejected")
	}
	if _, err := resolveContexts(nil, true, bareAzOn, nil, env); err == nil {
		t.Error("--all-contexts with --bare-az should be rejected")
	}
}

func TestResolveContextsDedupesAndTrims(t *testing.T) {
	got, err := resolveContexts([]string{"a", "a", "b"}, false, bareAzUnset, nil, envFunc(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Names) != 2 || got.Names[0] != "a" || got.Names[1] != "b" {
		t.Fatalf("names = %v", got.Names)
	}

	// A blank environment variable is not a context; it falls through to the
	// shared login rather than naming a context called "   ".
	blank, err := resolveContexts(nil, false, bareAzUnset, nil, envFunc(map[string]string{envContext: "   "}))
	if err != nil {
		t.Fatal(err)
	}
	if !blank.Bare {
		t.Errorf("a whitespace-only $%s should not count as a context: %+v", envContext, blank)
	}
}

func TestDescribeNamesTheSource(t *testing.T) {
	r := resolution{Names: []string{"contoso"}, Source: SourceEnv}
	if got := r.Describe(); !strings.Contains(got, "contoso") || !strings.Contains(got, envContext) {
		t.Errorf("Describe() = %q", got)
	}
	// The az-login line is produced later, from the token's claims, so Describe
	// says nothing for it.
	r = resolution{Bare: true, Source: SourceAzLogin}
	if got := r.Describe(); got != "" {
		t.Errorf("Describe() for the shared login should be empty, got %q", got)
	}
}

func TestPresetContexts(t *testing.T) {
	entries := []config.PresetEntry{
		{Context: "contoso"}, {Context: "globex"}, {Context: "contoso"},
	}
	got := presetContexts(entries)
	if len(got) != 2 || got[0] != "contoso" || got[1] != "globex" {
		t.Fatalf("PresetContexts = %v", got)
	}
	if got := presetContexts(nil); len(got) != 0 {
		t.Fatalf("no entries should yield no contexts, got %v", got)
	}
}
