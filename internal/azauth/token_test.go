// Tests for token.go: claim decoding, the argv handed to cloudctx/az, the
// parsing of `az account get-access-token` and the environment a child is
// given. The runner is always a fake, so nothing here logs in or reaches the
// network. Cache behaviour is covered in tokencache_test.go, and everything
// pimctl asks cloudctx itself in cloudctx_test.go.

package azauth

import (
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeJWT builds an unsigned JWT with the given payload JSON. No real token is
// ever used in tests.
func fakeJWT(payload string) string {
	seg := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	return seg(`{"alg":"none","typ":"JWT"}`) + "." + seg(payload) + ".signature"
}

// TestMain pins whether cloudctx looks installed, for every test in this
// package.
//
// CloudctxInstalled asks PATH, and a test that asks the host asserts on the
// machine it happens to run on: the same test passed on a developer's laptop
// and failed in CI, which has no cloudctx. Present is the default because most
// of these tests are about the cloudctx path; a test about its absence says so
// with installNoCloudctx.
func TestMain(m *testing.M) {
	CloudctxInstalled = func() bool { return true }
	// The version is pinned for the same reason: the real probe runs
	// `cloudctx --version` against whichever cloudctx the machine has, and one
	// too old for the contract would fail every test here for a reason that has
	// nothing to do with what they cover. Tests about the gate itself pin their
	// own answer.
	CloudctxVersion = func(Runner) (string, error) { return MinCloudctxVersion, nil }
	os.Exit(m.Run())
}

// noCloudctx is a runner for the tests that are not about cloudctx at all: it
// answers every cloudctx invocation the way the operating system answers one on
// a machine without it. Per-context state then stays in pimctl's own
// directories, which is what those tests assert on.
func noCloudctx(string, ...string) (stdout, stderr []byte, err error) {
	return nil, nil, &exec.Error{Name: "cloudctx", Err: exec.ErrNotFound}
}

func TestDecodeClaims(t *testing.T) {
	// Claim values mirror the shape of the real ARM token in the probe report.
	jwt := fakeJWT(
		`{"aud":"https://management.azure.com/","oid":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","tid":"11111111-2222-3333-4444-555555555555","appid":"04b07795-8ddb-461a-bbee-02f9e1bf7b46","scp":"user_impersonation"}`,
	)
	c, err := DecodeClaims(jwt)
	if err != nil {
		t.Fatalf("DecodeClaims: %v", err)
	}
	if c.OID != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Errorf("oid = %q", c.OID)
	}
	if c.TID != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("tid = %q", c.TID)
	}
	if c.AUD != "https://management.azure.com/" {
		t.Errorf("aud = %q", c.AUD)
	}
}

func TestDecodeClaimsPaddedSegment(t *testing.T) {
	// Some issuers emit standard-padded base64url. Both must decode.
	payload := `{"oid":"11111111-2222-3333-4444-555555555555","tid":"t"}`
	jwt := "h." + base64.URLEncoding.EncodeToString([]byte(payload)) + ".s"
	c, err := DecodeClaims(jwt)
	if err != nil {
		t.Fatalf("DecodeClaims: %v", err)
	}
	if c.OID != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("oid = %q", c.OID)
	}
}

// TestDecodeClaimsRejectsTruncatedJWT: a two-part string is not a JWT. The
// guard used to admit it — parts[1] happens to be in range — so a truncated
// token was reported as unparseable JSON instead of as the wrong shape.
func TestDecodeClaimsRejectsTruncatedJWT(t *testing.T) {
	_, err := DecodeClaims("header.payload")
	if err == nil {
		t.Fatal("a two-part token is not a JWT and must be refused")
	}
	if !strings.Contains(err.Error(), "not a JWT") {
		t.Errorf("error should say what is wrong with the shape: %v", err)
	}
}

func TestDecodeClaimsRejectsNonJWT(t *testing.T) {
	if _, err := DecodeClaims("not-a-jwt"); err == nil {
		t.Fatal("expected an error for a non-JWT")
	}
}

