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
