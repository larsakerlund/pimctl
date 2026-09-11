// Writing the activation record: one role's outcome as it lands, and the
// rewrite against ARM's listing afterwards. The file itself is record.go, and
// the schedule-request evidence this leans on is gathered in verify.go.

package cli

import (
	"time"
)

// Keeping the record true. Two things write to it: a run, as each role lands,
// and reconciliation against ARM once its listing has been read. Neither is
// simply "believe the last thing seen" — ARM's per-scope listing runs minutes
// behind its own writes in both directions, so a fresh activation is believed
// against its own schedule request (see verify.go) and a fresh deactivation is
// kept as a tombstone until the listing agrees the role is gone.

// recordResult writes one role's outcome to the record as it lands.
//
// Per role rather than per batch: a batch takes as long as its slowest role, and
// an interrupt partway through should still leave behind what actually
// succeeded. The file is a few KB, so the repeated read-merge-write costs
// nothing worth optimising.
//
// Callers serialise this; see executeActivations.
func recordResult(r result) {
	switch r.Outcome {
	case OutcomeActivated, OutcomeAlreadyActive:
		recordActivations([]result{r})
	case OutcomeDeactivated, OutcomeNotActive:
		forgetActivations([]result{r})
	default:
		// Everything else — failed, pending, aborted — says nothing certain
		// about what is held, so it is not written down.
	}
}

// recordActivations merges freshly activated roles into the record.
func recordActivations(results []result) {
	byContext := map[string][]recordEntry{}
	now := time.Now()
	for _, r := range results {
		if r.Outcome != OutcomeActivated && r.Outcome != OutcomeAlreadyActive {
			continue
		}
		entry := recordEntry{
			Context:          r.Context,
			Scope:            r.Scope,
			RoleDefinitionID: r.RoleDefinitionID,
			Role:             r.Role,
			ScopeName:        r.ScopeName,
			ScopeType:        r.ScopeType,
			RequestID:        r.RequestID,
			Start:            now,
			StartSource:      startFromLocal,
			Status:           string(r.Outcome),
			WrittenAt:        now,
		}
		if r.Since != nil {
			entry.Start = *r.Since
			entry.StartSource = startFromARM
		}
		if r.Until != nil {
			entry.End = *r.Until
		}
		entry.Key = recordKey(entry.Context, entry.Scope, entry.RoleDefinitionID)
		byContext[r.Context] = append(byContext[r.Context], entry)
	}
	for context, fresh := range byContext {
		writeRecord(context, mergeEntries(readRecord(context), fresh))
	}
}

// forgetActivations records that this machine has given up a role.
//
// It writes a tombstone rather than deleting the entry. Deleting was not enough:
// ARM keeps listing a just-deactivated role as Activated for a minute or two, so
// the next reconciliation read it back in and `status` reported the role as
// still held — the same lag as an activation, in the other direction. The
// tombstone outranks that listing until it agrees, or the ceiling expires.
func forgetActivations(results []result) {
	byContext := map[string][]recordEntry{}
	now := time.Now()
	for _, r := range results {
		if r.Outcome != OutcomeDeactivated && r.Outcome != OutcomeNotActive {
			continue
		}
		entry := recordEntry{
			Context:          r.Context,
			Scope:            r.Scope,
			RoleDefinitionID: r.RoleDefinitionID,
			Role:             r.Role,
			ScopeName:        r.ScopeName,
			Status:           recordRevoked,
			WrittenAt:        now,
		}
		entry.Key = recordKey(entry.Context, entry.Scope, entry.RoleDefinitionID)
		byContext[r.Context] = append(byContext[r.Context], entry)
	}
	for context, gone := range byContext {
		writeRecord(context, mergeEntries(readRecord(context), gone))
	}
}