func TestTokenArgs(t *testing.T) {
	name, args := TokenArgs("contoso")
	if name != "cloudctx" {
		t.Fatalf("name = %q, want cloudctx", name)
	}
	joined := strings.Join(args, " ")
	want := "exec contoso -- az account get-access-token --resource https://management.azure.com/ -o json"
	if joined != want {
		t.Fatalf("args = %q\nwant   %q", joined, want)
	}

	name, args = TokenArgs("")
	if name != "az" {
		t.Fatalf("name = %q, want az (bare az when no context is given)", name)
	}
	if strings.Contains(strings.Join(args, " "), "cloudctx") {
		t.Fatal("bare az invocation must not mention cloudctx")
	}
}

func TestAcquire(t *testing.T) {
	jwt := fakeJWT(`{"oid":"oid-1","tid":"tid-1"}`)
	run := func(name string, _ ...string) ([]byte, []byte, error) {
		if name != "cloudctx" {
			t.Fatalf("expected cloudctx, got %q", name)
		}
		return []byte(
			`{"accessToken":"` + jwt + `","expiresOn":"2026-09-04 12:00:00.000000","tenant":"tid-1"}`,
		), nil, nil
	}
	tok, err := Acquire("contoso", run)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if tok.PrincipalID != "oid-1" || tok.TenantID != "tid-1" {
		t.Fatalf("claims not decoded: %+v", struct{ P, T string }{tok.PrincipalID, tok.TenantID})
	}
	if tok.Label() != "contoso" {
		t.Errorf("Label() = %q", tok.Label())
	}
}

func TestAcquireSurfacesStderrVerbatim(t *testing.T) {
	stderr := "ERROR: Please run 'az login' to setup account."
	run := func(string, ...string) ([]byte, []byte, error) {
		return nil, []byte(stderr), errors.New("exit status 1")
	}
	_, err := Acquire("contoso", run)
	if err == nil {
		t.Fatal("expected an error")
	}
	var ee *ExecError
	if !errors.As(err, &ee) {
		t.Fatalf("expected an *ExecError, got %T", err)
	}
	if !strings.Contains(err.Error(), stderr) {
		t.Errorf("stderr not shown verbatim: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "cloudctx login contoso") {
		t.Errorf("missing the login hint: %q", err.Error())
	}
}

func TestListContexts(t *testing.T) {
	run := func(string, ...string) ([]byte, []byte, error) {
		return []byte("  contoso\n  globex\n\n"), nil, nil
	}
	names, err := ListContexts(run)
	if err != nil {
		t.Fatalf("ListContexts: %v", err)
	}
	if len(names) != 2 || names[0] != "contoso" || names[1] != "globex" {
		t.Fatalf("names = %v", names)
	}
}

func TestTokenNeverAppearsInExecError(t *testing.T) {
	// A failing invocation must not echo anything token-shaped.
	run := func(string, ...string) ([]byte, []byte, error) {
		return []byte("secret-token-material"), []byte("boom"), errors.New("exit status 1")
	}
	_, err := Acquire("contoso", run)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "secret-token-material") {
		t.Fatal("stdout (which carries the token) leaked into the error message")
	}
}

// TestAcquireIgnoresStderrNoiseOnSuccess pins the contract that a zero exit code
// is success regardless of what az wrote to stderr. Azure CLI extensions emit
// warnings there routinely — an azure-devops extension in this environment
// prints "No module named 'pkg_resources'" on every call — and treating that as
// a failure would break every token acquisition.
func TestAcquireIgnoresStderrNoiseOnSuccess(t *testing.T) {
	jwt := fakeJWT(`{"oid":"oid-1","tid":"tid-1"}`)
	noise := "ERROR: Failed to load command module 'azure-devops': No module named 'pkg_resources'\n"
	run := func(string, ...string) ([]byte, []byte, error) {
		return []byte(`{"accessToken":"` + jwt + `","expiresOn":"2026-09-04 12:00:00.000000","tenant":"tid-1"}`),
			[]byte(noise), nil // Exit 0 despite the stderr output.
	}
	tok, err := Acquire("contoso", run)
	if err != nil {
		t.Fatalf("stderr noise on a zero exit must not fail the call: %v", err)
	}
	if tok.PrincipalID != "oid-1" || tok.TenantID != "tid-1" {
		t.Fatalf("token not parsed from stdout: %+v", struct{ P, T string }{tok.PrincipalID, tok.TenantID})
	}
}

