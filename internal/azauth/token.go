// Minting one ARM token by running az under cloudctx, and reading the identity
// claims back out of it. Storing that token between runs is tokencache.go's
// job; nothing here touches the disk.

package azauth

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// ARMResource is the v1.0 resource id for Azure Resource Manager.
const ARMResource = "https://management.azure.com/"

// Token is an ARM access token plus the identity claims decoded from it.
type Token struct {
	// Context is the cloudctx context the token was minted in. Empty means the
	// ambient `az` login was used.
	Context string
	// AccessToken is the raw bearer token. Never print this.
	AccessToken string
	// ExpiresOn is az's local-time expiry string, passed through verbatim.
	ExpiresOn string
	// Tenant is the tenant reported by az account get-access-token.
	Tenant string
	// PrincipalID is the `oid` claim: the signed-in user's object id. This is
	// the principalId every SelfActivate request must carry, even when the
	// eligibility itself is held by a group.
	PrincipalID string
	// TenantID is the `tid` claim.
	TenantID string
	// UserPrincipalName is the `upn` claim (falling back to `unique_name` or
	// `preferred_username`). It identifies who the token belongs to without
	// costing a second `az` invocation, which is the only other way to learn it.
	UserPrincipalName string
	// FromCache records whether this token was served from disk rather than
	// minted. Reported by --debug; never with the token itself.
	FromCache bool
}

// Expiry parses az's local-time expiry string. az emits
// "2026-09-04 12:00:00.000000" in local time; newer versions may emit RFC3339.
// A value that cannot be parsed yields the zero time, which every caller treats
// as "already expired".
func (t Token) Expiry() time.Time {
	raw := strings.TrimSpace(t.ExpiresOn)
	if raw == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999",
		"2006-01-02 15:04:05",
		time.RFC3339,
	} {
		if ts, err := time.ParseInLocation(layout, raw, time.Local); err == nil {
			return ts
		}
	}
	return time.Time{}
}

// UsableFor reports whether the token still has at least margin of validity
// left. A token that expires mid-run is worse than one re-minted up front.
func (t Token) UsableFor(margin time.Duration) bool {
	exp := t.Expiry()
	return !exp.IsZero() && time.Until(exp) >= margin
}

// Label names the token's origin for user-facing output.
func (t Token) Label() string {
	if t.Context == "" {
		return "(default)"
	}
	return t.Context
}

// ExecError carries the verbatim stderr of a failed cloudctx/az invocation so
// callers can show it to the user without guessing at the cause.
type ExecError struct {
	Context string // the cloudctx context, empty for a bare `az` invocation.
	Cmd     string // the command line as run, for the message; it carries no secret.
	Stderr  string // az's own stderr, verbatim, which is where the real cause is.
	Err     error  // the exec failure underneath, for errors.Is and errors.As.
}

// Error renders the failure the way the user needs to see it: az's own stderr
// verbatim, then a hint naming what to do about it. The token is not involved —
// a failed invocation never produced one.
func (e *ExecError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = e.Err.Error()
	}
	if isMissingBinary(e.Err) {
		// "exec: \"cloudctx\": executable file not found in $PATH" is the
		// operating system talking. What the user needs is the sentence below.
		return e.hint(msg)
	}
	return fmt.Sprintf("%s failed: %s\nhint: %s", e.Cmd, msg, e.hint(msg))
}

// isMissingBinary reports whether an error is the operating system saying the
// command does not exist, as opposed to the command running and failing.
func isMissingBinary(err error) bool {
	return errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist)
}

