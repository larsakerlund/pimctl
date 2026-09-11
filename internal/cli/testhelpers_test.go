// Fixture and assertion helpers shared by the command tests: JSON probing,
// the fake cloudctx runner, token fixtures and the deadline overrides. The
// fake ARM itself is in fake_test.go.

package cli

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/larsakerlund/pimctl/internal/azauth"
	"github.com/larsakerlund/pimctl/internal/config"
	"github.com/larsakerlund/pimctl/internal/store"
)

// mustObject reads m[key] as a JSON object, failing the test if it is anything
// else. Only a test's own goroutine may call it.
func mustObject(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	v, ok := m[key].(map[string]any)
	if !ok {
		t.Fatalf("%s is not a JSON object: %#v", key, m[key])
	}
	return v
}

// mustText reads m[key] as a string, failing the test if it is anything else.
func mustText(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	v, ok := m[key].(string)
	if !ok {
		t.Fatalf("%s is not a string: %#v", key, m[key])
	}
	return v
}

// roundTripJSON marshals v and decodes it back into a generic envelope, so a
// test can assert on the exact key set that would go on the wire.
func roundTripJSON(t *testing.T, v any) map[string]map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshalling the request body: %v", err)
	}
	var envelope map[string]map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decoding the request body: %v", err)
	}
	return envelope
}

// writeJSON renders a stub response from the fake server's goroutine, where
// only Errorf is safe.
func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("writing the stub response: %v", err)
	}
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

// fakeTestJWT builds an unsigned JWT carrying the given claims. No real token
// is ever used in tests.
func fakeTestJWT(payload string) string {
	seg := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	return seg(`{"alg":"none"}`) + "." + seg(payload) + ".sig"
}

// installNoCloudctx makes every cloudctx invocation fail the way the operating
// system fails one: the binary is not there. az still answers, because pimctl
// on a machine without cloudctx is a supported configuration rather than a
// degraded one.
func installNoCloudctx(t *testing.T) {
	t.Helper()
	installCloudctxPresence(t, false)

	prev := azauth.DefaultRunner
	azauth.DefaultRunner = func(name string, args ...string) ([]byte, []byte, error) {
		if name == "cloudctx" {
			return nil, nil, &exec.Error{Name: "cloudctx", Err: exec.ErrNotFound}
		}
		if !strings.Contains(name+" "+strings.Join(args, " "), "get-access-token") {
			t.Errorf("test tried to run a real command: %s %s", name, strings.Join(args, " "))
			return nil, nil, errors.New("exec not permitted in tests")
		}
		expiry := time.Now().Add(time.Hour).Format("2006-01-02 15:04:05.000000")
		jwt := fakeTestJWT(`{"oid":"oid-1","tid":"tid-1","upn":"test@example.com"}`)
		return []byte(`{"accessToken":"` + jwt + `","expiresOn":"` + expiry + `","tenant":"tid-1"}`), nil, nil
	}
	t.Cleanup(func() { azauth.DefaultRunner = prev })
}

// installCloudctxPresence pins whether cloudctx looks installed for one test,
// restoring the package default afterwards. Tests call it through
// [installNoCloudctx], or directly when they need to say "present" explicitly
// rather than lean on the default.
func installCloudctxPresence(t *testing.T, present bool) {
	t.Helper()
	prev := azauth.CloudctxInstalled
	azauth.CloudctxInstalled = func() bool { return present }
	t.Cleanup(func() { azauth.CloudctxInstalled = prev })
}

