// [Row] — one eligible role, at one scope, in one context, with the activation
// covering it if there is one — and the operations over a slice of them:
// identity, matching activations on, deduping, sorting and the preset
// round-trip. That identity is what carries a selection from `ls` through `up`
// and `down` and back out as JSON. Acting on a selected row is activate.go.

package cli

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/larsakerlund/pimctl/internal/armclient"
	"github.com/larsakerlund/pimctl/internal/config"
)

// row pairs one eligible role with the activation covering it, if any.
type row struct {
	// Context is the cloudctx context the eligibility was read from.
	Context string
	// Elig is the eligibility itself, and the source of every name and id the
	// row renders.
	Elig armclient.Eligibility
	// EligibilityScope retains the granting scope when Elig.Scope is a narrower
	// activation target. Empty means the target and granting scope are equal.
	EligibilityScope string
	// Active is the live activation for this scope+role, or nil.
	Active      *armclient.Assignment
	ActiveState rowState // confidence in the activation state, including an absent activation.
	// AlsoVia lists the member types of duplicate eligibilities folded into
	// this row by DedupeRows, e.g. the group grant behind a direct one.
	AlsoVia []string
}

// sourceScope returns the scope whose eligibility and policy authorize a row.
func (r row) sourceScope() string {
	if r.EligibilityScope != "" {
		return r.EligibilityScope
	}
	return r.Elig.Properties.Scope
}

// activationRoleID qualifies the definition for the target without changing
// the source eligibility or its linked schedule identifier.
func (r row) activationRoleID() string {
	return armclient.QualifyRoleDefinitionID(
		r.Elig.Properties.Scope,
		r.sourceScope(),
		r.Elig.Properties.RoleDefinitionID,
	)
}

// Key is the (context, scope, role GUID) identity used for matching rows to
// activations and to preset entries.
func (r row) Key() string {
	return rowKey(r.Context, r.Elig.Properties.Scope, r.Elig.RoleDefinitionGUID())
}

// rowKey is the matching identity behind [row.Key] and [SelectionKeyFor]:
// context, scope and role definition GUID. Context names remain case-sensitive;
// only the Azure scope and role GUID are folded because ARM does not promise
// their case.
func rowKey(context, scope, roleGUID string) string {
	return context + "|" + strings.ToLower(scope) + "|" + strings.ToLower(roleGUID)
}

// SelectionKey is a short, stable identifier for one eligible role, safe to put
// in a script or hand to an agent. Unlike the printed row number it does not
// change when the tenant's role list does.
func (r row) SelectionKey() string {
	return selectionKeyFor(r.Context, r.Elig.Properties.Scope, r.Elig.RoleDefinitionGUID())
}

// selectionKeyFor derives the key one role has everywhere it appears — `ls`,
// `status`, and the result of an `up` or `down` — so a script can carry a row
// from one command to the next without matching on names.
//
// It is a truncated SHA-256 of the (context, scope, role) identity and of
// nothing else, which is what makes it stable: the key moves only when the role
// or the scope does, never because the tenant grew a role and shifted every
// printed row number by one. That is why --key takes this and not the index.
func selectionKeyFor(context, scope, roleGUID string) string {
	sum := sha256.Sum256([]byte(rowKey(context, scope, roleGUID)))
	return hex.EncodeToString(sum[:])[:selectionKeyLen]
}

// selectionKeyLen is short enough for a table column and long enough that a
// prefix is unambiguous across a few hundred roles.
const selectionKeyLen = 8

// activeSelectionKey is SelectionKey for an activation, so one key works across
// `ls`, `status`, `up` and `down`.
func activeSelectionKey(r activeRow) string {
	return selectionKeyFor(r.Context, r.Assignment.Properties.Scope, r.Assignment.RoleDefinitionGUID())
}

// IsActive reports whether this eligible role is currently activated.
func (r row) IsActive() bool { return r.Active != nil }

// ActiveUntil is the activation's end time, or nil.
func (r row) ActiveUntil() *time.Time {
	if r.Active == nil {
		return nil
	}
	return r.Active.Properties.EndDateTime
}

