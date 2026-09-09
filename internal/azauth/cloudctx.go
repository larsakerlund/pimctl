// Everything pimctl asks the cloudctx binary for, and the version gate in
// front of it: enumerating contexts, reading one context's registry entry
// (tenant and store path), and where a companion keeps per-context state.
// Minting a token inside a context is token.go's job, and the token file's
// shape is tokencache.go's; nothing here reads or writes a credential.

package azauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/larsakerlund/pimctl/internal/store"
)

// cloudctxBin is the binary pimctl delegates per-context isolation to.
const cloudctxBin = "cloudctx"

// MinCloudctxVersion is the oldest cloudctx pimctl will drive.
//
// 1.4.0 is the release that declared the companion contract
// (cloudctx docs/companions.md): `list --names`, the `store:` line of `show`,
// and the CLOUDCTX_STORE variable. pimctl depends on all three, so an older
// cloudctx is refused rather than worked around — the fallbacks that made the
// undeclared surfaces usable are what the contract exists to retire.
const MinCloudctxVersion = "1.4.0"

// versionCacheTTL is how long a probed cloudctx version is trusted.
//
// The probe is a process launch, and cloudctx is Python: 0.65 s measured here,
// the same order as the 1.23 s token mint the whole cache layer exists to
// avoid. Doing it on every command would double the cost of a warm one, so the
// answer is kept on disk and re-probed daily — or immediately, whenever the
// binary's path, size or mtime changes, which is what an update looks like.
const versionCacheTTL = 24 * time.Hour

// pimctlStateDirName is the directory pimctl keeps per-context state in, inside
// the context's own cloudctx store. The contract's shape is
// `$CLOUDCTX_STORE/<tool>/`.
const pimctlStateDirName = "pimctl"

// envStore is the variable a cloudctx context exports holding its own
// directory, `~/.cloudctx/<name>`. Inside a `cloudctx use` window it answers
// the store question for free, with no cloudctx spawn at all.
const envStore = "CLOUDCTX_STORE"

// envContext is the variable naming the context a shell is scoped to. It is
// read here only to decide whether [envStore] describes the context being asked
// about; deciding which context a *command* acts on is internal/cli's job.
const envContext = "CLOUDCTX_CONTEXT"

// CloudctxInstalled reports whether the cloudctx binary is on PATH.
//
// pimctl works without it: the shared `az login` is the fallback for everything
// except -c and --all-contexts, which are cloudctx's own features. The answer is
// looked up once, because it cannot change inside one command and PATH lookups
// on a cold filesystem are not free.
var CloudctxInstalled = sync.OnceValue(func() bool {
	_, err := exec.LookPath(cloudctxBin)
	return err == nil
})

// errCloudctxMissing explains that a cloudctx-only feature was asked for on a
// machine without it, rather than reporting "executable file not found".
var errCloudctxMissing = errors.New(
	"cloudctx is not installed, so there are no contexts to act on.\n" +
		"pimctl works without it against your `az login`; cloudctx is a separate tool that adds " +
		"per-context isolation and the -c/--all-contexts flags")

// ErrCloudctxTooOld is returned when cloudctx is installed but predates the
// companion contract. Callers match it with [errors.Is] to tell "your cloudctx
// needs updating" from "that context does not exist".
var ErrCloudctxTooOld = errors.New("cloudctx is older than pimctl requires")

// ContextInfo is what `cloudctx show <name>` says about one context: the fields
// pimctl reads out of the registry entry, and the store path underneath it.
type ContextInfo struct {
	// Name is the context as cloudctx knows it.
	Name string
	// Tenant is the `azure_tenant` field, or "" when the context pins none —
	// which means "there is nothing to check a cached token against".
	Tenant string
	// Store is the context's own directory (`~/.cloudctx/<name>`), from the
	// `store:` line. Empty when the line is absent, which a cloudctx meeting
	// [MinCloudctxVersion] never does.
	Store string
}

// showCache memoises `cloudctx show` for the life of the process. The registry
// is a local file that cannot change under a running command, and the call
// costs a 0.65 s process launch, so asking twice for the tenant and the store
// path would double the cost of every warm command.
var showCache sync.Map // context name → ContextInfo.

