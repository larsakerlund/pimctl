// Reading the Azure CLI's own profile to learn which tenant the shared
// `az login` currently points at. This is the cloudctx-free half of the tenant
// check: a file read, no process spawn, and never an error — a profile that
// cannot be read means the check does not apply, not that the command fails.

package azauth

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
)

// utf8BOM is the byte-order mark az prefixes its profile with.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// azProfileName is the file az keeps its subscription list in, inside the Azure
// CLI's config directory.
const azProfileName = "azureProfile.json"

// azProfile is the part of az's profile pimctl reads: which subscriptions the
// current login can see, and which of them is the default.
type azProfile struct {
	// Subscriptions is empty for a login that has not been given any, which is
	// a legitimate state and simply means there is no tenant to compare.
	Subscriptions []struct {
		TenantID  string `json:"tenantId"`  // the tenant this subscription belongs to.
		IsDefault bool   `json:"isDefault"` // exactly one is true for a login with subscriptions.
	} `json:"subscriptions"`
}

// defaultAzTenant returns the tenant id of the shared `az login`'s default
// subscription, and whether it could be determined at all.
//
// It is what makes the shared-login path's cached token as safe as a context's:
// a user who runs `az login` for another tenant changes which tenant "no
// context" means, and a token cached under the shared-login slot would
// otherwise be served for the wrong one. [ReadTokenCache] refuses an entry
// whose tenant no longer matches.
//
// The read is deliberately best-effort. az's profile is az's format, not a
// contract with pimctl: a missing, unreadable or unrecognisable file yields
// false, the check is skipped, and the worst case is the behaviour pimctl had
// before this existed. It never returns an error, because there is no failure
// here a user could act on.
//
// ~/.azure is read directly rather than through $AZURE_CONFIG_DIR: a bare az is
// spawned with that variable removed (see [childEnv]), so the store it reads is
// the one under the home directory whatever the surrounding shell says.
func defaultAzTenant() (tenantID string, ok bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	//nolint:gosec // G304: the path is the home directory plus two constants.
	raw, err := os.ReadFile(filepath.Join(home, ".azure", azProfileName))
	if err != nil {
		return "", false
	}
	// az writes the profile as UTF-8 with a byte-order mark, which
	// encoding/json will not parse.
	raw = bytes.TrimPrefix(raw, utf8BOM)

	var profile azProfile
	if err = json.Unmarshal(raw, &profile); err != nil {
		return "", false
	}
	for _, sub := range profile.Subscriptions {
		if sub.IsDefault && sub.TenantID != "" {
			return sub.TenantID, true
		}
	}
	return "", false
}