func TestListContextsIgnoresStderrNoiseOnSuccess(t *testing.T) {
	run := func(string, ...string) ([]byte, []byte, error) {
		return []byte("  contoso\n  globex\n"), []byte("WARNING: something irrelevant\n"), nil
	}
	names, err := ListContexts(run)
	if err != nil {
		t.Fatalf("stderr noise on a zero exit must not fail the call: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("names = %v", names)
	}
}

// TestAcquireToleratesWarningOnStdout covers the nastier variant: a CLI
// extension printing its warning on stdout, ahead of the JSON.
func TestAcquireToleratesWarningOnStdout(t *testing.T) {
	jwt := fakeJWT(`{"oid":"oid-2","tid":"tid-2"}`)
	run := func(string, ...string) ([]byte, []byte, error) {
		return []byte("WARNING: an extension said something on stdout\n" +
			`{"accessToken":"` + jwt + `","expiresOn":"x","tenant":"tid-2"}` + "\ntrailing noise\n"), nil, nil
	}
	tok, err := Acquire("contoso", run)
	if err != nil {
		t.Fatalf("a warning around the JSON should not break parsing: %v", err)
	}
	if tok.PrincipalID != "oid-2" {
		t.Fatalf("oid = %q", tok.PrincipalID)
	}
}

// TestAcquireParseFailureMentionsStderr: when stdout really is unusable, the
// error should surface what az said on stderr, since that usually explains it.
func TestAcquireParseFailureMentionsStderr(t *testing.T) {
	run := func(string, ...string) ([]byte, []byte, error) {
		return []byte("not json at all"), []byte("ERROR: Failed to load command module 'azure-devops'"), nil
	}
	_, err := Acquire("contoso", run)
	if err == nil {
		t.Fatal("expected a parse error")
	}
	if !strings.Contains(err.Error(), "azure-devops") {
		t.Errorf("the stderr hint is missing from the diagnostic: %q", err.Error())
	}
}

// TestExecErrorHintMatchesTheFailure: the hint is the sentence a user acts on,
// and telling someone to log in to a context that does not exist sends them at
// the wrong problem — the name is wrong, not the session.
func TestExecErrorHintMatchesTheFailure(t *testing.T) {
	cases := []struct {
		name   string
		err    *ExecError
		want   []string
		unwant []string
	}{
		{
			name: "unknown context",
			err: &ExecError{
				Context: "does-not-exist", Cmd: "cloudctx exec does-not-exist -- az …",
				Stderr: "cloudctx: error: unknown context 'does-not-exist'",
				Err:    errors.New("exit status 1"),
			},
			want:   []string{"no context called", "cloudctx list", "cloudctx new does-not-exist"},
			unwant: []string{"cloudctx login"},
		},
		{
			name: "an expired login",
			err: &ExecError{
				Context: "contoso", Cmd: "cloudctx exec contoso -- az …",
				Stderr: "AADSTS700082: The refresh token has expired",
				Err:    errors.New("exit status 1"),
			},
			want:   []string{"may not be logged in", "cloudctx login contoso"},
			unwant: []string{"cloudctx new"},
		},
		{
			name: "the shared az login",
			err: &ExecError{
				Cmd:    "az account get-access-token …",
				Stderr: "Please run 'az login' to setup account.",
				Err:    errors.New("exit status 1"),
			},
			want:   []string{"az login"},
			unwant: []string{"cloudctx"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.err.Error()
			// az's own words come first, whatever the hint says.
			if !strings.Contains(got, tc.err.Stderr) {
				t.Errorf("the underlying stderr was dropped:\n%s", got)
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("message is missing %q:\n%s", want, got)
				}
			}
			for _, unwant := range tc.unwant {
				if strings.Contains(got, unwant) {
					t.Errorf("message should not mention %q:\n%s", unwant, got)
				}
			}
		})
	}
}