// RequireCloudctx reports whether the installed cloudctx can be driven by this
// pimctl: nil when it is at least [MinCloudctxVersion], [errCloudctxMissing]
// when there is no cloudctx at all, and an error wrapping [ErrCloudctxTooOld]
// naming both versions when it is present but older.
//
// It is the gate in front of every cloudctx invocation. The version comes from
// the on-disk probe cache, so the common case is a stat and a small read.
func RequireCloudctx(run Runner) error {
	if !CloudctxInstalled() {
		return errCloudctxMissing
	}
	version, err := CloudctxVersion(run)
	if err != nil {
		return err
	}
	if compareVersions(version, MinCloudctxVersion) < 0 {
		return fmt.Errorf(
			"%w: cloudctx %s is installed, and pimctl needs %s or newer.\n"+
				"Update it with `cloudctx self-update`. %s added the companion contract pimctl drives it through "+
				"(`cloudctx list --names`, the store path in `cloudctx show`, and $CLOUDCTX_STORE)",
			ErrCloudctxTooOld, version, MinCloudctxVersion, MinCloudctxVersion)
	}
	return nil
}

// versionProbe is the cached answer to "which cloudctx is on this machine",
// tied to the binary it was read from so an update invalidates it at once.
type versionProbe struct {
	Version string `json:"version"` // as cloudctx reported it, e.g. "1.4.0".
	Path    string `json:"path"`    // the resolved binary the version belongs to.
	Size    int64  `json:"size"`    // its size in bytes, part of the identity.
	ModTime int64  `json:"modTime"` // its mtime in Unix nanoseconds, ditto.
	// CheckedAt is when the probe ran, so a hand-edited or replaced-in-place
	// binary is still re-probed within [versionCacheTTL].
	CheckedAt time.Time `json:"checkedAt"`
}

// CloudctxVersion returns the installed cloudctx's version, from the probe
// cache when it still describes the binary on PATH and from a `cloudctx
// --version` spawn otherwise. The error explains an unreadable version rather
// than guessing one: a version pimctl cannot read is one it cannot vouch for.
//
// It is a variable for the same reason [CloudctxInstalled] is: it reads the
// host — PATH, the binary's mtime, a cache file — so a test that left it alone
// would assert on whichever cloudctx the machine running it happens to have.
// [probeVersionCached] is the implementation, and what a test restores.
var CloudctxVersion = probeVersionCached

// probeVersionCached is [CloudctxVersion]'s implementation: the probe cache in
// front of a `cloudctx --version` spawn.
func probeVersionCached(run Runner) (string, error) {
	path, err := exec.LookPath(cloudctxBin)
	if err != nil {
		return "", errCloudctxMissing
	}
	info, statErr := os.Stat(path)
	if statErr == nil {
		if cached, ok := readVersionProbe(path, info); ok {
			return cached, nil
		}
	}
	version, err := probeCloudctxVersion(run)
	if err != nil {
		return "", err
	}
	if statErr == nil {
		writeVersionProbe(versionProbe{
			Version:   version,
			Path:      path,
			Size:      info.Size(),
			ModTime:   info.ModTime().UnixNano(),
			CheckedAt: time.Now(),
		})
	}
	return version, nil
}

// probeCloudctxVersion runs `cloudctx --version` and reads the version out of
// it. cloudctx prints `cloudctx X.Y.Z` on stdout and exits 0.
func probeCloudctxVersion(run Runner) (string, error) {
	if run == nil {
		run = DefaultRunner
	}
	stdout, stderr, err := run(cloudctxBin, "--version")
	if err != nil {
		return "", &ExecError{
			Cmd:    cloudctxBin + " --version",
			Stderr: string(stderr),
			Err:    err,
		}
	}
	// argparse's --version writes to stdout on Python 3.4+, but an older
	// runtime or a wrapper may put it on stderr; both are read rather than
	// failing over a stream choice.
	version := parseVersion(string(stdout) + "\n" + string(stderr))
	if version == "" {
		return "", fmt.Errorf(
			"could not read a version from `cloudctx --version`, which printed: %s",
			strings.TrimSpace(string(stdout)+" "+string(stderr)))
	}
	return version, nil
}

// parseVersion pulls the first dotted numeric version out of a line such as
// `cloudctx 1.4.0`. It returns "" when there is none, which the caller reports
// rather than treating as old or new.
func parseVersion(out string) string {
	// A version is at least major.minor: a bare word, or a number on its own,
	// is prose rather than the thing being looked for.
	const leastComponents = 2
	for field := range strings.FieldsSeq(out) {
		field = strings.TrimPrefix(strings.TrimSpace(field), "v")
		parts := strings.Split(field, ".")
		if len(parts) < leastComponents {
			continue
		}
		if _, err := strconv.Atoi(parts[0]); err != nil {
			continue
		}
		return field
	}
	return ""
}

