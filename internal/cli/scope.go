// Turning an ARM scope id into something an operator can safely act on: the
// shared labeler, its leaf-id fallback, and the table cells built from it.
// This file only renders scopes; which ones exist is read in eligible.go.

package cli

import (
	"strings"

	"github.com/larsakerlund/pimctl/internal/armclient"
)

// Naming a scope for a human. Azure lets several management groups share one
// display name — this tenant has three called "Contoso landing zones" — so a bare
// name can name the wrong place to elevate on. ScopeLabeler is the single answer
// to "what do I call this scope", built once per command from the widest set of
// scopes it knows about so the same scope reads the same way in every table.

// scopeRef is a scope's display name paired with its ARM id.
type scopeRef struct {
	Name string // the display name, which several scopes may share.
	ID   string // the full ARM id, which is unique.
}

// scopeLabeler renders a scope for display, appending the scope's leaf id to
// any display name shared by more than one scope.
//
// It is built once from the widest set of scopes a command knows about — every
// eligible role, not just the ones on screen — so the same scope reads the same
// way in the confirmation table, the results table and `status`. Deriving it
// per-table instead made a single-scope selection print a bare "Azure landing
// zones" with no way to tell which of the tenant's three it meant.
type scopeLabeler struct {
	// ambiguous holds the display names that cover more than one scope id, and
	// so have to be qualified with the scope's leaf when printed.
	ambiguous map[string]bool
}

// newScopeLabeler records which display names cover more than one scope.
func newScopeLabeler(refs []scopeRef) scopeLabeler {
	ids := map[string]map[string]bool{}
	for _, r := range refs {
		if ids[r.Name] == nil {
			ids[r.Name] = map[string]bool{}
		}
		ids[r.Name][r.ID] = true
	}
	amb := map[string]bool{}
	for name, set := range ids {
		if len(set) > 1 {
			amb[name] = true
		}
	}
	return scopeLabeler{ambiguous: amb}
}

// Label renders one scope.
//
// A management group is always shown with its own name appended, whether or not
// the rows on screen happen to collide. Management-group display names are set
// by whoever built the hierarchy and are routinely reused — this tenant has
// three called "Contoso landing zones" — so a bare display name is never enough
// to tell an operator which one they are about to elevate on, or which one they
// are giving up. Subscriptions keep the conditional treatment: their names are
// usually distinct, and the id adds noise when it is not needed.
func (l scopeLabeler) Label(name, id string) string {
	if id == "" {
		return name
	}
	if name == "" {
		// A message can know the scope id without having read a display name
		// for it. The leaf is still the right thing to print — bare, since
		// there is no name to qualify.
		return scopeLeaf(id)
	}
	if isManagementGroupScope(id) || l.ambiguous[name] {
		leaf := scopeLeaf(id)
		if leaf == "" || leaf == name {
			return name
		}
		return name + " (" + leaf + ")"
	}
	return name
}

// scopeLabelerForRows builds a labeler from every eligible role.
func scopeLabelerForRows(rows []row) scopeLabeler {
	refs := make([]scopeRef, 0, len(rows))
	for _, r := range rows {
		refs = append(refs, scopeRef{Name: r.Elig.ScopeName(), ID: r.Elig.Properties.Scope})
	}
	return newScopeLabeler(refs)
}

// scopeLabelerForActive builds a labeler from a set of activations.
func scopeLabelerForActive(rows []activeRow) scopeLabeler {
	refs := make([]scopeRef, 0, len(rows))
	for _, r := range rows {
		refs = append(refs, scopeRef{Name: r.Assignment.ScopeName(), ID: r.Assignment.Properties.Scope})
	}
	return newScopeLabeler(refs)
}

// isManagementGroupScope reports whether an ARM scope id names a management
// group.
func isManagementGroupScope(id string) bool {
	return strings.Contains(strings.ToLower(id), "/providers/microsoft.management/managementgroups/")
}

// scopeLeaf is the last path segment of an ARM scope id, shortened when it is a
// bare GUID (a subscription id) so it still fits a table column.
func scopeLeaf(id string) string {
	trimmed := strings.TrimSuffix(id, "/")
	leaf := trimmed
	if i := strings.LastIndex(trimmed, "/"); i >= 0 {
		leaf = trimmed[i+1:]
	}
	// A subscription id is a GUID, and its first block is plenty to tell two
	// scopes apart in a table. A management group's name is not a GUID — it is
	// the only human-readable thing identifying it — so abbreviating it turned
	// "contoso-throttled" into "contoso-" and made two hierarchies collide.
	if looksLikeGUID(leaf) {
		const firstBlock = 8 // a GUID's first dash-separated block.
		return leaf[:firstBlock]
	}
	return leaf
}

// looksLikeGUID reports whether s has the shape of a GUID.
//
// Shape, not validity: this only decides whether a scope leaf is a machine id
// safe to abbreviate or a name a human chose, and a near-GUID that is neither
// is not worth a parser.
func looksLikeGUID(s string) bool {
	const shape = "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"
	if len(s) != len(shape) {
		return false
	}
	for i, r := range s {
		if shape[i] == '-' {
			if r != '-' {
				return false
			}
			continue
		}
		isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
		if !isHex {
			return false
		}
	}
	return true
}

// truncatedScopeCells renders the scope column, disambiguating *after*
// truncation.
//
// The labeler only knows whether two scopes share a display name. Truncation
// can create a collision it never saw: "DEV - AGENTIC AI - AZ - CONTOSO" and
// "DEV - AGENTIC ALERTS - AZ - CONTOSO" are distinct names that cut to the same
// string, so the table showed two different subscriptions as identical rows.
func truncatedScopeCells(rows []row, scopes scopeLabeler, width int) []string {
	cells := make([]string, len(rows))
	for i, r := range rows {
		cells[i] = armclient.TruncateMiddle(scopes.Label(r.Elig.ScopeName(), r.Elig.Properties.Scope), width)
	}
	// Which rendered strings cover more than one real scope?
	ids := map[string]map[string]bool{}
	for i, r := range rows {
		if ids[cells[i]] == nil {
			ids[cells[i]] = map[string]bool{}
		}
		ids[cells[i]][r.Elig.Properties.Scope] = true
	}
	for i, r := range rows {
		// One rendered string, one real scope: nothing to disambiguate.
		const collision = 2
		if len(ids[cells[i]]) < collision {
			continue
		}
		leaf := scopeLeaf(r.Elig.Properties.Scope)
		// The leaf is appended after a " · " separator; if what is left of the
		// name would be shorter than minNameStub, show the leaf alone.
		const sepWidth, minNameStub = 3, 4
		keep := width - len(leaf) - sepWidth
		if keep < minNameStub {
			cells[i] = leaf
			continue
		}
		cells[i] = armclient.TruncateMiddle(
			scopes.Label(r.Elig.ScopeName(), r.Elig.Properties.Scope),
			keep,
		) + " (" + leaf + ")"
	}
	return cells
}
