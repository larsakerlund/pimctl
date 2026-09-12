// Covers account checks before token reuse, including user switches within
// one tenant, missing identity information, and cloudctx's isolated profile.

package azauth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeAzProfile(t *testing.T, dir, tenant, user string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"subscriptions": []any{map[string]any{
		"tenantId": tenant, "isDefault": true, "user": map[string]string{"name": user, "type": "user"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, azProfileName), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCachedTokenRequiresSelectedAccount(t *testing.T) {
	for _, name := range []string{"", "contoso"} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CACHE_HOME", t.TempDir())
			ForgetContextCache()
			t.Cleanup(ForgetContextCache)
			contextStore := t.TempDir()
			dir := filepath.Join(home, ".azure")
			if name != "" {
				dir = filepath.Join(contextStore, "azure")
			}
			run := func(_ string, _ ...string) ([]byte, []byte, error) { //nolint:unparam // Runner includes stderr even for a successful fake.
				return []byte("[contoso]\nazure_tenant = tid-1\nstore: " + contextStore + "\n"), nil, nil
			}
			tok := cacheToken(t, name, time.Hour)
			WriteTokenCache(tok, run)
			for _, tc := range []struct {
				name, tenant, user string
				wantHit            bool
			}{
				{"same account", "tid-1", "ada@example.com", true},
				{"case insensitive", "TID-1", "ADA@example.com", true},
				{"different user", "tid-1", "new-user@example.com", false},
				{"different tenant", "tid-2", "ada@example.com", false},
				{"unknown user", "tid-1", "", false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					writeAzProfile(t, dir, tc.tenant, tc.user)
					_, hit, err := cachedTokenFor(name, run)
					if err != nil || hit != tc.wantHit {
						t.Fatalf("cache hit=%v, want %v; err=%v", hit, tc.wantHit, err)
					}
				})
			}
			if err := os.Remove(filepath.Join(dir, azProfileName)); err != nil {
				t.Fatal(err)
			}
			if _, hit, err := cachedTokenFor(name, run); err != nil || hit {
				t.Fatalf("missing profile: hit=%v err=%v", hit, err)
			}
		})
	}
}

// TestExpectedAccountInsideAContextWindowSpawnsNothing: inside a `cloudctx use`
// window the store is in the environment, and a 0.65 s `cloudctx show` to
// learn what the shell already holds is the whole cost of a warm command. The
// profile is read from $CLOUDCTX_STORE/azure and the token-vs-profile check
// still stands.
func TestExpectedAccountInsideAContextWindowSpawnsNothing(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	contextStore := t.TempDir()
	t.Setenv("CLOUDCTX_CONTEXT", "contoso")
	t.Setenv("CLOUDCTX_STORE", contextStore)
	ForgetContextCache()
	t.Cleanup(ForgetContextCache)
	writeAzProfile(t, filepath.Join(contextStore, "azure"), "tid-1", "ada@example.com")

	noSpawn := func(name string, args ...string) (stdout, stderr []byte, err error) {
		t.Errorf("inside the context window nothing should be spawned, got %s %v", name, args)
		return nil, nil, nil
	}
	account, ok := expectedAccount("contoso", noSpawn)
	if !ok || account.Tenant != "tid-1" || account.User != "ada@example.com" {
		t.Fatalf("expectedAccount = %+v, %v; want the profile in $CLOUDCTX_STORE/azure", account, ok)
	}

	// The whole warm path: the cached token is found, checked against the
	// profile and served, with no process launched anywhere along it.
	WriteTokenCache(cacheToken(t, "contoso", time.Hour), noSpawn)
	if _, hit, err := cachedTokenFor("contoso", noSpawn); err != nil || !hit {
		t.Fatalf("cache hit=%v err=%v; want a hit with no spawn", hit, err)
	}
	// And the profile check still bites: another user in the same store is a
	// miss, not an invitation to use the previous account.
	writeAzProfile(t, filepath.Join(contextStore, "azure"), "tid-1", "someone-else@example.com")
	if _, hit, err := cachedTokenFor("contoso", noSpawn); err != nil || hit {
		t.Fatalf("a different user in the profile must be a miss: hit=%v err=%v", hit, err)
	}
}

// TestExpectedAccountOutsideAContextWindowAsksCloudctx: the environment answers
// only for the context it names. Asked about another one, pimctl still runs
// `cloudctx show`, and the pinned tenant it reports is still enforced.
func TestExpectedAccountOutsideAContextWindowAsksCloudctx(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("CLOUDCTX_CONTEXT", "globex")
	t.Setenv("CLOUDCTX_STORE", t.TempDir())
	ForgetContextCache()
	t.Cleanup(ForgetContextCache)
	contextStore := t.TempDir()
	writeAzProfile(t, filepath.Join(contextStore, "azure"), "tid-1", "ada@example.com")

	pinned := "tid-1"
	asked := 0
	run := func(_ string, args ...string) ([]byte, []byte, error) {
		if len(args) > 0 && args[0] == "show" {
			asked++
		}
		return []byte("[contoso]\nazure_tenant = " + pinned + "\nstore: " + contextStore + "\n"), nil, nil
	}
	account, ok := expectedAccount("contoso", run)
	if !ok || account.Tenant != "tid-1" || asked != 1 {
		t.Fatalf("expectedAccount = %+v, %v after %d show(s); want the store from `cloudctx show`", account, ok, asked)
	}

	// A context pinned to a tenant other than the one the profile selects is
	// unknown territory, and unknown disables reuse.
	pinned = "tid-2"
	ForgetContextCache()
	if account, ok := expectedAccount("contoso", run); ok {
		t.Errorf("a pinned tenant the profile disagrees with was accepted: %+v", account)
	}
}

func TestReadAzAccountRejectsUnidentifiableProfiles(t *testing.T) {
	for _, raw := range []string{
		`not json`, `{}`, `{"subscriptions":[]}`,
		`{"subscriptions":[{"tenantId":"tid-1","isDefault":true}]}`,
		`{"subscriptions":[{"tenantId":"tid-1","isDefault":true,"user":{"name":"app-id","type":"servicePrincipal"}}]}`,
	} {
		path := filepath.Join(t.TempDir(), azProfileName)
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, ok := readAzAccount(path); ok {
			t.Fatalf("accepted unidentifiable profile %s", raw)
		}
	}
}
