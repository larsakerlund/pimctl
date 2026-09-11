// The activation record on disk: its entries, where the file lives, and when
// an entry has nothing left to say. Turning entries into rows is
// localrecord.go; keeping them true against ARM is reconcile.go and verify.go.

package cli

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/larsakerlund/pimctl/internal/armclient"
	"github.com/larsakerlund/pimctl/internal/azauth"
	"github.com/larsakerlund/pimctl/internal/store"
)

// The activation record is a local note of what this machine did.
//
// Reading activation state from ARM costs a 25-scope fan-out — 2.44s at best,
// and it is the only network on the critical path of `pimctl status`. But
// pimctl already knows what it just activated, so it writes that down and
// renders from it instantly.
//
// The record is strictly ADDITIVE to ARM, never a replacement. It cannot see a
// role activated in the portal, by another machine, or by a colleague, and it
// cannot see one revoked out from under it. So every row it produces is marked
// unconfirmed until the ARM fan-out agrees, and the fan-out still runs on every
// command that shows activation state. What the record buys is *when* the first
// useful output appears, not what the answer eventually is.
//
// Nothing here is a secret: role names, scopes and timestamps, no tokens.

// recordVersion is the record file's schema version, checked on every read. A
// file carrying anything else is treated as empty rather than migrated: ARM is
// the authority anyway, so an unreadable record costs one fan-out and nothing
// else.
const recordVersion = 2

// recordRevoked marks an entry as a tombstone: a role this machine has just
// given up. It is kept, rather than deleted, because ARM's per-scope listing
// goes on reporting the role as Activated for minutes afterwards, and
// a deleted entry would let that stale listing put the role straight back.
const recordRevoked = "REVOKED"

// Where an entry's Start came from.
const (
	// startFromARM: ARM's own startDateTime for the activation.
	startFromARM = "arm"
	// startFromLocal: this machine's clock, because ARM did not say. It runs a
	// second or two late for a fresh activation and can be hours out for one
	// that was already active, so it is marked rather than passed off as ARM's.
	startFromLocal = "local"
)

// recordCeiling bounds how long this machine's own record may outrank ARM's
// listing when the listing disagrees.
//
// It is a backstop, not the mechanism. The mechanism is evidence: an activation
// ARM has not listed is checked against its own schedule request, which reports
// Provisioned long before the per-scope listing catches up — see verify.go. The
// ceiling only covers the case where that evidence cannot be had at all: no
// request id, or ARM will not answer for it.
//
// Half an hour is far longer than any lag observed (4.5 minutes at worst) and
// still expires within a working session. It is deliberately not sized to the
// lag: a constant that decides whether a role is held is wrong at both ends,
// which is why it decides nothing on its own.
var recordCeiling = 30 * time.Minute

// recordEntry is one activation this machine performed.
type recordEntry struct {
	Context string `json:"context"` // the cloudctx context the role was activated in.
	// Key is the (context, scope, role GUID) identity, matched against ARM's
	// rows during reconciliation and against a selection during a deactivation.
	Key              string `json:"key"`
	Scope            string `json:"scope"`            // the full ARM scope id, which is what unread-scope matching compares.
	RoleDefinitionID string `json:"roleDefinitionId"` // the role, qualified as ARM reported it.
	Role             string `json:"role"`             // display name, for the table this entry may have to render alone.
	ScopeName        string `json:"scopeName"`        // scope display name, likewise.
	ScopeType        string `json:"scopeType"`        // Subscription, ManagementGroup, ResourceGroup or Resource.
	// RequestID is the schedule request this came from. It is what confirms an
	// activation ARM's per-scope listing has not caught up with, so an entry
	// without one can only be aged out by the ceiling.
	RequestID string `json:"requestId,omitempty"`
	// Start is when the window opened; StartSource says whether that is ARM's
	// answer or this machine's clock.
	Start time.Time `json:"start"`
	// End is when it expires, and is how an entry is pruned: a record is never
	// kept past the window it describes.
	End time.Time `json:"end,omitzero"`
	// Status is the outcome that produced the entry — ACTIVATED, ALREADY
	// ACTIVE — or REVOKED for a tombstone, which records the opposite.
	Status string `json:"status"`
	// StartSource says whether Start came from ARM or from this machine's
	// clock, so a reader can tell a fact from an approximation.
	StartSource string `json:"startSource,omitempty"`
	// Listed records that ARM's own listing has shown this activation at least
	// once. Until it does, the entry is "confirming" and is believed on the
	// strength of its schedule request instead.
	Listed bool `json:"listed,omitempty"`
	// WrittenAt is when this machine last wrote the entry, and what the
	// 30-minute ceiling is measured from. Reconciliation deliberately does not
	// re-stamp it, or a role confirmed hours ago would look freshly activated.
	WrittenAt time.Time `json:"writtenAt"`
}

