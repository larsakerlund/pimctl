// Tests for the machine that has no cloudctx at all, and for the one whose
// cloudctx is older than the companion contract pimctl drives it through.
// pimctl is an Azure CLI tool first: cloudctx adds per-context isolation and
// the flags that select between contexts, and everything else has to work
// without it — including when the cloudctx that is installed is refused.

package cli

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/larsakerlund/pimctl/internal/armclient"
	"github.com/larsakerlund/pimctl/internal/azauth"
	"github.com/larsakerlund/pimctl/internal/config"
)

func TestSharedLoginPresetRoundTripWithoutCloudctx(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(strconv.FormatBool(legacy), func(t *testing.T) {
			installBareARM(t, twoLowImpactRoles())
			if _, _, err := runCmd(t, "up", "--all", "--save-preset", "daily", "-j", "work", "-y"); err != nil {
				t.Fatal(err)
			}
			ps, err := config.LoadPresets()
			if err != nil {
				t.Fatal(err)
			}
			entries, ok := ps.Get("daily")
			if !ok || len(entries) != 2 {
				t.Fatalf("saved selection missing: %+v", entries)
			}
			for _, entry := range entries {
				if entry.Context != "" {
					t.Fatalf("stored display label instead of login identity: %q", entry.Context)
				}
			}
			if legacy {
				for i := range entries {
					entries[i].Context = "(default)"
				}
				ps.Set("daily", entries)
				if err := config.SavePresets(ps); err != nil {
					t.Fatal(err)
				}
			}
			// A preset pins shared az even when a different context is in the shell.
			t.Setenv(envContext, "unrelated-shell-context")
			for _, args := range [][]string{{"up", "daily", "-j", "work", "-y"}, {"down", "daily", "-y"}} {
				_, stderr, err := runCmd(t, args...)
				if err != nil {
					t.Fatalf("preset replay %v failed: %v; %s", args, err, stderr)
				}
				if !strings.Contains(stderr, "using az login") {
					t.Fatalf("replay selected another login: %s", stderr)
				}
			}
		})
	}
}

// installBareARM points pimctl at a fake ARM through the shared-login path,
// with cloudctx absent. It is deliberately not fakeARM.install, which fakes the
// session opener: here the sessions have to come from the real code, since what
// is under test is that the real code copes without cloudctx.
func installBareARM(t *testing.T, elig []armclient.Eligibility) *httptest.Server {
	t.Helper()
	f := &fakeARM{t: t, eligibilities: elig}
	srv := f.start()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv(envContext, "")
	installNoCloudctx(t)
	// The ARM host is the only thing a test can inject into the real session
	// opener, so the token comes from the fake az above and the client points
	// at the fake ARM.
	installSessionOpener(t, func(res resolution, tm *timings, refresh bool) ([]*session, []error, error) {
		sessions, failures, err := openSessionsWith(res, tm, refresh)
		for _, s := range sessions {
			s.Client = armclient.New(srv.URL, s.Token.AccessToken, srv.Client())
		}
		return sessions, failures, err
	})
	return srv
}

func TestWithoutCloudctxTheDailyLoopWorks(t *testing.T) {
	installBareARM(t, twoLowImpactRoles())

	out, errOut, err := runCmd(t, "ls")
	if err != nil {
		t.Fatalf("ls without cloudctx: %v (stderr %q)", err, errOut)
	}
	if !strings.Contains(out, "Cost Management Contributor") {
		t.Errorf("ls listed nothing:\n%s", out)
	}
	// The shared login is announced, because nothing else says which tenant
	// this is.
	if !strings.Contains(errOut, "using az login") {
		t.Errorf("the shared login should be named: %q", errOut)
	}

	if out, _, err = runCmd(t, "up", "--all", "-j", "x", "-y"); err != nil {
		t.Fatalf("up without cloudctx: %v", err)
	}
	if !strings.Contains(out, "ACTIVATED") {
		t.Errorf("up activated nothing:\n%s", out)
	}
	if out, _, err = runCmd(t, "status"); err != nil {
		t.Fatalf("status without cloudctx: %v", err)
	}
	if !strings.Contains(out, "Cost Management Contributor") {
		t.Errorf("status showed nothing:\n%s", out)
	}
	if _, _, err = runCmd(t, "down", "-y"); err != nil {
		t.Fatalf("down without cloudctx: %v", err)
	}
}