// compareVersions orders two dotted versions numerically, returning -1, 0 or 1.
// A component that is not a number sorts as 0, so a suffixed build such as
// "1.4.0rc1" is treated as 1.4.0 rather than rejected.
func compareVersions(a, b string) int {
	fields := func(s string) []int {
		parts := strings.Split(s, ".")
		out := make([]int, len(parts))
		for i, p := range parts {
			digits := strings.TrimLeft(p, "0123456789")
			n, err := strconv.Atoi(strings.TrimSuffix(p, digits))
			if err != nil {
				n = 0
			}
			out[i] = n
		}
		return out
	}
	x, y := fields(a), fields(b)
	for i := range max(len(x), len(y)) {
		var xi, yi int
		if i < len(x) {
			xi = x[i]
		}
		if i < len(y) {
			yi = y[i]
		}
		if xi != yi {
			if xi < yi {
				return -1
			}
			return 1
		}
	}
	return 0
}

// versionProbePath is where the probe cache lives: one file for the machine,
// beside the other caches rather than in a context's store, because the
// question it answers is about the binary and not about any context.
func versionProbePath() (string, error) {
	dir, err := store.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "cloudctx-version.json"), nil
}

// readVersionProbe returns the cached version when it was read from this exact
// binary and is younger than [versionCacheTTL]. Every other outcome — no file,
// corrupt, another binary, too old — is a miss, because the answer to all of
// them is to probe again.
func readVersionProbe(path string, info os.FileInfo) (string, bool) {
	p, err := versionProbePath()
	if err != nil {
		return "", false
	}
	raw, err := os.ReadFile(p) //nolint:gosec // path is the cache dir plus a fixed name
	if err != nil {
		return "", false
	}
	var probe versionProbe
	if err = json.Unmarshal(raw, &probe); err != nil {
		return "", false
	}
	if probe.Version == "" || probe.Path != path || probe.Size != info.Size() {
		return "", false
	}
	if probe.ModTime != info.ModTime().UnixNano() {
		return "", false
	}
	if time.Since(probe.CheckedAt) > versionCacheTTL {
		return "", false
	}
	return probe.Version, true
}

// writeVersionProbe stores a probe result. Failures are silent: a probe that
// cannot be cached costs the next command one process launch, which is not
// worth failing a command over.
func writeVersionProbe(probe versionProbe) {
	path, err := versionProbePath()
	if err != nil {
		return
	}
	if err = os.MkdirAll(filepath.Dir(path), store.DirMode); err != nil {
		return
	}
	blob, err := json.Marshal(probe)
	if err != nil {
		return
	}
	store.WriteAtomic(path, blob)
}

// ListContexts returns every cloudctx context name, sorted as cloudctx sorts
// them. An empty registry yields no names and no error.
//
// It reads `cloudctx list --names`, the contract's machine-readable listing:
// one bare name per line and nothing else. The human `cloudctx list` is not
// parsed at all — its headers, markers and "no contexts. Create one with: …"
// sentence are what made --all-contexts once act on a context called "no".
func ListContexts(run Runner) ([]string, error) {
	if run == nil {
		run = DefaultRunner
	}
	if err := RequireCloudctx(run); err != nil {
		return nil, err
	}
	stdout, stderr, err := run(cloudctxBin, "list", "--names")
	if err != nil {
		if isMissingBinary(err) {
			return nil, errCloudctxMissing
		}
		return nil, &ExecError{Cmd: "cloudctx list --names", Stderr: string(stderr), Err: err}
	}
	return parseContextNames(string(stdout)), nil
}

