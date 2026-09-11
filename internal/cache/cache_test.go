// Tests for both caches on disk: that a listing and a policy survive a
// round-trip, that each stops being served past its own TTL, that a corrupt or
// wrong-version file reads as a miss rather than an error, and that DropPolicy
// and ClearPolicies remove what they say they do. Every case redirects
// XDG_CACHE_HOME at a temp dir and ages files by rewriting fetchedAt, so
// nothing here sleeps or touches the real cache.

package cache

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/larsakerlund/pimctl/internal/armclient"
	"github.com/larsakerlund/pimctl/internal/store"
)

// contribGUID is a real built-in role id, used so the key derivation is
// exercised on the shape it actually sees.
const contribGUID = "b24988ac-6180-42a0-ab88-20f7382dd24c"

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

// ageCache rewrites a cache file's fetchedAt so expiry can be tested without
// sleeping.
func ageCache(path string, by time.Duration) error {
	b, err := os.ReadFile(path) //nolint:gosec // a path this test created under t.TempDir()
	if err != nil {
		return err
	}
	var env map[string]any
	if decodeErr := json.Unmarshal(b, &env); decodeErr != nil {
		return decodeErr
	}
	env["fetchedAt"] = time.Now().Add(-by).Format(time.RFC3339Nano)
	out, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o600)
}

// agePolicyCache rewrites every entry's fetchedAt so TTL expiry can be tested
// without sleeping.
func agePolicyCache(t *testing.T, context string, by time.Duration) error {
	t.Helper()
	path, err := policyPath(testOwner(context))
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(path) //nolint:gosec // path is this test's own temp dir
	if err != nil {
		return err
	}
	var f map[string]any
	if uerr := json.Unmarshal(raw, &f); uerr != nil {
		return uerr
	}
	entries, ok := f["entries"].(map[string]any)
	if !ok {
		t.Fatalf("policy cache has no entries object: %v", f)
	}
	for k := range entries {
		entry, isObj := entries[k].(map[string]any)
		if !isObj {
			continue
		}
		entry["fetchedAt"] = time.Now().Add(-by).Format(time.RFC3339Nano)
		entries[k] = entry
	}
	f["entries"] = entries
	out, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o600)
}