// Expired reports whether the activation's own window has closed.
func (e recordEntry) Expired(now time.Time) bool {
	return !e.End.IsZero() && !now.Before(e.End)
}

// Revoked reports whether the entry is a tombstone for a role given up here.
func (e recordEntry) Revoked() bool { return e.Status == recordRevoked }

// Denies reports whether a tombstone can suppress a listed activation.
// A window starting after the deactivation is new access and must be shown.
// Without a start time, the listing supplies no evidence of a new window.
func (e recordEntry) Denies(a armclient.Assignment) bool {
	start := a.Properties.StartDateTime
	return e.Revoked() && (start == nil || !start.After(e.WrittenAt))
}

// Confirming reports whether ARM's listing has yet to catch up with an
// activation this machine made. Such an entry is not evidence-free: it is
// checked against its own schedule request before anything is concluded.
func (e recordEntry) Confirming(now time.Time) bool {
	return !e.Revoked() && !e.Listed && !e.PastCeiling(now)
}

// PastCeiling reports whether the entry has outranked ARM for as long as it may.
func (e recordEntry) PastCeiling(now time.Time) bool {
	return now.Sub(e.WrittenAt) > recordCeiling
}

// stale reports whether an entry has nothing left to say. An activation is
// spent once its own window closes; a tombstone is spent once it has held ARM
// off for as long as it may.
func (e recordEntry) stale(now time.Time) bool {
	if e.Revoked() {
		return e.PastCeiling(now)
	}
	return e.Expired(now)
}

// recordKey is an activation's identity in the record: context, scope and role
// definition. It is the same key a row is filed under, so an entry and the row
// rendered from it cannot end up under different names.
func recordKey(context, scope, roleDefinitionID string) string {
	return rowKey(context, scope, armclient.RoleDefinitionGUID(roleDefinitionID))
}

// recordFile is the on-disk shape of one context's record: a version and the
// entries. Nothing in it is a secret — role names, scopes and timestamps, never
// a token.
type recordFile struct {
	Owner   store.Owner   `json:"owner"`   // account whose activations these entries describe.
	Version int           `json:"version"` // a mismatch retires the file rather than risking a misread.
	Entries []recordEntry `json:"entries"` // every activation and tombstone still worth keeping.
}

// stateDir is where pimctl keeps state that is not a cache — the activation
// record is a note of something that happened, not a copy of something
// re-derivable, so it belongs under XDG_STATE_HOME rather than the cache.
func stateDir() (string, error) {
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "pimctl"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// Falling back to the cache directory keeps the feature working on a
		// machine with no home; losing the record only costs speed.
		return store.Dir()
	}
	return filepath.Join(home, ".local", "state", "pimctl"), nil
}

// recordPath locates an account-owned record inside its cloudctx context's
// store, or under stateDir for shared az and unavailable stores. Only files
// with the same account-specific name are migrated from the fallback directory;
// older files without ownership are never attributed to the current account.
func recordPath(owner store.Owner) (string, error) {
	dir, err := stateDir()
	if err != nil {
		return "", err
	}
	name := "active-" + owner.FileName() + ".json"
	return azauth.ContextStatePath(owner.Context, name, dir, azauth.DefaultRunner), nil
}

