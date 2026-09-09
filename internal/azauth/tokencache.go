// The on-disk token cache: where one context's entry lives, what makes it
// usable, and the atomic 0600 write and permission check that guard it.
// Acquiring a token in the first place is token.go's job, and the shared cache
// directory and its naming rules belong to internal/store.

package azauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/larsakerlund/pimctl/internal/store"
)

// Acquiring a token costs a cloudctx/az process launch — 1.23 s median, 0.59 s
// of it az starting up — on every single command. Caching it on disk removes
// that from the common case.
//
// az already persists access and refresh tokens unencrypted in
// ~/.azure/msal_token_cache.json at 0600, so this is a second copy of a
// credential the machine already holds rather than a new class of exposure. It
// is held to the same properties: 0600 inside a 0700 directory, written
// atomically, and — the part az does not do — *refused* on read if the mode is
// any wider, because a token readable by another account is worse than no cache
// at all.
//
// The token is never logged, never printed, and never included in an error.

// TokenCacheMargin is how much validity a cached token must have left to be
// reused. A token that expires mid-run is worse than one re-minted up front.
const TokenCacheMargin = 5 * time.Minute

// tokenCacheVersion invalidates every entry when the on-disk shape changes.
const tokenCacheVersion = 1

// cacheFileMode is the mode a cached token must not be wider than; the write
// side sets it through [store.SecretFileMode], and the directory's mode is
// store.DirMode, shared with everything else pimctl writes.
const cacheFileMode fs.FileMode = 0o600

// cachedToken is one entry's on-disk shape. It is [Token] minus FromCache,
// which is a property of how the token was obtained rather than of the token,
// and is set on the way out by [ReadTokenCache]. Version is checked on read so
// a change to these fields retires every existing entry instead of
// misinterpreting one.
type cachedToken struct {
	Version int `json:"version"` // retires every entry when the shape changes.
	// Context is stored as well as being in the filename, so a file moved or
	// renamed by hand is rejected rather than served for the wrong tenant.
	Context     string `json:"context"`
	AccessToken string `json:"accessToken"` // the token itself, which is why this file is 0600.
	ExpiresOn   string `json:"expiresOn"`   // az's own local-time string, kept verbatim.
	Tenant      string `json:"tenant"`      // as az reported it.
	// PrincipalID and TenantID come from the token's own claims, so a cache hit
	// needs no second `az` spawn to learn who the token is for.
	PrincipalID       string `json:"principalId"`
	TenantID          string `json:"tenantId"`                    // as above.
	UserPrincipalName string `json:"userPrincipalName,omitempty"` // for the "using az login: …" line; absent in older entries.
}

// TokenCachePath is where the token for one context is stored: inside that
// context's own cloudctx store, at `$CLOUDCTX_STORE/pimctl/token-<name>.json`,
// so `cloudctx delete <name>` sweeps the credential with the rest of the
// context. A context whose store cannot be established — no cloudctx, one
// older than [MinCloudctxVersion], an unknown context — and the nameless shared
// `az login` keep the pre-contract location under pimctl's own cache
// directory, which is also what the file is migrated from the first time the
// store answers.
//
// The tenant is recorded inside the file rather than in its name: a read
// happens before any token exists, so the caller cannot know the tenant yet.
// ReadTokenCache enforces the tenant instead, which gives the same guarantee
// that switching tenants inside one context cannot serve the previous tenant's
// token, without the read and write paths having to agree on a name they cannot
// both compute.
func TokenCachePath(context string, run Runner) (string, error) {
	dir, err := store.Dir()
	if err != nil {
		return "", err
	}
	return ContextStatePath(context, "token-"+store.FileName(context)+".json", dir, run), nil
}

// ErrCachePermissions is returned when a cache file is readable by anyone but
// its owner.
var ErrCachePermissions = errors.New("token cache file has permissions wider than 0600")

