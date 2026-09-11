// Tests for tokencache.go: the round trip, the expiry margin, the refusal to
// read a file whose mode is wider than 0600, and the rule that every other
// defect is a miss rather than an error. Each test redirects XDG_CACHE_HOME at
// a temporary directory, so the developer's real cache is never touched.

package azauth

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// readCache reads an entry and fails the test on a permissions error, so no
// call site has to discard the error to stay readable.
func readCache(t *testing.T, context, wantTenant string) *Token {
	t.Helper()
	tok, _, err := ReadTokenCache(context, wantTenant, TokenCacheMargin, noCloudctx)
	if err != nil {
		t.Fatalf("ReadTokenCache(%q): %v", context, err)
	}
	return tok
}

func cacheToken(t *testing.T, context string, expiresIn time.Duration) *Token {
	t.Helper()
	return &Token{
		Context:           context,
		AccessToken:       fakeJWT(`{"oid":"oid-1","tid":"tid-1","upn":"ada@example.com"}`),
		ExpiresOn:         time.Now().Add(expiresIn).Format("2006-01-02 15:04:05.000000"),
		Tenant:            "tid-1",
		PrincipalID:       "oid-1",
		TenantID:          "tid-1",
		UserPrincipalName: "ada@example.com",
	}
}

func TestTokenCacheRoundTrip(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	got, ok, err := ReadTokenCache("contoso", "", TokenCacheMargin, noCloudctx)
	if err != nil || ok {
		t.Fatalf("a fresh cache dir should miss cleanly, got (%v, %v, %v)", got, ok, err)
	}

	tok := cacheToken(t, "contoso", time.Hour)
	WriteTokenCache(tok, noCloudctx)

	got, ok, err = ReadTokenCache("contoso", "", TokenCacheMargin, noCloudctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("the token just written should be readable")
	}
	if got.AccessToken != tok.AccessToken || got.PrincipalID != "oid-1" || got.UserPrincipalName != "ada@example.com" {
		t.Fatalf("round trip lost data: %+v", got)
	}
	if !got.FromCache {
		t.Error("a cached token must be marked FromCache so --debug can say so")
	}

	// A different context must not see it.
	if got := readCache(t, "globex", ""); got != nil {
		t.Error("the token cache must be per-context")
	}
	// Nor a different tenant in the same context.
	if got := readCache(t, "contoso", "some-other-tenant"); got != nil {
		t.Error("a context that now points at another tenant must not reuse the old token")
	}
	if got := readCache(t, "contoso", "TID-1"); got == nil {
		t.Error("the tenant check should be case-insensitive")
	}
}

// TestTokenCacheExpiryMargin: a token about to expire is worse than none, so it
// must not be served.
func TestTokenCacheExpiryMargin(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	WriteTokenCache(cacheToken(t, "contoso", TokenCacheMargin+2*time.Minute), noCloudctx)
	if got := readCache(t, "contoso", ""); got == nil {
		t.Error("a token with more than the margin left should be reused")
	}

	// Inside the margin.
	WriteTokenCache(cacheToken(t, "contoso", TokenCacheMargin-time.Minute), noCloudctx)
	if got := readCache(t, "contoso", ""); got != nil {
		t.Error("a token expiring within the margin must not be reused")
	}

	// Already expired.
	WriteTokenCache(cacheToken(t, "contoso", -time.Minute), noCloudctx)
	if got := readCache(t, "contoso", ""); got != nil {
		t.Error("an expired token must not be reused")
	}

	// Unparseable expiry is treated as expired, never as valid forever.
	tok := cacheToken(t, "contoso", time.Hour)
	tok.ExpiresOn = "not a time"
	WriteTokenCache(tok, noCloudctx)
	if got := readCache(t, "contoso", ""); got != nil {
		t.Error("an unparseable expiry must be treated as expired")
	}
}

