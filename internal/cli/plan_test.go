// Covers plan.go: the policy read that opens every `up`, and what happens when
// the cached token it is made with has stopped being accepted. The duration
// clamp it feeds is covered in duration_test.go, and the requests a plan turns
// into are request_test.go's.

package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/larsakerlund/pimctl/internal/armclient"
	"github.com/larsakerlund/pimctl/internal/azauth"
)

// servePolicyRead answers the two GETs behind one policy read — the assignment
// listing pointing at a stub policy, and that policy with an eight-hour
// maximum and the Justification rule — and fails the test on anything else.
func servePolicyRead(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	switch {
	case strings.Contains(r.URL.Path, "/roleManagementPolicyAssignments"):
		scope := strings.TrimSuffix(
			r.URL.Path, "/providers/Microsoft.Authorization/roleManagementPolicyAssignments",
		)
		writeJSON(t, w, map[string]any{"value": []any{map[string]any{"properties": map[string]any{
			"policyId": scope + "/providers/Microsoft.Authorization/roleManagementPolicies/pol1",
		}}}})
	case strings.Contains(r.URL.Path, "/roleManagementPolicies/"):
		writeJSON(t, w, map[string]any{"id": r.URL.Path, "properties": map[string]any{"rules": []any{
			map[string]any{"id": "Expiration_EndUser_Assignment", "maximumDuration": "PT8H"},
			map[string]any{"id": "Enablement_EndUser_Assignment", "enabledRules": []string{"Justification"}},
		}}})
	default:
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

// cacheStaleToken caches a token for contoso that the test server will reject,
// and returns it as the cache reads it back — marked as a cache hit, which is
// the condition the one-time refresh is gated on.
func cacheStaleToken(t *testing.T) *azauth.Token {
	t.Helper()
	azauth.WriteTokenCache(&azauth.Token{
		Context:     "contoso",
		AccessToken: "stale-token",
		ExpiresOn:   time.Now().Add(time.Hour).Format("2006-01-02 15:04:05.000000"),
		Tenant:      "tid-1",
		TenantID:    "tid-1",
		PrincipalID: "oid-1",
	}, azauth.DefaultRunner)
	tok := readTokenCache(t, "contoso")
	if tok == nil || !tok.FromCache {
		t.Fatal("the stale token should have been readable from the cache")
	}
	return tok
}

// TestPlanReplacesARejectedCachedToken: the policy read is the first ARM call
// an `up` makes, so a cached token ARM has stopped accepting is rejected here
// before anywhere else. It gets the same treatment as a 401 on the request or
// its polls: the token is dropped and re-minted once, the read is repeated
// with the new one, and the plan comes out whole.
func TestPlanReplacesARejectedCachedToken(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	installFakeRunner(t, []string{"contoso"})
	tok := cacheStaleToken(t)

	// The re-mint goes through the fake registry; only the mint itself is
	// counted, since the registry reads behind it are not spawns of az.
	var mu sync.Mutex
	var seen []string
	minted := 0
	registry := azauth.DefaultRunner
	azauth.DefaultRunner = func(name string, args ...string) ([]byte, []byte, error) {
		if strings.Contains(strings.Join(args, " "), "get-access-token") {
			mu.Lock()
			minted++
			mu.Unlock()
		}
		return registry(name, args...)
	}
	t.Cleanup(func() { azauth.DefaultRunner = registry })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		seen = append(seen, auth)
		mu.Unlock()
		if auth == "stale-token" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":{"code":"ExpiredAuthenticationToken","message":"rejected"}}`)
			return
		}
		servePolicyRead(t, w, r)
	}))
	t.Cleanup(srv.Close)

	s := &session{Context: "contoso", Token: tok, Client: armclient.New(srv.URL, tok.AccessToken, srv.Client())}
	rows := []row{{Context: "contoso", Elig: twoLowImpactRoles()[0]}}
	items := buildPlan(context.Background(), rows, []*session{s}, 0, "", false)
	if len(items) != 1 {
		t.Fatalf("planned %d items for one row", len(items))
	}
	if items[0].PrepErr != nil {
		t.Fatalf("a 401 on a cached token should self-heal, got: %v", items[0].PrepErr)
	}
	if items[0].Settings == nil || items[0].Duration != 8*time.Hour {
		t.Fatalf("the policy was not read after the retry: %+v", items[0])
	}

	mu.Lock()
	defer mu.Unlock()
	if minted != 1 {
		t.Errorf("expected exactly one re-mint, got %d", minted)
	}
	// One rejected read, then the two policy GETs with the replacement.
	if len(seen) != 3 || seen[0] != "stale-token" || seen[1] == "stale-token" || seen[2] == "stale-token" {
		t.Fatalf("expected the stale token once and a fresh one for the rest, saw %q", seen)
	}
	if c := readTokenCache(t, "contoso"); c != nil && c.AccessToken == "stale-token" {
		t.Error("the rejected token is still cached")
	}
}