// parseContextNames reads the listing: one name per line, nothing else. Blank
// lines are dropped, so an empty registry yields no names.
func parseContextNames(out string) []string {
	var names []string
	for line := range strings.SplitSeq(out, "\n") {
		if name := strings.TrimSpace(line); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// ShowContext reads one context's registry entry with `cloudctx show <name>`,
// which spawns no az: the registry is a local file, so this costs a process
// launch and no network. The result is memoised for the life of the process.
//
// It returns an [ExecError] when cloudctx refuses — an unknown context prints
// `cloudctx: error: unknown context '<name>'` and exits 1 — and the version
// gate's error when cloudctx is too old to be driven.
func ShowContext(name string, run Runner) (ContextInfo, error) {
	if name == "" {
		return ContextInfo{}, nil // the shared az login is not a context.
	}
	if cached, ok := showCache.Load(name); ok {
		if info, isInfo := cached.(ContextInfo); isInfo {
			return info, nil
		}
	}
	if run == nil {
		run = DefaultRunner
	}
	if err := RequireCloudctx(run); err != nil {
		return ContextInfo{}, err
	}
	stdout, stderr, err := run(cloudctxBin, "show", name)
	if err != nil {
		return ContextInfo{}, &ExecError{
			Context: name,
			Cmd:     "cloudctx show " + name,
			Stderr:  string(stderr),
			Err:     err,
		}
	}
	info := parseShow(name, string(stdout))
	showCache.Store(name, info)
	return info, nil
}

// parseShow reads `cloudctx show`: a `[name]` header, one `key = value` line
// per registry field, then a blank line and the path lines, of which `store:`
// is the first. Only the two fields pimctl needs are kept; everything else is
// ignored, so a new field or path line cannot break the parse.
func parseShow(name, out string) ContextInfo {
	info := ContextInfo{Name: name}
	for line := range strings.SplitSeq(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if rest, found := strings.CutPrefix(trimmed, "store:"); found {
			info.Store = strings.TrimSpace(rest)
			continue
		}
		key, value, found := strings.Cut(trimmed, "=")
		if !found || strings.TrimSpace(key) != "azure_tenant" {
			continue
		}
		info.Tenant = strings.Trim(strings.TrimSpace(value), `"'`)
	}
	return info
}

// ForgetContextCache drops the memoised `cloudctx show` answers.
//
// Nothing in the shipped binary calls it: a pimctl process is short-lived and
// the registry cannot change under it. Tests call it between fake registries,
// so one test's contexts cannot answer the next one's questions.
func ForgetContextCache() {
	showCache.Clear()
}

// ContextTenant returns the Azure tenant id a cloudctx context is pinned to, or
// "" when the context does not name one.
//
// It is what makes a cached token safe to reuse: [ReadTokenCache] refuses an
// entry minted for a different tenant, and this is where the expected one comes
// from. See [ShowContext] for the cost, which is shared with the store lookup.
func ContextTenant(name string, run Runner) (string, error) {
	if name == "" {
		return "", nil // the shared az login is pinned to nothing.
	}
	info, err := ShowContext(name, run)
	if err != nil {
		return "", err
	}
	return info.Tenant, nil
}

// ContextStateDir returns the directory pimctl keeps a context's own state in:
// `$CLOUDCTX_STORE/pimctl`, inside cloudctx's isolation boundary, so
// `cloudctx delete <name>` sweeps it and `--keep-store` keeps it.
//
// ok is false whenever the store cannot be established — no cloudctx, a
// cloudctx too old to export it, an unknown context, or the nameless shared
// `az login`, which belongs to no context. The caller then falls back to the
// XDG directory, which is where everything lived before the contract existed.
//
// Inside a `cloudctx use` window the answer comes from the environment and
// costs nothing; otherwise it costs the `cloudctx show` launch, shared with the
// tenant lookup.
func ContextStateDir(name string, run Runner) (dir string, ok bool) {
	if name == "" {
		return "", false
	}
	if root := ambientStore(name); root != "" {
		return filepath.Join(root, pimctlStateDirName), true
	}
	if !CloudctxInstalled() {
		return "", false
	}
	info, err := ShowContext(name, run)
	if err != nil || info.Store == "" {
		return "", false
	}
	return filepath.Join(info.Store, pimctlStateDirName), true
}

// ambientStore returns $CLOUDCTX_STORE when this process is running inside a
// `cloudctx use` window for the named context, and "" otherwise. The two
// variables are read together: $CLOUDCTX_STORE alone would be the wrong
// context's directory whenever pimctl is asked about another one with -c.
func ambientStore(name string) string {
	if strings.TrimSpace(os.Getenv(envContext)) != name {
		return ""
	}
	return strings.TrimSpace(os.Getenv(envStore))
}

// ContextStatePath is where one per-context file lives: under the context's
// cloudctx store when there is one, and under pimctl's own directory otherwise.
// fallbackDir is the pre-contract location, and is also where the file is
// migrated from the first time the store answers.
//
// The file is named identically in both places, so a file that has moved is
// still recognisably the same one.
func ContextStatePath(name, fileName, fallbackDir string, run Runner) string {
	dir, ok := ContextStateDir(name, run)
	if !ok {
		return filepath.Join(fallbackDir, fileName)
	}
	path := filepath.Join(dir, fileName)
	store.MoveIfAbsent(filepath.Join(fallbackDir, fileName), path)
	return path
}

// ContextStateDirs lists the per-context state directories that exist today,
// for the commands that sweep across every context rather than acting on one.
//
// It is best-effort by design: no cloudctx, one too old, a registry that
// cannot be read or a context whose store has never been used all yield fewer
// directories rather than an error. The callers — `pimctl cache clear` and the
// activation-record sweep — delete what they can find, and anything missed is
// re-derived or rejected by the tenant check on its next read.
func ContextStateDirs(run Runner) []string {
	if !CloudctxInstalled() {
		return nil
	}
	names, err := ListContexts(run)
	if err != nil {
		return nil
	}
	dirs := make([]string, 0, len(names))
	for _, name := range names {
		if dir, ok := ContextStateDir(name, run); ok {
			dirs = append(dirs, dir)
		}
	}
	return dirs
}
