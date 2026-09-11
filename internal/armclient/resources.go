// Resource discovery supplies readable choices for project setup. These ARM
// reads do not provision resources or grant access; unavailable discovery must
// be reported instead of presenting an empty list as complete.

package armclient

import (
	"context"
	"strings"
)

// Resource describes the identifiers and labels used by the setup browser.
type Resource struct {
	ID          string `json:"id"`          // Full ARM identifier.
	Name        string `json:"name"`        // Resource or resource-group name.
	DisplayName string `json:"displayName"` // Subscription display name.
	Type        string `json:"type"`        // Provider resource type when present.
}

// Label returns the subscription display name or the ordinary resource name.
func (r Resource) Label() string {
	if r.DisplayName != "" {
		return r.DisplayName
	}
	if r.Name != "" {
		return r.Name
	}
	return r.ID
}

// ListSubscriptions reads the tenant's visible subscriptions with pagination.
// It returns ARM or decoding errors, never a partial list on failure.
func (c *Client) ListSubscriptions(ctx context.Context) ([]Resource, error) {
	return listAll[Resource](ctx, c, c.Host+"/subscriptions?api-version=2022-12-01")
}

// ListScopeChildren reads resource groups beneath a subscription, or resources
// beneath a resource group. scope is a validated subscription or group ID.
// It returns ARM or decoding errors without inventing unavailable children.
func (c *Client) ListScopeChildren(ctx context.Context, scope string) ([]Resource, error) {
	collection := "/resourcegroups"
	if strings.Contains(strings.ToLower(scope), "/resourcegroups/") {
		collection = "/resources"
	}
	return listAll[Resource](ctx, c, c.Host+scope+collection+"?api-version=2021-04-01")
}
