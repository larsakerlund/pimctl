// Scope-aware eligibility reads establish both ancestry and the caller's
// membership using ARM filters. They do not activate roles or infer ancestry
// from management-group names.

package armclient

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// ListEligibilitiesAtScope returns the caller's eligible schedules at or
// above scope. ARM's atScope() proves ancestry; assignedTo() proves user or
// group membership. Intersecting them avoids treating descendants or another
// principal's schedule as usable. It makes two paginated GETs and returns any
// HTTP or decoding error without a partial answer.
func (c *Client) ListEligibilitiesAtScope(ctx context.Context, scope, principalID string) ([]Eligibility, error) {
	at, err := c.scopedEligibilities(ctx, scope, "atScope()")
	if err != nil {
		return nil, err
	}
	mine, err := c.scopedEligibilities(ctx, scope, "assignedTo('"+principalID+"')")
	if err != nil {
		return nil, err
	}
	owned := make(map[string]bool, len(mine))
	for _, e := range mine {
		owned[eligibilityIdentity(e)] = true
	}
	out := make([]Eligibility, 0, len(at))
	for _, e := range at {
		if e.Properties.RoleEligibilityScheduleID != "" && owned[eligibilityIdentity(e)] {
			out = append(out, e)
		}
	}
	return out, nil
}

// eligibilityIdentity compares schedule and role identities case-insensitively;
// a display name or membership type is never sufficient proof of membership.
func eligibilityIdentity(e Eligibility) string {
	return strings.ToLower(e.Properties.RoleEligibilityScheduleID + "|" + e.RoleDefinitionGUID())
}

// scopedEligibilities reads one documented eligibility filter at an ARM scope.
// The caller supplies an internally assembled filter, never arbitrary query text.
func (c *Client) scopedEligibilities(ctx context.Context, scope, filter string) ([]Eligibility, error) {
	u := fmt.Sprintf(
		"%s%s/providers/Microsoft.Authorization/roleEligibilityScheduleInstances?api-version=%s&$filter=%s",
		c.Host,
		strings.TrimSuffix(scope, "/"),
		APIVersion,
		url.QueryEscape(filter),
	)
	return listAll[Eligibility](ctx, c, u)
}