// TestWithoutCloudctxTheContextFlagsExplainThemselves: -c and --all-contexts
// are cloudctx's own features, so without it they must say that rather than
// report an exec failure.
func TestWithoutCloudctxTheContextFlagsExplainThemselves(t *testing.T) {
	installBareARM(t, twoLowImpactRoles())

	for _, args := range [][]string{
		{"ls", "--all-contexts"},
		{"ls", "-c", "contoso"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, _, err := runCmd(t, args...)
			if err == nil {
				t.Fatal("expected a clear failure, got none")
			}
			if !strings.Contains(err.Error(), "cloudctx is not installed") {
				t.Errorf("the error does not explain the cause: %v", err)
			}
			// "executable file not found in $PATH" is the operating system
			// talking, not pimctl.
			if strings.Contains(err.Error(), "executable file not found") {
				t.Errorf("the raw exec failure leaked into the message: %v", err)
			}
		})
	}
}

// TestWithoutCloudctxCompletionIsEmpty: a TAB press is not the place to report
// that cloudctx is missing.
func TestWithoutCloudctxCompletionIsEmpty(t *testing.T) {
	installNoCloudctx(t)
	if got, _ := completeContexts(nil, nil, ""); len(got) != 0 {
		t.Errorf("context completion returned %v", got)
	}
	// The key completion reads caches, not cloudctx, and must not fail either.
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	if got, _ := completeKeys(&globalOpts{}); len(got) != 0 {
		t.Errorf("key completion returned %v", got)
	}
}

// TestWithoutCloudctxAPresetNamingAContextFails: a preset carries the contexts
// it was saved against, so replaying one on a machine without cloudctx cannot
// work — and has to say why.
func TestWithoutCloudctxAPresetNamingAContextFails(t *testing.T) {
	installBareARM(t, twoLowImpactRoles())
	writePreset(t, "daily", "contoso")

	_, _, err := runCmd(t, "up", "daily", "-j", "x", "-y")
	if err == nil {
		t.Fatal("a preset naming a context must fail without cloudctx")
	}
	if !strings.Contains(err.Error(), "cloudctx is not installed") {
		t.Errorf("the error does not explain the cause: %v", err)
	}
}

// TestWithoutCloudctxTheCacheClearAdviceOmitsIt: advice a machine cannot follow
// is not advice.
func TestWithoutCloudctxTheCacheClearAdviceOmitsIt(t *testing.T) {
	installBareARM(t, twoLowImpactRoles())
	out, _, err := runCmd(t, "cache", "clear")
	if err != nil {
		t.Fatalf("cache clear: %v", err)
	}
	if !strings.Contains(out, "az logout") {
		t.Errorf("the az advice is missing:\n%s", out)
	}
	if strings.Contains(out, "cloudctx") {
		t.Errorf("cloudctx is not installed and should not be suggested:\n%s", out)
	}
}

// TestBareTokenIsCheckedAgainstAzsOwnDefaultTenant: the shared login has no
// cloudctx registry to pin it, so its cached token is checked against the
// tenant az's own profile calls default. Nothing here spawns cloudctx.
func TestBareTokenIsCheckedAgainstAzsOwnDefaultTenant(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	installNoCloudctx(t)

	// az's profile says the default subscription lives in another tenant than
	// the one the cached token was minted for.
	azDir := home + "/.azure"
	if err := writeFileForTest(t, azDir, "azureProfile.json",
		`{"subscriptions":[{"tenantId":"tid-other","isDefault":true}]}`); err != nil {
		t.Fatal(err)
	}
	stale := &azauth.Token{
		Context: "", AccessToken: "stale", TenantID: "tid-1", Tenant: "tid-1",
		ExpiresOn: farFutureExpiry(),
	}
	azauth.WriteTokenCache(stale, azauth.DefaultRunner)

	tok, err := azauth.AcquireCached("", azauth.DefaultRunner, false)
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken == "stale" {
		t.Error("a token minted for another tenant was served for the shared login")
	}
}

// TestFakeARMServerIsReachable guards the helper itself: a mis-wired
// installBareARM would make every test above pass for the wrong reason.
func TestFakeARMServerIsReachable(t *testing.T) {
	srv := installBareARM(t, twoLowImpactRoles())
	url := srv.URL + "/providers/Microsoft.Authorization/roleEligibilityScheduleInstances"
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // a test server this test owns
	if resp.StatusCode != http.StatusOK {
		t.Errorf("fake ARM answered %d", resp.StatusCode)
	}
}

