// Tests for cloudctx.go: the version gate, the contract listing, the `show`
// parse, and where a context's state lives. Most run against a fake runner;
// the ones named "contract" run against a stub `cloudctx` on PATH that
// implements cloudctx's documented companion surfaces literally, so a change
// on either side of the seam shows up here.

package azauth

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// stubCloudctx writes a `cloudctx` onto a PATH of its own, implementing exactly
// the surfaces docs/companions.md declares — `--version`, `list --names`,
// `show`, and the unknown-context error — for the given version and contexts.
// It returns the fake cloudctx home the stub reports store paths under.
//
// Every shape here was taken from the released cloudctx v1.4.0 binary rather
// than from its documentation: `cloudctx 1.4.0` on stdout for --version, one
// bare name per line for `list --names`, `-v` alongside it refused with exit 2
// and an empty stdout, the four path lines of `show` with `store:` first, and
// `cloudctx: error: unknown context '<name>'` on stderr with exit 1 and an
// empty stdout.
func stubCloudctx(t *testing.T, version string, contexts ...string) string {
	t.Helper()
	bin := t.TempDir()
	home := t.TempDir()
	script := "#!/bin/sh\n" +
		"known=\"" + strings.Join(contexts, " ") + "\"\n" +
		"case \"$1\" in\n" +
		"  --version) echo \"cloudctx " + version + "\"; exit 0 ;;\n" +
		"  list)\n" +
		"    if [ \"$2\" = \"--names\" ] && [ -z \"$3\" ]; then for c in $known; do echo \"$c\"; done; exit 0; fi\n" +
		"    echo 'cloudctx list: error: argument -v/--verbose: not allowed with argument --names' >&2\n" +
		"    exit 2 ;;\n" +
		"  show)\n" +
		"    for c in $known; do\n" +
		"      if [ \"$c\" = \"$2\" ]; then\n" +
		"        echo \"[$2]\"; echo \"azure_tenant = tid-$2\"; echo \"display = $2 AB\"; echo\n" +
		"        echo \"store:           " + home + "/$2\"\n" +
		"        echo \"azure store:     " + home + "/$2/azure  (exists)\"\n" +
		"        echo \"aws config:      " + home + "/$2/aws/config  (missing)\"\n" +
		"        echo \"aws credentials: " + home + "/$2/aws/credentials  (missing)\"\n" +
		"        exit 0\n" +
		"      fi\n" +
		"    done\n" +
		"    echo \"cloudctx: error: unknown context '$2'\" >&2; exit 1 ;;\n" +
		"esac\n" +
		"echo \"cloudctx: error: unrecognised arguments\" >&2; exit 2\n"
	path := filepath.Join(bin, "cloudctx")
	//nolint:gosec // 0700: the stub is a program the test has to be able to run.
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	// The gate and the presence check both read the host here, which is the
	// point of these tests: they run against the stub rather than a pin.
	prevInstalled, prevVersion := CloudctxInstalled, CloudctxVersion
	CloudctxInstalled = func() bool { _, err := exec.LookPath(cloudctxBin); return err == nil }
	CloudctxVersion = probeVersionCached
	ForgetContextCache()
	t.Cleanup(func() {
		CloudctxInstalled, CloudctxVersion = prevInstalled, prevVersion
		ForgetContextCache()
	})
	return home
}

// pinVersion answers the version gate without consulting the host, for the
// tests that are about what the gate protects rather than about the probe.
func pinVersion(t *testing.T, version string) {
	t.Helper()
	prev := CloudctxVersion
	CloudctxVersion = func(Runner) (string, error) { return version, nil }
	ForgetContextCache()
	t.Cleanup(func() { CloudctxVersion = prev; ForgetContextCache() })
}