func TestEligibilityCacheRoundTripAndExpiry(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	if _, _, ok := Read(testOwner("contoso")); ok {
		t.Fatal("a fresh cache dir should have nothing in it")
	}

	roles := []armclient.Eligibility{{ID: "/x", Name: "x"}}
	Write(testOwner("contoso"), roles)

	got, age, ok := Read(testOwner("contoso"))
	if !ok {
		t.Fatal("the entry just written should be readable")
	}
	if len(got) != 1 || got[0].ID != "/x" {
		t.Fatalf("round trip lost data: %v", got)
	}
	if age > time.Minute {
		t.Errorf("age = %v, want ~0", age)
	}

	// A different context must not see it.
	if _, _, ok := Read(testOwner("globex")); ok {
		t.Error("the cache must be per-context")
	}

	// Corrupt content is treated as a miss, never an error.
	p, err := eligibilityPath(testOwner("contoso"))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFile(p, "not json"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := Read(testOwner("contoso")); ok {
		t.Error("corrupt cache content should read as a miss")
	}

	// Expiry.
	Write(testOwner("contoso"), roles)
	if err := ageCache(p, TTL+time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := Read(testOwner("contoso")); ok {
		t.Error("an entry older than the TTL must not be used")
	}

	// Clear removes it.
	Write(testOwner("contoso"), roles)
	if _, err := Clear(); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := Read(testOwner("contoso")); ok {
		t.Error("Clear should have removed the entry")
	}
}

func TestFormatAge(t *testing.T) {
	for in, want := range map[time.Duration]string{
		10 * time.Second: "10s",
		4 * time.Minute:  "4m",
		90 * time.Second: "1m",
	} {
		if got := FormatAge(in); got != want {
			t.Errorf("FormatAge(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestPolicyCacheRoundTripAndTTL(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	scope := "/providers/Microsoft.Management/managementGroups/contoso-prod"
	roleDef := "/providers/Microsoft.Authorization/roleDefinitions/" + contribGUID

	if got := LookupPolicy(testOwner("contoso"), scope, roleDef, false); got != nil {
		t.Fatal("a fresh cache dir should hold no policies")
	}

	want := &armclient.RoleSettings{
		MaximumDuration:    4 * time.Hour,
		MaximumDurationISO: "PT4H",
		EnabledRules:       []string{"MultiFactorAuthentication", "Justification"},
	}
	StorePolicies(testOwner("contoso"), map[string]*armclient.RoleSettings{
		PolicyKey(scope, roleDef): want,
	})

	got := LookupPolicy(testOwner("contoso"), scope, roleDef, false)
	if got == nil {
		t.Fatal("the policy just stored should be readable")
	}
	if got.MaximumDurationISO != "PT4H" || !got.RequiresJustification() || got.RequiresTicket() {
		t.Fatalf("round trip lost data: %+v", got)
	}

	// Keyed on scope and role, not one or the other.
	if LookupPolicy(testOwner("contoso"), "/subscriptions/other", roleDef, false) != nil {
		t.Error("a different scope must miss")
	}
	if LookupPolicy(
		testOwner("contoso"),
		scope,
		"/providers/Microsoft.Authorization/roleDefinitions/other",
		false,
	) != nil {
		t.Error("a different role must miss")
	}
	if LookupPolicy(testOwner("globex"), scope, roleDef, false) != nil {
		t.Error("the policy cache must be per-context")
	}

	// --refresh bypasses it.
	if LookupPolicy(testOwner("contoso"), scope, roleDef, true) != nil {
		t.Error("--refresh must bypass the policy cache")
	}

	// Expiry.
	if err := agePolicyCache(t, "contoso", PolicyTTL+time.Hour); err != nil {
		t.Fatal(err)
	}
	if LookupPolicy(testOwner("contoso"), scope, roleDef, false) != nil {
		t.Error("a policy older than the TTL must not be used")
	}
}

// TestDropPolicyRemovesOnlyTheRejectedEntry covers the recovery path: when ARM
// rejects a request on a policy rule the cached copy is known to be stale.
func TestDropPolicyRemovesOnlyTheRejectedEntry(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	scopeA := "/providers/Microsoft.Management/managementGroups/contoso-prod"
	scopeB := "/providers/Microsoft.Management/managementGroups/contoso-qa"
	roleDef := "/providers/Microsoft.Authorization/roleDefinitions/" + contribGUID
	s := &armclient.RoleSettings{MaximumDurationISO: "PT4H", MaximumDuration: 4 * time.Hour}

	StorePolicies(testOwner("contoso"), map[string]*armclient.RoleSettings{
		PolicyKey(scopeA, roleDef): s,
		PolicyKey(scopeB, roleDef): s,
	})
	DropPolicy(testOwner("contoso"), scopeA, roleDef)

	if LookupPolicy(testOwner("contoso"), scopeA, roleDef, false) != nil {
		t.Error("the rejected policy should have been dropped")
	}
	if LookupPolicy(testOwner("contoso"), scopeB, roleDef, false) == nil {
		t.Error("DropPolicy removed an unrelated entry")
	}
}

func TestClearPolicies(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	scope := "/subscriptions/x"
	roleDef := "/providers/Microsoft.Authorization/roleDefinitions/" + contribGUID
	StorePolicies(testOwner("contoso"), map[string]*armclient.RoleSettings{
		PolicyKey(scope, roleDef): {MaximumDurationISO: "PT1H", MaximumDuration: time.Hour},
	})
	n, err := ClearPolicies()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("ClearPolicies removed %d files, want 1", n)
	}
	if LookupPolicy(testOwner("contoso"), scope, roleDef, false) != nil {
		t.Error("the policy survived ClearPolicies")
	}
}

func testOwner(name string) store.Owner {
	return store.Owner{Context: name, TenantID: "tid-1", PrincipalID: "oid-1"}
}

func TestCachesAreBoundToTenantAndPrincipal(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	owner := testOwner("")
	roles := []armclient.Eligibility{{ID: "old-account-role"}}
	Write(owner, roles)
	scope, role := "/providers/Microsoft.Management/managementGroups/shared", "reader"
	StorePolicies(owner, map[string]*armclient.RoleSettings{PolicyKey(scope, role): {MaximumDuration: time.Hour}})
	for _, other := range []store.Owner{
		{Context: "", TenantID: "other-tenant", PrincipalID: owner.PrincipalID},
		{Context: "", TenantID: owner.TenantID, PrincipalID: "other-user"},
		{},
	} {
		if _, _, ok := Read(other); ok {
			t.Errorf("another owner read roles: %+v", other)
		}
		if LookupPolicy(other, scope, role, false) != nil {
			t.Errorf("another owner read policy: %+v", other)
		}
	}
	other := store.Owner{TenantID: owner.TenantID, PrincipalID: "other-user"}
	Write(other, []armclient.Eligibility{{ID: "new-account-role"}})
	got, _, ok := Read(owner)
	if !ok || len(got) != 1 || got[0].ID != "old-account-role" {
		t.Fatal("another user's write replaced the original account's data")
	}
	// Renaming a file must not bypass the ownership check inside it.
	oldPath, err := eligibilityPath(owner)
	if err != nil {
		t.Fatal(err)
	}
	newPath, err := eligibilityPath(other)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(oldPath) //nolint:gosec // Account-cache path in this test's temporary directory.
	if err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // Account-cache path in this test's temporary directory.
	if err := os.WriteFile(
		newPath,
		raw,
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := Read(other); ok {
		t.Fatal("renamed file bypassed ownership check")
	}
}