// readRecord returns an account's live entries, tombstones included.
// A missing, corrupt or wrong-version file is an empty record, never an error:
// the ARM fan-out is the authority and will fill it in.
func readRecord(owner store.Owner) []recordEntry {
	path, err := recordPath(owner)
	if err != nil {
		return nil
	}
	return readOwnedRecordFile(path, &owner)
}

// readRecordFile reads one record file's live entries. A missing, corrupt or
// wrong-version file is an empty record, never an error: the ARM fan-out is the
// authority and will fill it in.
func readRecordFile(path string) []recordEntry { return readOwnedRecordFile(path, nil) }

// readOwnedRecordFile checks file ownership when owner is non-nil. A nil owner
// is reserved for cache-clear reporting, which never renders roles as held.
func readOwnedRecordFile(path string, owner *store.Owner) []recordEntry {
	raw, err := os.ReadFile(path) //nolint:gosec // path is the state dir plus a sanitised context name
	if err != nil {
		return nil
	}
	var f recordFile
	if err = json.Unmarshal(raw, &f); err != nil || f.Version != recordVersion {
		return nil
	}
	if owner != nil && !f.Owner.Matches(*owner) {
		return nil
	}
	now := time.Now()
	live := make([]recordEntry, 0, len(f.Entries))
	for _, e := range f.Entries {
		if !e.stale(now) {
			// Rebuild derived keys so older case-folded context keys cannot
			// change the identity of an account-owned record.
			e.Key = recordKey(e.Context, e.Scope, e.RoleDefinitionID)
			live = append(live, e)
		}
	}
	return live
}

// writeRecord replaces an account's record, pruning expired entries on the way
// out so the file cannot grow without bound.
func writeRecord(owner store.Owner, entries []recordEntry) {
	if !owner.Valid() {
		return
	}
	path, err := recordPath(owner)
	if err != nil {
		return
	}
	now := time.Now()
	keep := make([]recordEntry, 0, len(entries))
	for _, e := range entries {
		if !e.stale(now) {
			keep = append(keep, e)
		}
	}
	blob, err := json.Marshal(recordFile{Version: recordVersion, Owner: owner, Entries: keep})
	if err != nil {
		return
	}
	if err = os.MkdirAll(filepath.Dir(path), store.DirMode); err != nil {
		return
	}
	store.WriteAtomic(path, blob)
}

// contextFromRecordFile recovers a context's name from its record filename,
// for a file with no live entries left to ask. The name is the sanitised form,
// which differs from the real one only for a context whose name needed
// sanitising; the shared az login is named in words, since [store.AzLoginName]
// is a filename rather than something a user typed.
func contextFromRecordFile(fileName string) string {
	name := strings.TrimSuffix(strings.TrimPrefix(fileName, "active-"), ".json")
	if i := strings.LastIndexByte(name, '-'); i >= 0 && len(name[i+1:]) == 64 {
		if _, err := hex.DecodeString(name[i+1:]); err == nil {
			name = name[:i]
		}
	}
	if name == store.AzLoginName {
		return "the shared az login"
	}
	return name
}

// mergeEntries overlays fresh entries onto existing ones, keyed by role+scope.
func mergeEntries(existing, fresh []recordEntry) []recordEntry {
	index := map[string]int{}
	out := make([]recordEntry, 0, len(existing)+len(fresh))
	for _, e := range existing {
		index[e.Key] = len(out)
		out = append(out, e)
	}
	for _, e := range fresh {
		if i, ok := index[e.Key]; ok {
			out[i] = mergeEntry(out[i], e)
			continue
		}
		index[e.Key] = len(out)
		out = append(out, e)
	}
	return out
}