// TestContractListingIsTheOnlyListingAsked: `cloudctx list --names` is the
// declared listing. `_names` is internal to cloudctx and the human `list` is
// for people — parsing it is what once made pimctl act on a context called
// "no", from the sentence "no contexts. Create one with: …".
func TestContractListingIsTheOnlyListingAsked(t *testing.T) {
	pinVersion(t, MinCloudctxVersion)
	var asked [][]string
	run := func(_ string, args ...string) ([]byte, []byte, error) {
		asked = append(asked, args)
		return []byte("contoso\nglobex\n"), nil, nil
	}
	got, err := ListContexts(run)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "contoso" || got[1] != "globex" {
		t.Errorf("names = %v", got)
	}
	if len(asked) != 1 || strings.Join(asked[0], " ") != "list --names" {
		t.Errorf("cloudctx was asked %v; only `list --names` is contractual", asked)
	}
}

// TestEmptyRegistryIsNoNamesAndNoError: an empty registry prints nothing and
// exits 0, which is an answer rather than a failure.
func TestEmptyRegistryIsNoNamesAndNoError(t *testing.T) {
	pinVersion(t, MinCloudctxVersion)
	got, err := ListContexts(func(string, ...string) ([]byte, []byte, error) {
		return []byte(""), nil, nil
	})
	if err != nil || len(got) != 0 {
		t.Errorf("empty registry = %v, %v; want no contexts and no error", got, err)
	}
}

// TestListContextsWithoutCloudctx: the operating system saying the binary does
// not exist is not something to pass through to a user who may never have
// chosen to want it.
func TestListContextsWithoutCloudctx(t *testing.T) {
	prev := CloudctxInstalled
	CloudctxInstalled = func() bool { return false }
	t.Cleanup(func() { CloudctxInstalled = prev })

	_, err := ListContexts(func(string, ...string) ([]byte, []byte, error) {
		return nil, nil, &exec.Error{Name: "cloudctx", Err: exec.ErrNotFound}
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "cloudctx is not installed") {
		t.Errorf("the error does not explain the cause: %v", err)
	}
	if strings.Contains(err.Error(), "executable file not found") {
		t.Errorf("the raw exec failure leaked: %v", err)
	}
	// pimctl still works without it, and the message has to say so — otherwise
	// the reader concludes the tool needs something it does not.
	if !strings.Contains(err.Error(), "az login") {
		t.Errorf("the error should say what still works: %v", err)
	}
}

// TestAnOlderCloudctxIsRefusedOnce: pimctl drives cloudctx through the contract
// 1.4.0 declared. An older one is refused with one sentence naming the version
// needed, rather than degrading to the guesswork the contract replaced.
func TestAnOlderCloudctxIsRefusedOnce(t *testing.T) {
	pinVersion(t, "1.3.0")
	//nolint:unparam // the point is that it is never called at all.
	run := func(string, ...string) (stdout, stderr []byte, err error) {
		t.Error("nothing should be spawned once the version is known to be too old")
		return nil, nil, nil
	}
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"ListContexts", func() error { _, err := ListContexts(run); return err }()},
		{"ShowContext", func() error { _, err := ShowContext("contoso", run); return err }()},
		{"Acquire", func() error { _, err := Acquire("contoso", run); return err }()},
	} {
		if !errors.Is(tc.err, ErrCloudctxTooOld) {
			t.Errorf("%s: err = %v, want ErrCloudctxTooOld", tc.name, tc.err)
		}
		for _, want := range []string{"1.3.0", MinCloudctxVersion, "cloudctx self-update"} {
			if !strings.Contains(tc.err.Error(), want) {
				t.Errorf("%s: the error does not mention %q: %v", tc.name, want, tc.err)
			}
		}
	}
}

// TestTheBarePathIgnoresTheVersionGate: pimctl is an Azure CLI tool first. An
// out-of-date cloudctx says nothing about the shared `az login`, which never
// goes through cloudctx at all.
func TestTheBarePathIgnoresTheVersionGate(t *testing.T) {
	pinVersion(t, "1.3.0")
	jwt := fakeJWT(`{"oid":"oid-1","tid":"tid-1","upn":"someone@example.com"}`)
	tok, err := Acquire("", func(name string, _ ...string) ([]byte, []byte, error) {
		if name != "az" {
			t.Errorf("the bare path spawned %q", name)
		}
		return []byte(
			`{"accessToken":"` + jwt + `","expiresOn":"2099-01-01 00:00:00.000000","tenant":"tid-1"}`,
		), nil, nil
	})
	if err != nil || tok == nil {
		t.Fatalf("the bare path must work regardless of cloudctx: %v", err)
	}
}