// hint is the sentence after the failure: what the user should do next.
//
// A context that does not exist cannot be logged in to, and telling someone to
// run `cloudctx login typo` sends them at the wrong problem — the name is
// wrong, not the session. cloudctx says "unknown context" itself, so that is
// what this reads; if it ever stops saying it, the hint falls back to the login
// advice, which is the right answer for every other failure.
func (e *ExecError) hint(msg string) string {
	switch {
	case isMissingBinary(e.Err) && strings.HasPrefix(e.Cmd, cloudctxBin):
		return fmt.Sprintf(
			"cloudctx is not installed, so there is no %q context.\n"+
				"pimctl works without it against your `az login`; -c and --all-contexts are cloudctx's own features",
			e.Context)
	case isMissingBinary(e.Err):
		return "the Azure CLI is not installed, and pimctl needs it to authenticate.\n" +
			"See https://learn.microsoft.com/cli/azure/install-azure-cli"
	case e.Context != "" && strings.Contains(strings.ToLower(msg), "unknown context"):
		return fmt.Sprintf(
			"there is no context called %q — `cloudctx list` shows the names, `cloudctx new %s` creates it",
			e.Context, e.Context)
	case e.Context != "":
		return fmt.Sprintf("the context may not be logged in — run `cloudctx login %s`", e.Context)
	default:
		return "the shared az login may have expired — run `az login`"
	}
}

// Unwrap exposes the underlying [os/exec] failure to [errors.Is] and
// [errors.As], so a caller can still distinguish "cloudctx is not installed"
// from "the command ran and exited non-zero".
func (e *ExecError) Unwrap() error { return e.Err }

// Runner executes a command and returns stdout, stderr and an error. It exists
// so tests can substitute a fake az/cloudctx.
type Runner func(name string, args ...string) (stdout, stderr []byte, err error)

// cloudctxVars are the environment variables a cloudctx context exports, and
// which therefore have to be removed from a child that is meant to run outside
// any context. The list mirrors cloudctx's own CLEARABLE_VARS — the seven
// managed variables of its companion contract — which is what `cloudctx clear`
// unsets and what `cloudctx exec` strips from the caller before overlaying.
//
// AZURE_CONFIG_DIR is the one that matters to az; the rest are cleared for the
// same reason cloudctx clears them, so that "no context" means no context
// rather than "no Azure context".
var cloudctxVars = []string{
	"CLOUDCTX_CONTEXT",
	"CLOUDCTX_AZURE_LABEL",
	"CLOUDCTX_STORE",
	"AZURE_CONFIG_DIR",
	"AWS_CONFIG_FILE",
	"AWS_SHARED_CREDENTIALS_FILE",
	"AWS_PROFILE",
}

