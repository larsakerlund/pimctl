// Account ownership for persisted data. This identifies an account without
// storing credentials; callers decide what each owned file contains.

package store

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Owner identifies the authenticated account to which a persisted file belongs.
// Context alone is insufficient because its selected tenant or user can change.
type Owner struct {
	Context     string `json:"context"`     // cloudctx name; empty for the shared Azure CLI login.
	TenantID    string `json:"tenantId"`    // tenant from the authenticated token.
	PrincipalID string `json:"principalId"` // user object id from that token.
}

// Valid reports whether tenant and principal are known. Unbound data must not
// be reused under an account whose identity cannot be established.
func (o Owner) Valid() bool { return o.TenantID != "" && o.PrincipalID != "" }

// Matches compares account ownership, preserving context-name case while
// treating Azure tenant and principal ids as case-insensitive.
func (o Owner) Matches(other Owner) bool {
	return o.Valid() && other.Valid() && o.Context == other.Context &&
		strings.EqualFold(o.TenantID, other.TenantID) && strings.EqualFold(o.PrincipalID, other.PrincipalID)
}

// FileName namespaces account-owned files by context, tenant, and principal.
// A digest avoids exposing object ids in names and disambiguates context names
// that sanitise to the same filename. Callers still check the stored Owner.
func (o Owner) FileName() string {
	sum := sha256.Sum256(
		[]byte(o.Context + "\x00" + strings.ToLower(o.TenantID) + "\x00" + strings.ToLower(o.PrincipalID)),
	)
	return FileName(o.Context) + "-" + hex.EncodeToString(sum[:])
}