// installFakeRunner replaces the process runner for the duration of a test.
//
// No unit test may shell out to a real cloudctx or az: CI has neither, and on a
// developer's machine a real cloudctx would make the result depend on which
// contexts that person happens to have configured. Every exec goes through
// azauth.DefaultRunner, so replacing it here closes the door for good — an
// unexpected command is a test failure, not a silent fallthrough to the host.
func installFakeRunner(t *testing.T, contexts []string) {
	t.Helper()
	azauth.ForgetContextCache()
	prevHome := testCloudctxHome
	testCloudctxHome = filepath.Join(t.TempDir(), "cloudctx")
	writeFakeProfiles(t, contexts)
	t.Cleanup(func() {
		testCloudctxHome = prevHome
		azauth.ForgetContextCache()
	})
	prev := azauth.DefaultRunner
	azauth.DefaultRunner = func(name string, args ...string) ([]byte, []byte, error) {
		joined := name + " " + strings.Join(args, " ")
		switch {
		case name == "cloudctx" && len(args) == 1 && args[0] == "--version":
			return []byte("cloudctx " + azauth.MinCloudctxVersion + "\n"), nil, nil
		case name == "cloudctx" && len(args) == 2 && args[0] == "list" && args[1] == "--names":
			// The contract listing: bare names, one per line, nothing else.
			var b strings.Builder
			for _, c := range contexts {
				if c != "" {
					b.WriteString(c + "\n")
				}
			}
			return []byte(b.String()), nil, nil
		case name == "cloudctx" && len(args) > 1 && args[0] == "show":
			// The registry read behind the tenant and store lookups: local, no
			// az spawn. `store:` is the first path line, as the contract says.
			return fmt.Appendf(nil,
				"[%s]\nazure_tenant = tid-1\n\nstore:           %s\nazure store:     %s/azure  (exists)\n",
				args[1], fakeContextStore(args[1]), fakeContextStore(args[1])), nil, nil
		case strings.Contains(joined, "get-access-token"):
			expiry := time.Now().Add(time.Hour).Format("2006-01-02 15:04:05.000000")
			jwt := fakeTestJWT(`{"oid":"oid-1","tid":"tid-1","upn":"test@example.com"}`)
			return []byte(`{"accessToken":"` + jwt + `","expiresOn":"` + expiry + `","tenant":"tid-1"}`), nil, nil
		default:
			t.Errorf("test tried to run a real command: %s", joined)
			return nil, []byte("not available in tests"), errors.New("exec not permitted in tests")
		}
	}
	t.Cleanup(func() { azauth.DefaultRunner = prev })
}

// writeFakeProfiles provides the selected account for each fake context.
func writeFakeProfiles(t *testing.T, contexts []string) {
	t.Helper()
	for _, name := range contexts {
		if name == "" {
			continue
		}
		dir := filepath.Join(fakeContextStore(name), "azure")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeFile(
			filepath.Join(dir, "azureProfile.json"),
			`{"subscriptions":[{"tenantId":"tid-1","isDefault":true,"user":{"name":"test@example.com","type":"user"}}]}`,
		); err != nil {
			t.Fatal(err)
		}
	}
}

// testCloudctxHome stands in for ~/.cloudctx: the root the fake registry's
// per-context stores hang off, so a test's tokens and activation records land
// where the companion contract puts them instead of in the developer's real
// cloudctx home.
//
// [installFakeRunner] gives each test its own, for the same reason each test
// gets its own XDG directories: per-context state is now shared between the
// token cache and the activation record, and a root shared between tests let
// one test's activations show up in another's `status`. TestMain's value is the
// fallback for a test that writes per-context state without installing a fake
// registry at all.
var testCloudctxHome string

// fakeContextStore is one context's store directory in the test tree, the value
// the fake `cloudctx show` reports on its `store:` line.
func fakeContextStore(context string) string {
	return filepath.Join(testCloudctxHome, context)
}

// readTokenCache reads a cached token, failing the test on a permissions error
// so call sites need not discard it.
func readTokenCache(t *testing.T, context string) *azauth.Token {
	t.Helper()
	tok, _, err := azauth.ReadTokenCache(context, "", azauth.TokenCacheMargin, azauth.DefaultRunner)
	if err != nil {
		t.Fatalf("ReadTokenCache(%q): %v", context, err)
	}
	return tok
}

// shortDeadlines collapses both per-scope deadlines so a test does not sit out
// the real ones.
func shortDeadlines(t *testing.T, d time.Duration) {
	t.Helper()
	tm := defaultTimeouts()
	tm.scopeSoftDeadline, tm.waitScope = d, d
	installTimeouts(t, tm)
}

// pastCeiling collapses the ceiling on how long this machine's own record may
// outrank ARM, for tests about what happens once that backstop expires.
func pastCeiling(t *testing.T) {
	t.Helper()
	prev := recordCeiling
	recordCeiling = -1
	t.Cleanup(func() { recordCeiling = prev })
}

