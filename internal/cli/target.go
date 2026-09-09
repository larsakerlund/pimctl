// [Target], one role to give up, and the three ways one can be named: from the
// activation listing, from the eligibility listing, or from a preset. Where a
// target came from is what decides how ARM's answer is read, which is why the
// three constructors live together. Choosing which targets to act on is
// deactivate.go; sending one to ARM is request.go.

package cli

import (
	"time"

	"github.com/larsakerlund/pimctl/internal/armclient"
	"github.com/larsakerlund/pimctl/internal/config"
)

// target is one role pimctl will try to deactivate.
//
// SeenActive records whether the activation listing actually showed it. That
// distinction decides how RoleAssignmentDoesNotExist is read: for a role we
// just saw active it means the assignment has not finished propagating and is
// still held (a failure); for a role we are attempting speculatively it means
// what it says (not active — nothing to do).
type target struct {
	Session          *session   // the context's client; attached before the request is sent.
	Context          string     // which cloudctx context the role is held in.
	Scope            string     // the full ARM scope id to send the request at.
	RoleDefinitionID string     // the role, which the request re-qualifies to Scope.
	RoleName         string     // display name, for the plan and the result.
	ScopeName        string     // scope display name, likewise.
	ScopeType        string     // Subscription, ManagementGroup, ResourceGroup or Resource.
	EndDateTime      *time.Time // when it would have expired anyway; nil when the listing did not show it.
	// SeenActive records that the activation listing showed this role as held.
	// It decides what RoleAssignmentDoesNotExist means: for a role just seen
	// active it is propagation, and a failure; for a speculative one it is the
	// requested end state.
	SeenActive bool
}

// targetFromActive is a target for a role the activation listing has just
// shown, so it carries that row's session and window and sets SeenActive.
func targetFromActive(r activeRow) target {
	return target{
		Session:          r.Session,
		Context:          r.Context,
		Scope:            r.Assignment.Properties.Scope,
		RoleDefinitionID: r.Assignment.Properties.RoleDefinitionID,
		RoleName:         r.Assignment.RoleName(),
		ScopeName:        r.Assignment.ScopeName(),
		ScopeType:        r.Assignment.ScopeType(),
		EndDateTime:      r.Assignment.Properties.EndDateTime,
		SeenActive:       true,
	}
}

// targetFromEligible is a target for a role named through the eligibility
// listing, which says nothing about whether it is activated. SeenActive stays
// false, so RoleAssignmentDoesNotExist reads as "not active" rather than as a
// failure to give up a role that is held.
func targetFromEligible(r row) target {
	return target{
		Context:          r.Context,
		Scope:            r.Elig.Properties.Scope,
		RoleDefinitionID: r.Elig.Properties.RoleDefinitionID,
		RoleName:         r.Elig.RoleName(),
		ScopeName:        r.Elig.ScopeName(),
		ScopeType:        r.Elig.ScopeType(),
	}
}

// targetFromPreset is a target for a role named by a preset. A preset stores
// scope and role definition id, which is everything a deactivation needs, so
// this path reads no listing at all; the scope type it does not store is
// derived from the scope id.
func targetFromPreset(e config.PresetEntry) target {
	return target{
		Context:          e.Context,
		Scope:            e.Scope,
		RoleDefinitionID: e.RoleDefinitionID,
		RoleName:         e.RoleName,
		ScopeName:        e.ScopeName,
		ScopeType:        armclient.NormalizeScopeType("", e.Scope),
	}
}

// key identifies the role a target names, so two targets built from different
// sources are recognised as the same role; see MergeTargets.
func (t target) key() string {
	return rowKey(t.Context, t.Scope, armclient.RoleDefinitionGUID(t.RoleDefinitionID))
}

// mergeTargets folds a speculative selection together with what the listing
// showed, keeping the richer active entry when both describe the same role.
func mergeTargets(selected, active []target) []target {
	seen := map[string]int{}
	out := make([]target, 0, len(selected))
	for _, t := range selected {
		if i, ok := seen[t.key()]; ok {
			if t.SeenActive && !out[i].SeenActive {
				out[i] = t
			}
			continue
		}
		seen[t.key()] = len(out)
		out = append(out, t)
	}
	for _, a := range active {
		if i, ok := seen[a.key()]; ok && !out[i].SeenActive {
			out[i] = a
		}
	}
	return out
}