// ReadTokenCache returns a cached token if one is present, intact, correctly
// permissioned, for the expected tenant, and still valid for at least margin.
// An empty wantTenant accepts whichever tenant the entry holds.
//
// ok says whether there is a token to use. Every ordinary way of not having one
// — no file, a corrupt or stale entry, the wrong context or tenant — is a miss
// with a nil error, because the answer to all of them is "mint a fresh one".
// The single error is a file whose mode has been widened, which the user needs
// told about rather than silently worked around.
//
// The three results are (tok, true, nil), (nil, false, nil) and
// (nil, false, err); a token is never returned alongside an error.
func ReadTokenCache(context, wantTenant string, margin time.Duration, run Runner) (tok *Token, ok bool, err error) {
	path, err := TokenCachePath(context, run)
	if err != nil {
		return nil, false, nil //nolint:nilerr // a path we cannot compute is a miss; see the doc comment.
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, false, nil //nolint:nilerr // no cache file is the ordinary case, not a failure.
	}
	if perm := info.Mode().Perm(); perm&^cacheFileMode != 0 {
		return nil, false, fmt.Errorf("%w: %s is %#o", ErrCachePermissions, path, perm)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // path is derived from the cache dir and a sanitised name
	if err != nil {
		return nil, false, nil //nolint:nilerr // an unreadable cache is a miss; the token is re-minted.
	}
	var c cachedToken
	if err = json.Unmarshal(raw, &c); err != nil {
		return nil, false, nil //nolint:nilerr // a corrupt cache is a miss; the token is re-minted.
	}
	if c.Version != tokenCacheVersion || c.Context != context || c.AccessToken == "" {
		return nil, false, nil // wrong shape or wrong context.
	}
	if wantTenant != "" && !strings.EqualFold(c.TenantID, wantTenant) {
		return nil, false, nil // the context now points at a different tenant.
	}
	cached := &Token{
		Context:           c.Context,
		AccessToken:       c.AccessToken,
		ExpiresOn:         c.ExpiresOn,
		Tenant:            c.Tenant,
		PrincipalID:       c.PrincipalID,
		TenantID:          c.TenantID,
		UserPrincipalName: c.UserPrincipalName,
		FromCache:         true,
	}
	if !cached.UsableFor(margin) {
		return nil, false, nil // expired, or too close to it to be worth using.
	}
	return cached, true, nil
}

// WriteTokenCache stores a token. Failures are deliberately silent: a cache
// that cannot be written must not break the command that just authenticated.
func WriteTokenCache(tok *Token, run Runner) {
	if tok == nil || tok.AccessToken == "" {
		return
	}
	path, err := TokenCachePath(tok.Context, run)
	if err != nil {
		return
	}
	if err = os.MkdirAll(filepath.Dir(path), store.DirMode); err != nil {
		return
	}
	//nolint:gosec // G117: serialising the access token is this file's purpose;
	// it is written 0600 in a 0700 directory and refused on read if wider.
	blob, err := json.Marshal(cachedToken{
		Version:           tokenCacheVersion,
		Context:           tok.Context,
		AccessToken:       tok.AccessToken,
		ExpiresOn:         tok.ExpiresOn,
		Tenant:            tok.Tenant,
		PrincipalID:       tok.PrincipalID,
		TenantID:          tok.TenantID,
		UserPrincipalName: tok.UserPrincipalName,
	})
	if err != nil {
		return
	}
	store.WriteSecret(path, blob)
}

// DropTokenCache removes the cached token for one context, used when ARM
// rejects it with a 401.
func DropTokenCache(context string, run Runner) {
	if path, err := TokenCachePath(context, run); err == nil {
		store.RemoveQuietly(path)
	}
}

// ClearTokenCache removes every cached token, in pimctl's own cache directory
// and in every cloudctx context's store, and reports how many files went.
//
// Both places have to be swept: a context registered with a cloudctx meeting
// [MinCloudctxVersion] keeps its token inside its store, and one that predates
// the move — or a machine that has since lost cloudctx — still has it under
// XDG. Enumerating the contexts costs one `cloudctx list --names`, which is
// only ever paid by `pimctl cache clear`.
//
// A context that cannot be enumerated or whose store cannot be read is skipped
// rather than failing the command: `cache clear` deletes what it can find, and
// what it cannot find is re-minted anyway.
func ClearTokenCache(run Runner) (int, error) {
	removed, err := store.RemoveFiles("token-")
	if err != nil {
		return removed, err
	}
	for _, dir := range ContextStateDirs(run) {
		n, dirErr := store.RemoveFilesIn(dir, "token-")
		if dirErr != nil {
			continue
		}
		removed += n
	}
	return removed, nil
}