// TestMain points every XDG directory at a temporary tree before a single test
// runs.
//
// Individual tests still isolate themselves — t.Setenv is the readable, local
// way to say what a test depends on — but relying on that alone is one
// forgotten line away from a test writing into the developer's real
// ~/.local/state/pimctl, which is exactly what happened: the activation record
// grew phantom entries that then showed up in their next `pimctl status`. A
// test that touches the real home is a bug even when it passes, so the package
// makes it impossible rather than remembering not to.
func TestMain(m *testing.M) {
	// Whether cloudctx looks installed is pinned for the whole package, for the
	// same reason the exec runner is: a test that asks PATH asserts on the
	// machine it happens to run on. Present is the default because that is the
	// machine most of these tests describe; installNoCloudctx says otherwise.
	azauth.CloudctxInstalled = func() bool { return true }
	// Likewise the version: the real probe runs `cloudctx --version` against
	// whichever cloudctx the machine has, and a test that consults the host
	// asserts on the host. The default is a cloudctx that meets the contract;
	// installOldCloudctx says otherwise.
	azauth.CloudctxVersion = func(azauth.Runner) (string, error) { return azauth.MinCloudctxVersion, nil }
	// And no test may reach a real cloudctx or az even by omission: a test that
	// writes per-context state without installing a fake registry would
	// otherwise ask the developer's own cloudctx where that state belongs.
	azauth.DefaultRunner = func(string, ...string) ([]byte, []byte, error) {
		return nil, []byte("not available in tests"), errors.New("exec not permitted in tests")
	}

	root, err := os.MkdirTemp("", "pimctl-test-xdg-")
	if err != nil {
		panic("cannot create the test XDG root: " + err.Error())
	}
	// Where the fake registry's context stores live, so per-context state goes
	// somewhere disposable rather than into a real ~/.cloudctx.
	testCloudctxHome = filepath.Join(root, "cloudctx")
	for _, v := range []string{"XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME"} {
		if err := os.Setenv(v, filepath.Join(root, v)); err != nil {
			panic("cannot set " + v + ": " + err.Error())
		}
	}
	code := m.Run()
	if err := os.RemoveAll(root); err != nil {
		// Nothing left to fail: the tests are done and the tree is in TMPDIR.
		fmt.Fprintf(os.Stderr, "warning: cannot remove %s: %v\n", root, err)
	}
	os.Exit(code)
}

// heldEntries returns the activations the record believes are still held —
// everything readRecord returns except the tombstones, which record the
// opposite.
//
// It lives here because only tests ask the question in this shape: the code
// filters tombstones where it renders rows, so a package-level helper nothing
// shipped called was production code in name only. The context is fixed at
// contoso, the only one the record tests use.
func heldEntries() []recordEntry {
	entries := readRecord(testOwner("contoso"))
	held := make([]recordEntry, 0, len(entries))
	for _, e := range entries {
		if !e.Revoked() {
			held = append(held, e)
		}
	}
	return held
}

// writeRecordFileForTest writes a record file verbatim, without the pruning
// [writeRecord] does, so a test can set up a file whose entries have all
// expired — which is what a record left behind by yesterday's work looks like.
func writeRecordFileForTest(t *testing.T, context string, entries []recordEntry) {
	t.Helper()
	path, err := recordPath(testOwner(context))
	if err != nil {
		t.Fatal(err)
	}
	blob, err := json.Marshal(recordFile{Version: recordVersion, Owner: testOwner(context), Entries: entries})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Dir(path), store.DirMode); err != nil {
		t.Fatal(err)
	}
	store.WriteAtomic(path, blob)
}

// writePreset saves a one-role preset naming a context, for tests about what
// happens when that context cannot be opened.
func writePreset(t *testing.T, name, context string) {
	t.Helper()
	ps := config.Presets{}
	ps.Set(name, []config.PresetEntry{{
		Context:          context,
		RoleName:         "Cost Management Contributor",
		ScopeName:        "Contoso landing zones",
		Scope:            "/providers/Microsoft.Management/managementGroups/contoso-prod",
		RoleDefinitionID: "/providers/Microsoft.Authorization/roleDefinitions/" + costGUID,
	}})
	if err := config.SavePresets(&ps); err != nil {
		t.Fatal(err)
	}
}

// writeFileForTest creates dir and writes one file into it, owner-only.
func writeFileForTest(t *testing.T, dir, name, content string) error {
	t.Helper()
	if err := os.MkdirAll(dir, store.DirMode); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name), []byte(content), store.SecretFileMode)
}

// farFutureExpiry is an az-style expiry an hour out, for a token a test wants
// treated as usable.
func farFutureExpiry() string {
	return time.Now().Add(time.Hour).Format("2006-01-02 15:04:05.000000")
}

// testOwner is the account the fake ARM session authenticates as.
func testOwner(name string) store.Owner {
	if name == "(default)" {
		name = ""
	}
	return store.Owner{Context: name, TenantID: "tid-1", PrincipalID: "oid-1"}
}
