// The persisted PIM policy cache: one file per context holding every
// (scope, role) policy that context has read. The eligibility listing, and the
// directory and atomic write both files share, are in cache.go; what a policy
// contains is armclient.RoleSettings.

package cache

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/larsakerlund/pimctl/internal/armclient"
	"github.com/larsakerlund/pimctl/internal/store"
)

// A PIM policy costs two sequential ARM GETs — measured at 1.18s per
// (scope, role) — and the in-process cache on armclient.Client dies with the
// process, so every `pimctl up` paid it again. Persisting it takes that off the
// critical path of the command people run most.
//
// The risk is a policy change going unseen: a shortened maximum duration, or
// Justification/Ticketing newly required. That is bounded three ways — a 24h
// TTL, `--refresh`, and the fact that a stale policy cannot cause a wrong
// activation, only a rejected one. ARM validates every request against the live
// policy, so the worst case is a clear RoleAssignmentRequestPolicyValidationFailed
// naming the rule, after which the entry is dropped and re-read.

// PolicyTTL is how long a persisted policy stays usable. Twenty-four hours is
// long because the failure it risks is cheap: ARM validates every request
// against the live policy, so a stale entry costs one clear rejection and a
// re-read, never a wrong activation.
const PolicyTTL = 24 * time.Hour

// policyVersion is the on-disk format of [policyFile]. A file written by any
// other version is treated as absent, so a shape change costs one re-read
// rather than a mis-parse.
const policyVersion = 1

// policyFile is one context's whole policy cache: every entry it has read, in
// a single file keyed by [PolicyKey]. One file rather than one per role because
// `pimctl up` on a dozen roles would otherwise be a dozen opens on the warm
// path, and the whole map is small enough to rewrite on every store.
type policyFile struct {
	Version int                    `json:"version"` // the on-disk format; a mismatch means "absent".
	Entries map[string]policyEntry `json:"entries"` // every policy this context has read, keyed by [PolicyKey].
}

// policyEntry is one cached policy and when it was read; the TTL is measured
// from FetchedAt. A nil Settings is treated as a miss, so a half-written entry
// can never be served as a policy with no rules.
type policyEntry struct {
	FetchedAt time.Time               `json:"fetchedAt"` // when ARM answered; [PolicyTTL] is measured from it.
	Settings  *armclient.RoleSettings `json:"settings"`  // the policy itself; nil is treated as a miss.
}

// policyPath is where one context's policy cache lives. FileName keeps the
// unnamed az login in a file of its own, so it can never share one with a
// named context.
func policyPath(context string) (string, error) {
	dir, err := store.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "policies-"+store.FileName(context)+".json"), nil
}

// PolicyKey identifies a policy by scope and role definition, matching the
// in-process cache's key so the two cannot disagree. Both halves are
// lower-cased and the role is reduced to its bare GUID, because the same
// definition arrives qualified under whichever scope granted it — the same
// re-qualification that the activation request itself has to do.
func PolicyKey(scope, roleDefinitionID string) string {
	return strings.ToLower(scope) + "|" + strings.ToLower(armclient.RoleDefinitionGUID(roleDefinitionID))
}

// readPolicies loads the cached policies for a context. A missing, corrupt or
// wrong-version file is an empty map, never an error: the caller just reads
// from ARM instead.
func readPolicies(context string) map[string]policyEntry {
	path, err := policyPath(context)
	if err != nil {
		return nil
	}
	raw, err := os.ReadFile(path) //nolint:gosec // path is the cache dir plus a sanitised context name
	if err != nil {
		return nil
	}
	var f policyFile
	if err = json.Unmarshal(raw, &f); err != nil || f.Version != policyVersion {
		return nil
	}
	return f.Entries
}

// writePolicies replaces a context's policy file. Failures are silent: a cache
// that cannot be written must not break the activation that produced the data.
func writePolicies(context string, entries map[string]policyEntry) {
	path, err := policyPath(context)
	if err != nil {
		return
	}
	blob, err := json.Marshal(policyFile{Version: policyVersion, Entries: entries})
	if err != nil {
		return
	}
	if err = os.MkdirAll(filepath.Dir(path), store.DirMode); err != nil {
		return
	}
	store.WriteAtomic(path, blob)
}

// LookupPolicy returns the cached policy for one (scope, role), or nil when
// there is none, it has no settings, or it is older than [PolicyTTL]. refresh
// is `--refresh` on the command line: it forces a miss, so the caller reads
// from ARM. Every nil is a miss, never an error — the caller's fallback is the
// live read it would have done anyway.
func LookupPolicy(context, scope, roleDefinitionID string, refresh bool) *armclient.RoleSettings {
	if refresh {
		return nil
	}
	entry, ok := readPolicies(context)[PolicyKey(scope, roleDefinitionID)]
	if !ok || entry.Settings == nil {
		return nil
	}
	if age := time.Since(entry.FetchedAt); age < 0 || age > PolicyTTL {
		return nil
	}
	return entry.Settings
}

// StorePolicies merges freshly-read policies into the context's cache, keyed by
// [PolicyKey], stamping them all with the current time. Nil settings are
// skipped, existing entries for other keys are preserved, and the file is
// rewritten in full. It writes to disk and fails silently.
func StorePolicies(context string, settings map[string]*armclient.RoleSettings) {
	if len(settings) == 0 {
		return
	}
	entries := readPolicies(context)
	if entries == nil {
		entries = map[string]policyEntry{}
	}
	now := time.Now()
	for key, s := range settings {
		if s != nil {
			entries[key] = policyEntry{FetchedAt: now, Settings: s}
		}
	}
	writePolicies(context, entries)
}

// DropPolicy removes one entry, called when ARM answers
// RoleAssignmentRequestPolicyValidationFailed: the rejection proves the cached
// copy no longer matches the live policy, so the next read goes to ARM. It
// rewrites the file and fails silently; dropping an entry that is not there is
// a no-op.
func DropPolicy(context, scope, roleDefinitionID string) {
	entries := readPolicies(context)
	if entries == nil {
		return
	}
	delete(entries, PolicyKey(scope, roleDefinitionID))
	writePolicies(context, entries)
}

// ClearPolicies removes every context's policy file and reports how many it
// deleted, for `pimctl cache clear`.
func ClearPolicies() (int, error) {
	return store.RemoveFiles("policies-")
}
