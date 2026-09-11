// Scoped-cache tests pin target/account isolation and expiry. These files are
// eligibility evidence only, and ordinary cache clearing must remove them.

package cache

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/larsakerlund/pimctl/internal/armclient"
)

func TestScopedCacheOwnershipExpiryAndClearing(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	owner := testOwner("work")
	scope := "/subscriptions/sub/resourceGroups/dev"
	roles := []armclient.Eligibility{{Name: "source"}}
	WriteScoped(owner, scope, roles)
	if got, ok := ReadScoped(owner, scope); !ok || len(got) != 1 {
		t.Fatalf("round trip: %#v, %v", got, ok)
	}
	if _, ok := ReadScoped(testOwner("other"), scope); ok {
		t.Fatal("cross-context cache hit")
	}
	if _, ok := ReadScoped(owner, scope+"other"); ok {
		t.Fatal("cross-scope cache hit")
	}
	other := owner
	other.PrincipalID = "other-user"
	if _, ok := ReadScoped(other, scope); ok {
		t.Fatal("cross-account cache hit")
	}
	path, err := scopedPath(owner, scope)
	if err != nil {
		t.Fatal(err)
	}
	expired := scopedEnvelope{
		Scope:   scope,
		Listing: envelope{Version: version, Owner: owner, FetchedAt: time.Now().Add(-2 * TTL), Roles: roles},
	}
	raw, err := json.Marshal(expired)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadScoped(owner, scope); ok {
		t.Fatal("used expired evidence")
	}
	WriteScoped(owner, scope, roles)
	if _, err := Clear(); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadScoped(owner, scope); ok {
		t.Fatal("cache clear left scoped evidence")
	}
}
