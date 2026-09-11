// Read the selected Azure CLI account from its profile before reusing a token.
// Missing or unfamiliar profiles disable cache reuse; authentication remains
// the Azure CLI's responsibility.

package azauth

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// utf8BOM is the byte-order mark az prefixes its profile with.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// azProfileName is the subscription profile inside the Azure CLI config directory.
const azProfileName = "azureProfile.json"

// azAccount identifies the selected user and tenant without reading credentials.
type azAccount struct {
	Tenant string // tenant id of the default subscription.
	User   string // Azure CLI login name, compared with the token's user claim.
}

// azProfile contains the selected subscription and its account identity.
type azProfile struct {
	Subscriptions []struct {
		TenantID  string `json:"tenantId"`  // tenant owning this subscription.
		IsDefault bool   `json:"isDefault"` // whether az selects this subscription.
		User      struct {
			Name string `json:"name"` // login name; an empty value cannot identify an account.
			Type string `json:"type"` // only user logins can be matched to a token's user claim.
		} `json:"user"` // account associated with the subscription.
	} `json:"subscriptions"` // subscriptions known to az.
}

// expectedAccount reads the account used by a token invocation. Named contexts
// use the companion contract's $CLOUDCTX_STORE/azure directory and must agree
// with any pinned tenant. Unknown identities disable cache reuse.
func expectedAccount(name string, run Runner) (azAccount, bool) {
	var dir, pinnedTenant string
	if name == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return azAccount{}, false
		}
		// A shared az child has AZURE_CONFIG_DIR stripped by childEnv.
		dir = filepath.Join(home, ".azure")
	} else {
		info, err := ShowContext(name, run)
		if err != nil || info.Store == "" {
			return azAccount{}, false
		}
		dir, pinnedTenant = filepath.Join(info.Store, "azure"), info.Tenant
	}
	account, ok := readAzAccount(filepath.Join(dir, azProfileName))
	if !ok || pinnedTenant != "" && !strings.EqualFold(account.Tenant, pinnedTenant) {
		return azAccount{}, false
	}
	return account, true
}

// readAzAccount reads a profile, accepting only an identifiable default user.
// It returns false for absent, corrupt, or unfamiliar profiles and never logs
// their contents. Service-principal identities are not inferred from user claims.
func readAzAccount(path string) (azAccount, bool) {
	raw, err := os.ReadFile(path) //nolint:gosec // path is the selected Azure CLI directory plus its profile filename.
	if err != nil {
		return azAccount{}, false
	}
	var profile azProfile
	if err = json.Unmarshal(bytes.TrimPrefix(raw, utf8BOM), &profile); err != nil {
		return azAccount{}, false
	}
	for _, sub := range profile.Subscriptions {
		if sub.IsDefault && sub.TenantID != "" && sub.User.Name != "" && strings.EqualFold(sub.User.Type, "user") {
			return azAccount{Tenant: sub.TenantID, User: sub.User.Name}, true
		}
	}
	return azAccount{}, false
}