// reconcileRecord rewrites a context's record to match what ARM reports, which
// is the authority everywhere it has actually spoken.
//
// It has not spoken about a scope whose listing missed the soft deadline: that
// answer is unknown, not empty. And its listing is not the only thing it has to
// say about an activation — verdicts carries what each activation's own schedule
// request reports, which is current minutes before the listing is. An entry the
// listing has not caught up with is kept while its request says the role is
// provisioned, and dropped when the request says otherwise.
//
// Tombstones work the same way in reverse: one is kept while ARM still lists the
// role it denies, and dropped once the listing agrees it is gone.
func reconcileRecord(
	context string,
	active []activeRow,
	unconfirmedScopes []activationScope,
	verdicts map[string]entryVerdict,
) {
	unknown := unreadScopes(unconfirmedScopes)
	listed := map[string]activeRow{}
	for _, a := range active {
		if a.Context == context {
			listed[recordKey(a.Context, a.Assignment.Properties.Scope, a.Assignment.Properties.RoleDefinitionID)] = a
		}
	}
	previous := readRecord(context)
	kept, revoked := keepAgainstListing(previous, listed, unknown, verdicts)
	writeRecord(context, mergeEntries(kept, listedEntries(context, active, previous, revoked)))
}

// keepAgainstListing decides which existing entries survive a listing, and
// returns the tombstones among them so ARM's own rows can be filtered by them.
func keepAgainstListing(
	previous []recordEntry,
	listed map[string]activeRow,
	unknown map[string]bool,
	verdicts map[string]entryVerdict,
) (kept []recordEntry, revoked map[string]bool) {
	now := time.Now()
	revoked = map[string]bool{}
	kept = make([]recordEntry, 0, len(previous))
	for _, e := range previous {
		scopeUnread := scopeIsUnread(unknown, e.Context, e.Scope)
		row, isListed := listed[e.Key]
		switch {
		case e.Revoked():
			// Keep denying the role until ARM stops listing it — or while its
			// scope went unread, which proves nothing either way.
			if isListed && e.Denies(row.Assignment) || !isListed && scopeUnread {
				revoked[e.Key] = true
				kept = append(kept, e)
			}
		case isListed:
			// ARM's own row replaces it below; see listedEntries.
			continue
		case scopeUnread, e.Confirming(now) && verdicts[entryKey(e)] != verdictGone:
			kept = append(kept, e)
		}
	}
	return kept, revoked
}

// listedEntries renders ARM's activation rows as record entries, carrying over
// what the previous entry knew that a listing cannot say.
func listedEntries(context string, active []activeRow, previous []recordEntry, revoked map[string]bool) []recordEntry {
	prev := map[string]recordEntry{}
	for _, e := range previous {
		prev[e.Key] = e
	}
	now := time.Now()
	out := make([]recordEntry, 0, len(active))
	for _, a := range active {
		if a.Context != context {
			continue
		}
		entry := recordEntry{
			Context:          a.Context,
			Scope:            a.Assignment.Properties.Scope,
			RoleDefinitionID: a.Assignment.Properties.RoleDefinitionID,
			Role:             a.Assignment.RoleName(),
			ScopeName:        a.Assignment.ScopeName(),
			ScopeType:        a.Assignment.ScopeType(),
			Status:           string(OutcomeActivated),
			Listed:           true,
			WrittenAt:        now,
		}
		if s := a.Assignment.Properties.StartDateTime; s != nil {
			entry.Start = *s
			entry.StartSource = startFromARM
		}
		if e := a.Assignment.Properties.EndDateTime; e != nil {
			entry.End = *e
		}
		entry.Key = recordKey(entry.Context, entry.Scope, entry.RoleDefinitionID)
		if revoked[entry.Key] {
			continue
		}
		if p, ok := prev[entry.Key]; ok && !p.Revoked() {
			// Keep the original write time and request id: they are what the
			// ceiling and the request check are measured against. Re-stamping
			// WrittenAt made every confirmed row look freshly activated, so the
			// "just activated here" marker never cleared.
			entry.WrittenAt = p.WrittenAt
			entry.RequestID = p.RequestID
		}
		out = append(out, entry)
	}
	return out
}