// TestChildEnvStripsCloudctxVarsForBareAz: a bare `az` is the shared-login
// path, and inside a `cloudctx use` window it would otherwise inherit that
// context's AZURE_CONFIG_DIR — minting the context's token and caching it under
// the shared-login slot, where a later unscoped run reuses it.
func TestChildEnvStripsCloudctxVarsForBareAz(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin",
		"HOME=/Users/someone",
		"CLOUDCTX_CONTEXT=contoso",
		"CLOUDCTX_AZURE_LABEL=Prod",
		"AZURE_CONFIG_DIR=/Users/someone/.cloudctx/contoso/azure",
		"AWS_CONFIG_FILE=/Users/someone/.cloudctx/contoso/aws/config",
		"AWS_SHARED_CREDENTIALS_FILE=/Users/someone/.cloudctx/contoso/aws/credentials",
		"AWS_PROFILE=contoso",
		"AZURE_CORE_ONLY_SHOW_ERRORS=true",
	}

	got := childEnv("az", parent)
	for _, banned := range cloudctxVars {
		for _, kv := range got {
			if strings.HasPrefix(kv, banned+"=") {
				t.Errorf("a bare az inherited %s", kv)
			}
		}
	}
	// Everything else survives: pimctl is removing a context, not sanitising
	// the user's environment.
	for _, want := range []string{"PATH=/usr/bin", "HOME=/Users/someone", "AZURE_CORE_ONLY_SHOW_ERRORS=true"} {
		if !slices.Contains(got, want) {
			t.Errorf("%q was removed and should not have been", want)
		}
	}

	// Anything routed through cloudctx keeps the parent environment: cloudctx
	// builds the child's environment itself.
	if got := childEnv("cloudctx", parent); !slices.Equal(got, parent) {
		t.Errorf("cloudctx should be spawned with the parent environment, got %v", got)
	}
}

// TestExecRunnerAppliesTheChildEnv proves the stripping reaches the process,
// not just the helper: a stand-in `az` on PATH reports what it was given.
func TestExecRunnerAppliesTheChildEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "az")
	// Executable, because the point is to spawn it; the directory is the test's.
	script := []byte("#!/bin/sh\nprintenv\n")
	if err := os.WriteFile(path, script, 0o700); err != nil { //nolint:gosec // G306: see above
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AZURE_CONFIG_DIR", filepath.Join(dir, "contoso", "azure"))
	t.Setenv("CLOUDCTX_CONTEXT", "contoso")

	stdout, stderr, err := execRunner("az", "account", "get-access-token")
	if err != nil {
		t.Fatalf("running the stand-in az: %v (stderr %s)", err, stderr)
	}
	env := string(stdout)
	for _, banned := range cloudctxVars {
		if strings.Contains(env, banned+"=") {
			t.Errorf("the child saw %s:\n%s", banned, env)
		}
	}
	if !strings.Contains(env, "PATH=") {
		t.Errorf("the child lost its PATH:\n%s", env)
	}
}

// TestMissingAzIsReportedAsMissingAz: the same rule for the tool pimctl really
// does require.
func TestMissingAzIsReportedAsMissingAz(t *testing.T) {
	e := &ExecError{
		Cmd: "az account get-access-token …",
		Err: &exec.Error{Name: "az", Err: exec.ErrNotFound},
	}
	if !strings.Contains(e.Error(), "Azure CLI is not installed") {
		t.Errorf("a missing az should say so: %v", e)
	}
	if strings.Contains(e.Error(), "cloudctx") {
		t.Errorf("a missing az is not about cloudctx: %v", e)
	}
}