// TestEmptyCloudctxRegistryExplainsItself: cloudctx installed with nothing in
// it is what a new machine looks like, not a failure. The message has to say
// what to do next — and must not be the "%!w(<nil>)" that wrapping an empty
// errors.Join produced, which told the reader nothing whatsoever.
func TestEmptyCloudctxRegistryExplainsItself(t *testing.T) {
	installCloudctxPresence(t, true)
	prev := azauth.DefaultRunner
	// An empty registry prints nothing at all and exits 0 — the contract's
	// answer, and the reason pimctl no longer parses the human listing, whose
	// "no contexts. Create one with: …" sentence it once read as a context.
	azauth.DefaultRunner = func(_ string, args ...string) ([]byte, []byte, error) {
		if strings.Join(args, " ") == "list --names" {
			return []byte(""), nil, nil
		}
		return nil, nil, errors.New("unexpected command")
	}
	azauth.ForgetContextCache()
	t.Cleanup(func() { azauth.DefaultRunner = prev; azauth.ForgetContextCache() })
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv(envContext, "")

	_, _, err := runCmd(t, "ls", "--all-contexts")
	if err == nil {
		t.Fatal("an empty registry has nothing to act on and must fail")
	}
	for _, want := range []string{"no contexts yet", "cloudctx new", "az login"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message is missing %q: %v", want, err)
		}
	}
	// The two ways this used to read.
	if strings.Contains(err.Error(), "%!w") || strings.Contains(err.Error(), "<nil>") {
		t.Errorf("a formatting artefact reached the user: %v", err)
	}
	if strings.Contains(err.Error(), "no context could be used") {
		t.Errorf("nothing was tried, so nothing could have failed: %v", err)
	}
}

// TestNoSessionsWithoutFailuresIsStillReadable guards the layer underneath: any
// future path that produces zero sessions and zero failures must not format a
// nil error into the message.
func TestNoSessionsWithoutFailuresIsStillReadable(t *testing.T) {
	_, _, err := openSessionsWith(resolution{Source: SourceAllContexts}, newTimings(false), false)
	if err == nil {
		t.Fatal("no sessions is an error")
	}
	if strings.Contains(err.Error(), "%!w") || strings.Contains(err.Error(), "<nil>") {
		t.Errorf("a formatting artefact reached the user: %v", err)
	}
}

// TestAnOlderCloudctxIsRefusedWithOneSentence: pimctl requires the cloudctx
// release that declared the companion contract. An older one is a setup
// problem with a one-line fix, so the command says which version is needed and
// how to get it rather than failing at the first surface that is missing.
func TestAnOlderCloudctxIsRefusedWithOneSentence(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()}
	f.install()
	prev := azauth.CloudctxVersion
	azauth.CloudctxVersion = func(azauth.Runner) (string, error) { return "1.3.0", nil }
	azauth.ForgetContextCache()
	t.Cleanup(func() { azauth.CloudctxVersion = prev; azauth.ForgetContextCache() })
	// The real opener, so the refusal comes from the code the user meets.
	installSessionOpener(t, openSessionsWith)

	_, _, err := runCmd(t, "ls", "-c", "contoso")
	if err == nil {
		t.Fatal("a cloudctx too old for the contract must be refused, not worked around")
	}
	for _, want := range []string{"1.3.0", azauth.MinCloudctxVersion, "cloudctx self-update"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error is missing %q: %v", want, err)
		}
	}
}

// TestAnOlderCloudctxStillLeavesTheAzLoginAlone: the version of a tool pimctl
// does not have to use says nothing about the shared `az login`, which never
// goes through cloudctx.
func TestAnOlderCloudctxStillLeavesTheAzLoginAlone(t *testing.T) {
	installBareARM(t, twoLowImpactRoles())
	prev := azauth.CloudctxVersion
	azauth.CloudctxVersion = func(azauth.Runner) (string, error) { return "1.3.0", nil }
	t.Cleanup(func() { azauth.CloudctxVersion = prev })

	out, errOut, err := runCmd(t, "ls")
	if err != nil {
		t.Fatalf("the shared login must work regardless of cloudctx: %v (stderr %q)", err, errOut)
	}
	if !strings.Contains(out, "Cost Management Contributor") {
		t.Errorf("ls listed nothing:\n%s", out)
	}
}