// TestTokenCacheRefusesWidePermissions: a token readable by another account is
// worse than no cache at all, so reading one is an error, not a miss.
func TestTokenCacheRefusesWidePermissions(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir)
	WriteTokenCache(cacheToken(t, "contoso", time.Hour), noCloudctx)

	path, err := TokenCachePath("contoso", noCloudctx)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("token cache written with mode %#o, want 0600", perm)
	}
	if di, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	} else if perm := di.Mode().Perm(); perm != 0o700 {
		t.Fatalf("cache directory mode %#o, want 0700", perm)
	}

	// Deliberately widening the mode is the point of this test: a token another
	// account can read must be refused, not served.
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o666} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		got, ok, err := ReadTokenCache("contoso", "", TokenCacheMargin, noCloudctx)
		if got != nil || ok {
			t.Errorf("mode %#o: a world- or group-readable token was served", mode)
		}
		if !errors.Is(err, ErrCachePermissions) {
			t.Errorf("mode %#o: got err %v, want ErrCachePermissions", mode, err)
		}
	}

	// Narrower than 0600 is fine.
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	if got := readCache(t, "contoso", ""); got == nil {
		t.Error("a 0400 cache file should still be usable")
	}
}

func TestTokenCacheCorruptAndMissingAreMisses(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	WriteTokenCache(cacheToken(t, "contoso", time.Hour), noCloudctx)
	path, err := TokenCachePath("contoso", noCloudctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, ok, err := ReadTokenCache("contoso", "", TokenCacheMargin, noCloudctx)
	if got != nil || ok || err != nil {
		t.Errorf("corrupt cache should be a silent miss, got (%v, %v, %v)", got, ok, err)
	}
}

func TestDropAndClearTokenCache(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	WriteTokenCache(cacheToken(t, "contoso", time.Hour), noCloudctx)
	WriteTokenCache(cacheToken(t, "globex", time.Hour), noCloudctx)

	DropTokenCache("contoso", noCloudctx)
	if got := readCache(t, "contoso", ""); got != nil {
		t.Error("DropTokenCache should have removed the entry")
	}
	if got := readCache(t, "globex", ""); got == nil {
		t.Error("DropTokenCache removed the wrong context")
	}

	WriteTokenCache(cacheToken(t, "contoso", time.Hour), noCloudctx)
	n, err := ClearTokenCache(noCloudctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("ClearTokenCache removed %d entries, want 2", n)
	}
	for _, c := range []string{"contoso", "globex"} {
		if got := readCache(t, c, ""); got != nil {
			t.Errorf("%s survived ClearTokenCache", c)
		}
	}
}

func TestAcquireCachedUsesTheCacheThenTheRunner(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	contextStore := t.TempDir()
	writeAzProfile(t, filepath.Join(contextStore, "azure"), "tid-1", "someone@example.com")
	ForgetContextCache()
	t.Cleanup(ForgetContextCache)

	// mints counts token acquisitions only. `cloudctx show` is a registry read
	// that costs no az spawn, which is what makes checking the tenant on every
	// cache read affordable.
	mints := 0
	jwt := fakeJWT(`{"oid":"oid-1","tid":"tid-1","upn":"someone@example.com"}`)
	expiry := time.Now().Add(time.Hour).Format("2006-01-02 15:04:05.000000")
	run := func(_ string, args ...string) ([]byte, []byte, error) {
		if len(args) > 0 && args[0] == "show" {
			return []byte("[contoso]\nazure_tenant = tid-1\nstore: " + contextStore + "\n"), nil, nil
		}
		mints++
		return []byte(`{"accessToken":"` + jwt + `","expiresOn":"` + expiry + `","tenant":"tid-1"}`), nil, nil
	}

	first, err := AcquireCached("contoso", run, false)
	if err != nil {
		t.Fatal(err)
	}
	if mints != 1 || first.FromCache {
		t.Fatalf("the first call should mint: mints=%d fromCache=%v", mints, first.FromCache)
	}
	if first.UserPrincipalName != "someone@example.com" {
		t.Errorf("upn not decoded: %q", first.UserPrincipalName)
	}

	second, err := AcquireCached("contoso", run, false)
	if err != nil {
		t.Fatal(err)
	}
	if mints != 1 {
		t.Errorf("the second call should have hit the cache, but az ran %d times", mints)
	}
	if !second.FromCache {
		t.Error("the second token should be marked FromCache")
	}

	// refresh bypasses the cache.
	if _, err = AcquireCached("contoso", run, true); err != nil {
		t.Fatal(err)
	}
	if mints != 2 {
		t.Errorf("--refresh should have re-minted, az ran %d times", mints)
	}
}

func TestTokenNeverAppearsInCachePathOrErrors(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	tok := cacheToken(t, "contoso", time.Hour)
	WriteTokenCache(tok, noCloudctx)
	path, err := TokenCachePath("contoso", noCloudctx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(path, tok.AccessToken) {
		t.Fatal("the token leaked into its own cache path")
	}
	//nolint:gosec // G302: widening the mode deliberately, to prove the read is
	// refused and that the refusal message carries no token.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, permErr := ReadTokenCache("contoso", "", TokenCacheMargin, noCloudctx)
	if permErr == nil {
		t.Fatal("expected a permissions error")
	}
	if strings.Contains(permErr.Error(), tok.AccessToken) {
		t.Fatal("the token leaked into an error message")
	}
	if strings.Contains(permErr.Error(), base64.RawURLEncoding.EncodeToString([]byte("secret"))) {
		t.Fatal("unexpected token-shaped content in the error")
	}
}

func TestExpiryParsesAzFormats(t *testing.T) {
	base := time.Date(2026, 9, 4, 12, 0, 0, 0, time.Local)
	for _, raw := range []string{
		"2026-09-04 12:00:00.000000",
		"2026-09-04 12:00:00",
	} {
		tok := Token{ExpiresOn: raw}
		if got := tok.Expiry(); !got.Equal(base) {
			t.Errorf("Expiry(%q) = %v, want %v", raw, got, base)
		}
	}
	if got := (Token{ExpiresOn: ""}).Expiry(); !got.IsZero() {
		t.Error("an empty expiry should be the zero time")
	}
	if (Token{ExpiresOn: ""}).UsableFor(time.Minute) {
		t.Error("a token with no expiry must never be considered usable")
	}
}

// TestCachedTokenIsRefusedForAnotherTenant is the check the cache's own
// documentation used to claim and not make.
//
// pimctl's cache lives outside cloudctx's per-context store, so nothing sweeps
// it when a context is deleted and recreated against a different tenant, or
// repointed at one. Serving that entry would act on another tenant with a
// token minted for them — the one failure this tool must not have.
func TestCachedTokenIsRefusedForAnotherTenant(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	contextStore := t.TempDir()
	writeAzProfile(t, filepath.Join(contextStore, "azure"), "tid-1", "someone@example.com")
	ForgetContextCache()
	t.Cleanup(ForgetContextCache)
	ForgetContextCache()
	t.Cleanup(ForgetContextCache)

	mints := 0
	tenant := "tid-1"
	jwt := fakeJWT(`{"oid":"oid-1","tid":"tid-1","upn":"someone@example.com"}`)
	expiry := time.Now().Add(time.Hour).Format("2006-01-02 15:04:05.000000")
	run := func(_ string, args ...string) ([]byte, []byte, error) {
		if len(args) > 0 && args[0] == "show" {
			return []byte("[contoso]\nazure_tenant = " + tenant + "\nstore: " + contextStore + "\n"), nil, nil
		}
		mints++
		return []byte(`{"accessToken":"` + jwt + `","expiresOn":"` + expiry + `","tenant":"tid-1"}`), nil, nil
	}

	if _, err := AcquireCached("contoso", run, false); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireCached("contoso", run, false); err != nil || mints != 1 {
		t.Fatalf("the second call should have hit the cache: mints=%d err=%v", mints, err)
	}

	// The context now points at a different tenant. The cached token was minted
	// for the old one and must not be served. Re-pointing a context happens
	// between pimctl runs, never inside one, so the memoised registry answer is
	// dropped here the way a new process would drop it.
	tenant = "tid-2"
	ForgetContextCache()
	got, err := AcquireCached("contoso", run, false)
	if err != nil {
		t.Fatal(err)
	}
	if mints != 2 {
		t.Errorf("a token minted for another tenant was served from the cache (mints=%d)", mints)
	}
	if got.FromCache {
		t.Error("the re-minted token should not be marked FromCache")
	}

	// An unpinned context still checks the selected Azure CLI account.
	tenant = ""
	ForgetContextCache()
	before := mints
	if _, err := AcquireCached("contoso", run, false); err != nil {
		t.Fatal(err)
	}
	if mints != before {
		t.Errorf("a context with no azure_tenant should still use its cache (mints=%d)", mints)
	}
}

// TestTokenCacheLivesInTheContextStore: a cached ARM token is a credential for
// one context, so it belongs inside that context's cloudctx store — where
// `cloudctx delete` sweeps it. An entry written before the move is migrated on
// the first read rather than left behind for nothing to clean up.
func TestTokenCacheLivesInTheContextStore(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	home := t.TempDir()
	prev := CloudctxVersion
	CloudctxVersion = func(Runner) (string, error) { return MinCloudctxVersion, nil }
	ForgetContextCache()
	t.Cleanup(func() { CloudctxVersion = prev; ForgetContextCache() })

	//nolint:unparam // a `cloudctx show` that works writes nothing to stderr.
	withStore := func(string, ...string) (stdout, stderr []byte, err error) {
		return []byte("[contoso]\nazure_tenant = tid-1\n\nstore:           " +
			filepath.Join(home, "contoso") + "\n"), nil, nil
	}

	// Written by a pimctl that could not see a store: the pre-contract path.
	tok := cacheToken(t, "contoso", time.Hour)
	WriteTokenCache(tok, noCloudctx)
	oldPath, err := TokenCachePath("contoso", noCloudctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(oldPath); statErr != nil {
		t.Fatalf("the pre-contract entry was not written: %v", statErr)
	}

	newPath, err := TokenCachePath("contoso", withStore)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "contoso", "pimctl", "token-contoso.json")
	if newPath != want {
		t.Errorf("path = %q, want %q", newPath, want)
	}
	if _, statErr := os.Stat(oldPath); statErr == nil {
		t.Error("the entry was copied rather than moved; two tokens for one context is one too many")
	}
	info, statErr := os.Stat(newPath)
	if statErr != nil {
		t.Fatalf("the entry did not arrive in the store: %v", statErr)
	}
	if perm := info.Mode().Perm(); perm != cacheFileMode {
		t.Errorf("the migrated token is %#o, want %#o", perm, cacheFileMode)
	}
	got, ok, err := ReadTokenCache("contoso", "tid-1", TokenCacheMargin, withStore)
	if err != nil || !ok || got.AccessToken != tok.AccessToken {
		t.Errorf("the migrated entry is not usable: ok=%v err=%v", ok, err)
	}

	// And it is swept from the store, not only from pimctl's own directory.
	n, err := ClearTokenCache(func(_ string, args ...string) ([]byte, []byte, error) {
		if strings.Join(args, " ") == "list --names" {
			return []byte("contoso\n"), nil, nil
		}
		return withStore("cloudctx", args...)
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("cache clear removed %d token(s), want 1", n)
	}
	if _, statErr := os.Stat(newPath); statErr == nil {
		t.Error("the token in the context store survived `pimctl cache clear`")
	}
}