// TestVersionComparison pins the ordering the gate decides on, including the
// suffixed builds a pre-release tag produces.
func TestVersionComparison(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.4.0", "1.4.0", 0},
		{"1.3.0", "1.4.0", -1},
		{"1.4.1", "1.4.0", 1},
		{"2.0", "1.4.0", 1},
		{"1.4", "1.4.0", 0},
		{"1.10.0", "1.4.0", 1},
		{"1.4.0rc1", "1.4.0", 0},
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// TestParseVersion reads the version out of what `cloudctx --version` prints.
func TestParseVersion(t *testing.T) {
	for in, want := range map[string]string{
		"cloudctx 1.4.0\n": "1.4.0",
		"cloudctx v1.4.0":  "1.4.0",
		"1.4.0":            "1.4.0",
		"cloudctx":         "",
		"":                 "",
	} {
		if got := parseVersion(in); got != want {
			t.Errorf("parseVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestUnreadableVersionIsAnError: a version pimctl cannot read is one it cannot
// vouch for, and guessing "new enough" would put the guesswork back.
func TestUnreadableVersionIsAnError(t *testing.T) {
	_, err := probeCloudctxVersion(func(string, ...string) ([]byte, []byte, error) {
		return []byte("cloudctx, the context switcher\n"), nil, nil
	})
	if err == nil || !strings.Contains(err.Error(), "cloudctx, the context switcher") {
		t.Errorf("the error should quote what cloudctx printed: %v", err)
	}
}

// TestContextTenantParsesCloudctxShow covers the shapes `cloudctx show` prints:
// a registry entry as key = value lines, then a blank line and the path lines,
// of which `store:` is the first.
func TestContextTenantParsesCloudctxShow(t *testing.T) {
	pinVersion(t, MinCloudctxVersion)
	cases := []struct {
		name        string
		out         string
		want, store string
	}{
		{
			name: "a full entry",
			out: "[contoso]\nazure_tenant = 11111111-2222-3333-4444-555555555555\n" +
				"display = Contoso\n\nstore:           /home/x/.cloudctx/contoso\n" +
				"azure store:     /home/x/.cloudctx/contoso/azure  (exists)\n",
			want:  "11111111-2222-3333-4444-555555555555",
			store: "/home/x/.cloudctx/contoso",
		},
		{name: "no azure_tenant", out: "[contoso]\ndisplay = Contoso\n"},
		{name: "quoted value", out: "[contoso]\nazure_tenant = \"tid-1\"\n", want: "tid-1"},
		{name: "no output at all"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ForgetContextCache()
			info, err := ShowContext("contoso", func(string, ...string) ([]byte, []byte, error) {
				return []byte(tc.out), nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if info.Tenant != tc.want {
				t.Errorf("tenant = %q, want %q", info.Tenant, tc.want)
			}
			if info.Store != tc.store {
				t.Errorf("store = %q, want %q", info.Store, tc.store)
			}
		})
	}

	// The shared az login is pinned to nothing, and asking cloudctx about it
	// would be asking about a context that does not exist.
	ran := false
	if _, err := ContextTenant("", func(string, ...string) ([]byte, []byte, error) {
		ran = true
		return nil, nil, nil
	}); err != nil || ran {
		t.Errorf("the bare path must not consult cloudctx (ran=%v, err=%v)", ran, err)
	}
}

// TestAmbientStoreCostsNoSpawn: inside a `cloudctx use` window the context's
// store is already in the environment, and a 0.65 s process launch to learn
// what the shell has been holding all along is 0.65 s wasted.
func TestAmbientStoreCostsNoSpawn(t *testing.T) {
	t.Setenv("CLOUDCTX_CONTEXT", "contoso")
	t.Setenv("CLOUDCTX_STORE", "/home/x/.cloudctx/contoso")
	ForgetContextCache()
	t.Cleanup(ForgetContextCache)

	dir, ok := ContextStateDir("contoso", func(string, ...string) ([]byte, []byte, error) {
		t.Error("the ambient store must not spawn cloudctx")
		return nil, nil, nil
	})
	if !ok || dir != filepath.Join(ambientStore("contoso"), "pimctl") {
		t.Errorf("ContextStateDir = %q, %v", dir, ok)
	}

	// Another context's store is not this one's: the two variables are read
	// together or the answer is the wrong tenant's directory.
	pinVersion(t, MinCloudctxVersion)
	asked := false
	if _, ok := ContextStateDir("globex", func(string, ...string) ([]byte, []byte, error) {
		asked = true
		return []byte("[globex]\n\nstore:           /home/x/.cloudctx/globex\n"), nil, nil
	}); !ok || !asked {
		t.Error("a context other than the ambient one must be looked up, not assumed")
	}
}

// TestContractStubDrivesTheRealPath runs the real exec path against a stub
// cloudctx implementing the contract literally: the listing, the store path,
// and the unknown-context error with its exact phrase and exit code.
func TestContractStubDrivesTheRealPath(t *testing.T) {
	home := stubCloudctx(t, MinCloudctxVersion, "acme", "globex")

	names, err := ListContexts(execRunner)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, ",") != "acme,globex" {
		t.Errorf("names = %v", names)
	}

	info, err := ShowContext("acme", execRunner)
	if err != nil {
		t.Fatal(err)
	}
	if info.Tenant != "tid-acme" || info.Store != filepath.Join(home, "acme") {
		t.Errorf("show = %+v", info)
	}

	// The version the gate compares against is read from the same binary, and
	// its format — `cloudctx X.Y.Z` — is what pimctl parses.
	version, err := CloudctxVersion(execRunner)
	if err != nil || version != MinCloudctxVersion {
		t.Errorf("version = %q, %v; want %q", version, err, MinCloudctxVersion)
	}

	dir, ok := ContextStateDir("acme", execRunner)
	if !ok || dir != filepath.Join(home, "acme", "pimctl") {
		t.Errorf("ContextStateDir = %q, %v; want the store's pimctl/ subdirectory", dir, ok)
	}

	// An unknown context: `cloudctx: error: unknown context '<name>'` on
	// stderr, nothing on stdout, exit 1. pimctl turns that into the hint that
	// the name is wrong rather than the session.
	_, err = ShowContext("nope", execRunner)
	if err == nil {
		t.Fatal("an unknown context must be an error")
	}
	for _, want := range []string{"no context called", "cloudctx new nope"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the hint does not mention %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "cloudctx login") {
		t.Errorf("a context that does not exist cannot be logged in to: %v", err)
	}
}

// TestContractStubTooOldIsRefused: the same stub reporting 1.3.0, which is what
// a machine that has not updated cloudctx since the contract looks like.
func TestContractStubTooOldIsRefused(t *testing.T) {
	stubCloudctx(t, "1.3.0", "acme")

	_, err := ListContexts(execRunner)
	if !errors.Is(err, ErrCloudctxTooOld) {
		t.Fatalf("err = %v, want ErrCloudctxTooOld", err)
	}
	if !strings.Contains(err.Error(), MinCloudctxVersion) {
		t.Errorf("the error must name the version required: %v", err)
	}
}

// TestTheVersionProbeIsCached: cloudctx is Python and a spawn costs ~0.65 s
// here, the same order as the token mint the cache layer exists to avoid. The
// probe therefore runs once per binary per day, not once per command.
func TestTheVersionProbeIsCached(t *testing.T) {
	stubCloudctx(t, MinCloudctxVersion, "acme")

	spawns := 0
	counting := func(name string, args ...string) ([]byte, []byte, error) {
		if len(args) == 1 && args[0] == "--version" {
			spawns++
		}
		return execRunner(name, args...)
	}
	for range 3 {
		if _, err := CloudctxVersion(counting); err != nil {
			t.Fatal(err)
		}
	}
	if spawns != 1 {
		t.Errorf("the version was probed %d times; the cache should make it once", spawns)
	}
}