// matchActivations attaches the live activation (if any) to each eligible row.
func matchActivations(rows []row, active []activeRow) []row {
	byKey := map[string]*activeRow{}
	for i := range active {
		a := &active[i].Assignment
		if !a.IsActivated() {
			continue
		}
		byKey[rowKey(active[i].Context, a.Properties.Scope, a.RoleDefinitionGUID())] = &active[i]
	}
	for i := range rows {
		k := rows[i].Key()
		rows[i].Active = nil
		if a, ok := byKey[k]; ok {
			rows[i].Active = &a.Assignment
			rows[i].ActiveState = a.State
		}
	}
	return rows
}

// dedupeRows folds genuine duplicate eligibilities into one row.
//
// The same role at the same scope can be granted twice — once directly and once
// through a group — and ARM returns both instances. Activating both fired two
// concurrent PUTs for the same thing, one of which then failed as
// RoleAssignmentExists. One row per (context, scope, role), preferring a Direct
// membership since that is the one whose eligibility schedule is least likely
// to disappear, and recording the other memberships for display.
func dedupeRows(rows []row) []row {
	byKey := map[string]int{}
	out := make([]row, 0, len(rows))
	for _, r := range rows {
		k := r.Key()
		idx, seen := byKey[k]
		if !seen {
			byKey[k] = len(out)
			out = append(out, r)
			continue
		}
		kept := &out[idx]
		isDirect := func(x row) bool { return strings.EqualFold(x.Elig.Properties.MemberType, "Direct") }
		if !isDirect(*kept) && isDirect(r) {
			// The incoming row takes over; the one it displaces becomes the
			// "also via" note, along with anything already folded into it.
			r.AlsoVia = append(append([]string{}, kept.AlsoVia...), kept.Elig.Properties.MemberType)
			*kept = r
			continue
		}
		kept.AlsoVia = append(kept.AlsoVia, r.Elig.Properties.MemberType)
	}
	return out
}

// sortRows orders rows by context, then role, then scope name — a stable order
// so the printed index numbers mean the same thing between runs.
func sortRows(rows []row) {
	slices.SortStableFunc(rows, func(a, b row) int {
		return cmp.Or(
			cmp.Compare(a.Context, b.Context),
			cmp.Compare(a.Elig.RoleName(), b.Elig.RoleName()),
			cmp.Compare(a.Elig.ScopeName(), b.Elig.ScopeName()),
			cmp.Compare(a.Elig.Properties.Scope, b.Elig.Properties.Scope),
		)
	})
}

// multipleContexts reports whether the rows span more than one context, which
// is what decides if the CONTEXT column earns its width.
func multipleContexts(rows []row) bool {
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.Context] = true
		if len(seen) > 1 {
			return true
		}
	}
	return false
}

// multipleActiveContexts is multipleContexts for activations.
func multipleActiveContexts(rows []activeRow) bool {
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.Context] = true
		if len(seen) > 1 {
			return true
		}
	}
	return false
}

// applyPreset selects the rows a preset names. Entries that are no longer
// eligible are returned as a list of human-readable descriptions rather than
// treated as an error.
func applyPreset(rows []row, entries []config.PresetEntry) (selected []row, missing []string) {
	byKey := map[string]row{}
	for _, r := range rows {
		byKey[r.Key()] = r
	}
	for _, e := range entries {
		k := rowKey(contextLabel(e.Context), e.Scope, armclient.RoleDefinitionGUID(e.RoleDefinitionID))
		if r, ok := byKey[k]; ok {
			selected = append(selected, r)
			continue
		}
		name := e.RoleName
		if name == "" {
			name = armclient.RoleDefinitionGUID(e.RoleDefinitionID)
		}
		scope := e.ScopeName
		if scope == "" {
			scope = e.Scope
		}
		missing = append(missing, fmt.Sprintf("%s @ %s [%s]", name, scope, e.Context))
	}
	return selected, missing
}

// toPresetEntries converts a selection into storable preset entries.
func toPresetEntries(rows []row) []config.PresetEntry {
	out := make([]config.PresetEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, config.PresetEntry{
			EligibilityScope: r.EligibilityScope,
			Context:          contextName(r.Context),
			Scope:            r.Elig.Properties.Scope,
			RoleDefinitionID: r.activationRoleID(),
			RoleName:         r.Elig.RoleName(),
			ScopeName:        r.Elig.ScopeName(),
		})
	}
	return out
}
