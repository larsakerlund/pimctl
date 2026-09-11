// The activation record as one run sees it: reading it into rows, and merging
// those rows with what Azure listed. It is what `status` prints before Azure
// answers and what survives a listing that does not mention a role — always as
// rows marked unconfirmed, never as the last word. The file and its lifetime
// rules are in record.go; writing it back is reconcile.go.

package cli

import (
	"time"

	"github.com/larsakerlund/pimctl/internal/armclient"
)

// localRecord is this machine's own view for one run: the rows it believes are
// held, and the roles it has just given up.
//
// The two travel together because both outrank ARM for the same reason — ARM's
// per-scope listing lags this machine's own writes by minutes in either
// direction — and using one without the other reintroduces half the
// bug: rendering the held rows but ignoring the tombstones shows a role you have
// already dropped as "activated elsewhere".
type localRecord struct {
	rows []activeRow // what the record believes is held, sorted as the tables sort.
	// revoked holds selection keys given up here that ARM may still be listing.
	revoked map[string]recordEntry
	// verdicts is what ARM's schedule requests said about the rows its listing
	// has not caught up with. Empty before the listing has been read at all.
	verdicts map[string]entryVerdict
}

// denies reports whether a deactivation record still contradicts this listing
// row. A later activation window supersedes the earlier deactivation.
func (lr localRecord) denies(r activeRow) bool {
	entry, ok := lr.revoked[activeSelectionKey(r)]
	return ok && entry.Denies(r.Assignment)
}

// stands reports whether a row from the record survives a listing that does not
// contain it.
//
// Three ways it can: the listing never covered its scope; its own schedule
// request says the role is provisioned; or ARM could not be asked at all and the
// entry is still inside the ceiling. Only a request that says otherwise, or a
// row the listing has confirmed before and now omits, is really gone.
func (lr localRecord) stands(r activeRow, scopeUnread bool) bool {
	if scopeUnread {
		return true
	}
	if r.State != RowConfirming {
		return false
	}
	return lr.verdicts[activeSelectionKey(r)] != verdictGone
}

// readLocalRecord renders this machine's record for every open session.
func readLocalRecord(rc *runContext) localRecord {
	lr := localRecord{revoked: map[string]recordEntry{}}
	now := time.Now()
	for _, s := range rc.Sessions {
		label := s.Token.Label()
		for _, e := range readRecord(s.Token.Label()) {
			row := recordRow(label, s, e)
			if e.Revoked() {
				lr.revoked[activeSelectionKey(row)] = e
				continue
			}
			if e.Confirming(now) {
				row.State = RowConfirming
			}
			lr.rows = append(lr.rows, row)
		}
	}
	sortActivations(lr.rows)
	return lr
}

// recordRow renders one record entry as an activation row.
//
// It is the single place a record entry becomes a row, so the selection key a
// verdict is filed under and the key the table is rendered from cannot drift
// apart — they did once, and every verdict silently missed its row.
func recordRow(label string, s *session, e recordEntry) activeRow {
	a := armclient.Assignment{}
	a.Properties.AssignmentType = "Activated"
	a.Properties.Scope = e.Scope
	a.Properties.RoleDefinitionID = e.RoleDefinitionID
	if !e.Start.IsZero() {
		start := e.Start
		a.Properties.StartDateTime = &start
	}
	if !e.End.IsZero() {
		end := e.End
		a.Properties.EndDateTime = &end
	}
	a.Properties.ExpandedProperties.RoleDefinition = armclient.Named{DisplayName: e.Role}
	a.Properties.ExpandedProperties.Scope = armclient.Named{
		DisplayName: e.ScopeName, Type: e.ScopeType, ID: e.Scope,
	}
	return activeRow{Context: label, Session: s, Assignment: a, State: RowUnconfirmed}
}

// entryKey is the selection key a record entry's row is filed under.
func entryKey(e recordEntry) string {
	return activeSelectionKey(recordRow(e.Context, nil, e))
}

// localActiveRows is the record's held rows alone, for callers that only render.
func localActiveRows(rc *runContext) []activeRow {
	return readLocalRecord(rc).rows
}

// mergeActive combines what Azure listed with what this machine knows that the
// listing cannot see yet.
//
// Rendering the raw listing drops a held role the moment its scope misses the
// deadline — observed as "No roles are currently activated." printed in the same
// breath as naming a scope unconfirmed. It also loses a role activated minutes
// ago, because the listing runs that far behind ARM's own writes, and it
// resurrects one deactivated just as recently for the same reason. So three
// things stand in: an unanswered scope keeps its last known state, an activation
// the listing has not caught up with is kept for as long as its own schedule
// request says it is provisioned, and a role given up here is kept out while the
// listing still reports it.
func mergeActive(local localRecord, active []activeRow, unconfirmed []activationScope) []activeRow {
	unknown := unreadScopes(unconfirmed)
	seen := map[string]bool{}
	out := make([]activeRow, 0, len(active))
	for _, r := range active {
		key := activeSelectionKey(r)
		if local.denies(r) {
			continue
		}
		seen[key] = true
		out = append(out, r)
	}
	for _, r := range local.rows {
		key := activeSelectionKey(r)
		if seen[key] {
			continue
		}
		scopeUnread := scopeIsUnread(unknown, r.Context, r.Assignment.Properties.Scope)
		if !local.stands(r, scopeUnread) {
			continue
		}
		out = append(out, r)
	}
	sortActivations(out)
	return out
}

// unreadScopes indexes the scope ids ARM did not answer for.
//
// Ids, not display labels: this set decides whether a recorded role survives a
// listing that omits it. Indexing it by anything a human would read means a
// renamed or relabelled scope stops matching, and every row at an unread scope
// silently disappears.
func unreadScopes(unconfirmed []activationScope) map[string]bool {
	out := make(map[string]bool, len(unconfirmed))
	for _, id := range unconfirmed {
		out[id.key()] = true
	}
	return out
}

// scopeIsUnread checks both an individual scope and a tenant-wide fallback.
// A failed fallback leaves every scope in that context unknown.
func scopeIsUnread(unknown map[string]bool, label, id string) bool {
	return unknown[(activationScope{Context: label, ID: id}).key()] || unknown[(activationScope{Context: label}).key()]
}
