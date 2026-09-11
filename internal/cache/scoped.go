// Scoped eligibility caches retain ARM's ancestry and membership evidence for
// a target. They contain no activation state and share eligibility cache TTL
// and clearing; ordinary eligibility listings remain separate.

package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/larsakerlund/pimctl/internal/armclient"
	"github.com/larsakerlund/pimctl/internal/store"
)

// scopedEnvelope binds ancestry evidence to its exact target and account.
type scopedEnvelope struct {
	Scope   string   `json:"scope"`   // Full target ID, checked on read.
	Listing envelope `json:"listing"` // Account ownership, timestamp and original schedules.
}

// scopedPath derives a filename cleared by the ordinary eligibility cache clear.
func scopedPath(owner store.Owner, scope string) (string, error) {
	p, err := eligibilityPath(owner)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(strings.ToLower(scope)))
	return strings.TrimSuffix(p, ".json") + "-scope-" + hex.EncodeToString(sum[:]) + ".json", nil
}

// ReadScoped returns usable account-owned evidence for scope, or a cache miss.
func ReadScoped(owner store.Owner, scope string) ([]armclient.Eligibility, bool) {
	p, err := scopedPath(owner, scope)
	if err != nil {
		return nil, false
	}
	b, err := os.ReadFile(p) //nolint:gosec // the path is derived from account and scope digests.
	if err != nil {
		return nil, false
	}
	var env scopedEnvelope
	if json.Unmarshal(b, &env) != nil {
		return nil, false
	}
	age := time.Since(env.Listing.FetchedAt)
	if env.Listing.Version != version || !env.Listing.Owner.Matches(owner) ||
		!strings.EqualFold(env.Scope, scope) || age < 0 || age > TTL {
		return nil, false
	}
	return env.Listing.Roles, true
}

// WriteScoped caches complete scoped reads; failures never break activation.
func WriteScoped(owner store.Owner, scope string, roles []armclient.Eligibility) {
	if !owner.Valid() {
		return
	}
	p, err := scopedPath(owner, scope)
	if err != nil {
		return
	}
	b, err := json.Marshal(scopedEnvelope{Scope: scope, Listing: envelope{
		Version: version, Owner: owner, FetchedAt: time.Now(), Roles: roles,
	}})
	if err != nil {
		return
	}
	if err = os.MkdirAll(filepath.Dir(p), store.DirMode); err != nil {
		return
	}
	store.WriteAtomic(p, b)
}