// childEnv is the environment a spawned command sees, given the parent's.
//
// A bare `az` — the shared-login path — must not inherit a context's
// AZURE_CONFIG_DIR. Inside a `cloudctx use` window it otherwise reads that
// context's token store, so --bare-az would mint the context's token and cache
// it under the shared-login slot, where a later unscoped run would reuse it.
// Anything routed through cloudctx keeps the parent environment: cloudctx
// builds the child's environment itself.
func childEnv(name string, parent []string) []string {
	if name != "az" {
		return parent
	}
	out := make([]string, 0, len(parent))
	for _, kv := range parent {
		key, _, _ := strings.Cut(kv, "=")
		if slices.Contains(cloudctxVars, key) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// execRunner is the real [Runner]: it launches name with args, waits for it,
// and returns the two streams buffered whole. It blocks for as long as the
// child takes, which for an interactive `az login` is as long as the person
// takes. The child's environment comes from [childEnv].
func execRunner(name string, args ...string) (stdout, stderr []byte, err error) {
	// The command line is built in this package from a fixed argv shape and a
	// cloudctx context name; nothing here is interpolated into a shell, and
	// running az through cloudctx is what pimctl exists to do.
	//
	// Deliberately exec.Command and not CommandContext: cancelling the context
	// would kill a half-finished `az login` device-code flow, and an interrupt
	// today lets the child finish. Changing that is the owner's call.
	cmd := exec.Command(name, args...) //nolint:gosec,noctx // see above
	cmd.Env = childEnv(name, os.Environ())
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err = cmd.Run()
	return out.Bytes(), errb.Bytes(), err
}

// DefaultRunner shells out for real.
var DefaultRunner Runner = execRunner

// tokenResponse is the subset of `az account get-access-token -o json` that
// pimctl reads. az emits more fields; the rest carry nothing the PIM APIs need.
type tokenResponse struct {
	AccessToken string `json:"accessToken"` // the bearer token; never logged, never written outside the 0600 cache.
	// ExpiresOn is az's local-time string, not RFC 3339, and is parsed in the
	// machine's own location.
	ExpiresOn string `json:"expiresOn"`
	Tenant    string `json:"tenant"` // the tenant az minted it for, used to confirm the context resolved as expected.
}

// TokenArgs builds the argv for minting an ARM token in the given cloudctx
// context. An empty context yields a bare `az` invocation.
func TokenArgs(context string) (name string, args []string) {
	azArgs := []string{"account", "get-access-token", "--resource", ARMResource, "-o", "json"}
	if context == "" {
		return "az", azArgs
	}
	return "cloudctx", append([]string{"exec", context, "--"}, append([]string{"az"}, azArgs...)...)
}

// AcquireCached returns a usable token for the context, from the on-disk cache
// when one is valid and by invoking cloudctx/az when it is not.
//
// A cached entry is only used when it was minted for the tenant the context is
// pinned to today. The cache lives outside cloudctx's per-context store, so
// nothing sweeps it when a context is deleted and recreated against a different
// tenant, or repointed at one — and a token for the wrong tenant is the one
// failure this tool must not have. The tenant comes from `cloudctx show`, which
// reads a local file and spawns no az, so the check costs a process launch
// rather than the 1.23 s mint it protects.
//
// A context that names no tenant is not checked: there is nothing to check
// against, and the entry's own context name still has to match.
func AcquireCached(context string, run Runner, refresh bool) (*Token, error) {
	if !refresh {
		tok, ok, err := cachedTokenFor(context, run)
		if err != nil {
			return nil, err
		}
		if ok {
			return tok, nil
		}
	}
	tok, err := Acquire(context, run)
	if err != nil {
		return nil, err
	}
	WriteTokenCache(tok, run)
	return tok, nil
}

// cachedTokenFor returns the cached token for a context when there is one that
// may still be used: unexpired, and minted for the tenant that context means
// today.
//
// Where "today" comes from depends on the path, and neither costs an az spawn.
// A named context is pinned to a tenant in cloudctx's registry, read with
// `cloudctx show`. The shared `az login` is pinned to nothing, so its tenant is
// whichever one az's own default subscription belongs to — a file read, no
// cloudctx involved, which is what keeps the bare path working on a machine
// that has never heard of cloudctx.
//
// Either source may decline to answer: cloudctx missing or the context gone,
// az's profile absent or in a shape pimctl does not recognise. That makes the
// check unenforceable rather than failed, so the cache is skipped and the token
// minted, which fails loudly if something really is wrong. The only error
// returned is a cache file whose mode has been widened, which the user needs
// told about rather than silently worked around.
func cachedTokenFor(context string, run Runner) (tok *Token, ok bool, err error) {
	wantTenant, known := expectedTenant(context, run)
	if !known {
		return nil, false, nil
	}
	return ReadTokenCache(context, wantTenant, TokenCacheMargin, run)
}

// expectedTenant is the tenant a context should have minted its token for.
//
// known is false when nothing can be said, in which case the caller mints
// rather than trusting the cache. An empty tenantID with known true is a
// context that names no tenant: there is nothing to compare, and the entry's
// own context name still has to match.
func expectedTenant(context string, run Runner) (tenantID string, known bool) {
	if context == "" {
		return defaultAzTenant()
	}
	tenant, err := ContextTenant(context, run)
	if err != nil {
		return "", false
	}
	return tenant, true
}

// Acquire mints one ARM token for a cloudctx context and decodes its claims.
//
// A named context is refused before anything is spawned when the installed
// cloudctx is missing or predates [MinCloudctxVersion]: pimctl drives it
// through the companion contract, and a cloudctx that does not implement it
// would be driven by guesswork. The nameless shared `az login` never consults
// cloudctx and is unaffected.
func Acquire(context string, run Runner) (*Token, error) {
	if run == nil {
		run = DefaultRunner
	}
	if context != "" {
		if err := RequireCloudctx(run); err != nil {
			return nil, err
		}
	}
	name, args := TokenArgs(context)
	stdout, stderr, err := run(name, args...)
	if err != nil {
		return nil, &ExecError{
			Context: context,
			Cmd:     name + " " + strings.Join(args, " "),
			Stderr:  string(stderr),
			Err:     err,
		}
	}
	// Only stdout is parsed. A zero exit code is success even when az wrote to
	// stderr — extensions emit deprecation and import warnings there routinely,
	// and treating that noise as failure would break every call.
	var tr tokenResponse
	if err = json.Unmarshal(extractJSONObject(stdout), &tr); err != nil {
		msg := fmt.Sprintf("could not parse the token JSON from %s: %v", name, err)
		if noise := strings.TrimSpace(string(stderr)); noise != "" {
			msg += fmt.Sprintf("\n%s also wrote to stderr, which may explain it:\n%s", name, noise)
		}
		return nil, errors.New(msg)
	}
	if tr.AccessToken == "" {
		return nil, fmt.Errorf("%s returned an empty accessToken", name)
	}
	claims, err := DecodeClaims(tr.AccessToken)
	if err != nil {
		return nil, err
	}
	t := &Token{
		Context:           context,
		AccessToken:       tr.AccessToken,
		ExpiresOn:         tr.ExpiresOn,
		Tenant:            tr.Tenant,
		PrincipalID:       claims.OID,
		TenantID:          claims.TID,
		UserPrincipalName: claims.User(),
	}
	if t.TenantID == "" {
		t.TenantID = tr.Tenant
	}
	if t.PrincipalID == "" {
		return nil, errors.New("the ARM token has no oid claim; cannot determine the principal id")
	}
	return t, nil
}

// extractJSONObject returns the outermost JSON object in b, tolerating a
// warning line printed before or after it. Azure CLI extensions occasionally
// emit warnings on stdout rather than stderr, which would otherwise turn a
// perfectly good token response into a parse error.
func extractJSONObject(b []byte) []byte {
	start := bytes.IndexByte(b, '{')
	end := bytes.LastIndexByte(b, '}')
	if start < 0 || end < start {
		return b
	}
	return b[start : end+1]
}

// Claims is the subset of JWT payload fields pimctl needs.
type Claims struct {
	// OID is the signed-in user's object id, which every activation request
	// must carry as principalId even when the eligibility came through a group.
	OID string `json:"oid"`
	TID string `json:"tid"`   // the tenant id, used to name the tenant in the claims-challenge recovery command.
	AUD string `json:"aud"`   // the audience, which must be the ARM endpoint.
	APP string `json:"appid"` // the client id az used, which is why Graph PIM scopes are out of reach.
	// UPN is present for a member; a guest's token may carry only
	// unique_name or preferred_username, so all three are read.
	UPN               string `json:"upn"`
	UniqueName        string `json:"unique_name"`        // a guest's fallback identifier.
	PreferredUsername string `json:"preferred_username"` // the last fallback, and what most tokens carry.
}

// User returns the best available human identifier for the token's owner.
func (c Claims) User() string {
	for _, v := range []string{c.UPN, c.PreferredUsername, c.UniqueName} {
		if v != "" {
			return v
		}
	}
	return ""
}

// DecodeClaims parses (without verifying) the payload of a JWT. Verification is
// the resource server's job; pimctl only needs oid and tid.
func DecodeClaims(jwt string) (*Claims, error) {
	// header.payload.signature; only the payload is read.
	const jwtParts = 3
	parts := strings.Split(jwt, ".")
	if len(parts) < jwtParts {
		return nil, fmt.Errorf("access token is not a JWT (expected 3 dot-separated parts, got %d)", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil, fmt.Errorf("could not base64url-decode the token payload: %w", err)
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, fmt.Errorf("could not parse the token payload as JSON: %w", err)
	}
	return &c, nil
}