// mergeEntry folds a fresh entry into the one already recorded for that role,
// and never trades a fact for the absence of one.
//
// Blind replacement lost real information. Re-running `up` on a role that is
// already active takes the ALREADY ACTIVE path, which never sends a request and
// so has no request id, no ARM start time and no knowledge of the listing — and
// that emptier entry replaced the richer one written when the role was actually
// activated. The cost was specific: with the request id gone, the confirmation
// step skips the role, which removes exactly the evidence that keeps `status`
// right through ARM's listing lag — the lag the second `up` had just proved was
// still running.
//
// Two things still replace outright. A tombstone is a deliberate contradiction
// of what came before, in either direction. And a request id that differs from
// the recorded one means a genuinely new activation, whose window, start and
// unlisted state are all its own.
func mergeEntry(old, fresh recordEntry) recordEntry {
	if old.Revoked() || fresh.Revoked() {
		return fresh
	}
	if fresh.RequestID != "" && fresh.RequestID != old.RequestID {
		return fresh
	}
	out := fresh
	if out.RequestID == "" {
		out.RequestID = old.RequestID
	}
	// ARM's own start time outranks this machine's clock however recently the
	// clock was read; see startFromARM.
	if out.StartSource != startFromARM && old.StartSource == startFromARM {
		out.Start, out.StartSource = old.Start, old.StartSource
	}
	if out.End.IsZero() {
		out.End = old.End
	}
	// Listed is a fact about the same activation, and the fresh entry is not
	// evidence against it: an ALREADY ACTIVE result never consults the listing.
	if !out.Listed {
		out.Listed = old.Listed
	}
	// ALREADY ACTIVE says only that the role was held, which is what the entry
	// already said. The outcome that created it is the more informative one.
	if out.Status == string(OutcomeAlreadyActive) && old.Status != "" {
		out.Status = old.Status
	}
	if out.ScopeName == "" {
		out.ScopeName = old.ScopeName
	}
	if out.ScopeType == "" {
		out.ScopeType = old.ScopeType
	}
	return out
}

// clearRecords removes every activation record, reporting how many activations
// were forgotten and which contexts they belonged to.
//
// Activations rather than files: "2 activation record(s)" for two files holding
// three activations across two contexts reads as though two roles were
// forgotten, and does not mention that a context the command never named was
// one of them.
func clearRecords() (entries int, contexts []string, err error) {
	dir, dirErr := stateDir()
	if dirErr != nil {
		return 0, nil, dirErr
	}
	seen := map[string]bool{}
	entries, err = clearRecordsIn(dir, seen, &contexts)
	if err != nil {
		return entries, contexts, err
	}
	// Records written since cloudctx gained the companion contract live in the
	// context's own store, so the sweep has to visit those too — otherwise
	// `cache clear --all` would report success and leave every current record
	// in place. A context that cannot be enumerated is skipped: what is left
	// behind costs one ARM fan-out, not a wrong answer.
	for _, contextDir := range azauth.ContextStateDirs(azauth.DefaultRunner) {
		n, sweepErr := clearRecordsIn(contextDir, seen, &contexts)
		entries += n
		if sweepErr != nil {
			return entries, contexts, sweepErr
		}
	}
	slices.Sort(contexts)
	return entries, contexts, nil
}

// clearRecordsIn removes the record files in one directory, counting the
// activations forgotten and adding each file's context to contexts. seen keeps
// a context from being named twice when it has a record in more than one
// place. A directory that does not exist holds no records and is not an error.
func clearRecordsIn(dir string, seen map[string]bool, contexts *[]string) (entries int, err error) {
	files, readErr := os.ReadDir(dir)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return 0, nil
		}
		return 0, readErr
	}
	for _, f := range files {
		if f.IsDir() || !strings.HasPrefix(f.Name(), "active-") {
			continue
		}
		path := filepath.Join(dir, f.Name())
		// The context comes from the filename, not from the entries: a file
		// whose activations have all expired still belongs to a context, and
		// naming only the contexts with live entries meant a command that
		// deleted three files could report none of them.
		name := contextFromRecordFile(f.Name())
		for _, e := range readRecordFile(path) {
			entries++
			if e.Context != "" {
				name = e.Context // the real name, not the sanitised filename.
			}
		}
		if !seen[name] {
			seen[name] = true
			*contexts = append(*contexts, name)
		}
		if err := os.Remove(path); err != nil {
			return entries, err
		}
	}
	return entries, nil
}
