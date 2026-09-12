// Covers the pure half of init_scope.go: initChildren reading subscriptions,
// resource groups and resources through the fake ARM, following a nextLink and
// surfacing an ARM error. navigateInitScope and browseGrantScope draw the
// single-choice picker, which needs a terminal; scripts/tui-scope-test.exp
// drives that half through a pty.

package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/larsakerlund/pimctl/internal/armclient"
	"github.com/larsakerlund/pimctl/internal/azauth"
)

// initScopeFixture is a fake ARM with two subscriptions served across two
// pages, one resource group under the first and one resource under that.
func initScopeFixture(t *testing.T) (*fakeARM, *runContext, *session) {
	t.Helper()
	f := &fakeARM{
		t: t,
		resources: map[string][]armclient.Resource{
			"/subscriptions": {
				{ID: "/subscriptions/s1", Name: "s1", DisplayName: "Dev"},
				{ID: "/subscriptions/s2", Name: "s2", DisplayName: "Prod"},
			},
			"/subscriptions/s1/resourcegroups": {
				{ID: "/subscriptions/s1/resourceGroups/rg1", Name: "rg1"},
			},
			"/subscriptions/s1/resourcegroups/rg1/resources": {
				{
					ID:   "/subscriptions/s1/resourceGroups/rg1/providers/Microsoft.Storage/storageAccounts/sa1",
					Name: "sa1",
					Type: "Microsoft.Storage/storageAccounts",
				},
			},
		},
		pagedResources: "/subscriptions",
	}
	f.install()
	tok := &azauth.Token{Context: "contoso", AccessToken: "fake", PrincipalID: "oid-1", TenantID: "tid-1"}
	s := &session{Context: "contoso", Token: tok, Client: armclient.New(f.srv.URL, tok.AccessToken, f.srv.Client())}
	rc := &runContext{Ctx: context.Background(), Timeouts: defaultTimeouts(), Timings: newTimings(false)}
	return f, rc, s
}

// labelsOf renders the browser's choices the way navigateInitScope labels
// them, so a test reads like the list a user would see.
func labelsOf(resources []armclient.Resource) []string {
	out := make([]string, 0, len(resources))
	for _, r := range resources {
		out = append(out, r.Label()+" · "+r.ID)
	}
	return out
}

func TestInitChildrenWalksSubscriptionsGroupsAndResources(t *testing.T) {
	f, rc, s := initScopeFixture(t)

	subs, err := initChildren(rc, s, "")
	if err != nil {
		t.Fatalf("initChildren at the root: %v", err)
	}
	want := []string{"Dev · /subscriptions/s1", "Prod · /subscriptions/s2"}
	if got := labelsOf(subs); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("subscriptions = %v, want %v", got, want)
	}
	if n := f.secondPageCount(); n != 1 {
		t.Errorf("the second subscription page was fetched %d time(s), want 1", n)
	}
	// A subscription is browsed, never activated, because the browser only
	// stops at a Resource.
	if armclient.NormalizeScopeType("", subs[0].ID) == "Resource" {
		t.Errorf("a subscription must not read as a resource: %s", subs[0].ID)
	}

	groups, err := initChildren(rc, s, subs[0].ID)
	if err != nil {
		t.Fatalf("initChildren under %s: %v", subs[0].ID, err)
	}
	if got := labelsOf(groups); len(got) != 1 || got[0] != "rg1 · /subscriptions/s1/resourceGroups/rg1" {
		t.Errorf("resource groups = %v", got)
	}

	resources, err := initChildren(rc, s, groups[0].ID)
	if err != nil {
		t.Fatalf("initChildren under %s: %v", groups[0].ID, err)
	}
	if len(resources) != 1 || resources[0].Label() != "sa1" {
		t.Fatalf("resources = %+v", resources)
	}
	// A resource is a leaf: choosing it ends the walk rather than browsing on.
	if armclient.NormalizeScopeType("", resources[0].ID) != "Resource" {
		t.Errorf("a resource must read as a leaf: %s", resources[0].ID)
	}
}

func TestInitChildrenSurfacesAnARMErrorRatherThanAnEmptyList(t *testing.T) {
	_, rc, s := initScopeFixture(t)
	got, err := initChildren(rc, s, "/subscriptions/unknown")
	if err == nil {
		t.Fatalf("a scope ARM cannot list must be an error, not %d children", len(got))
	}
	var ae *armclient.APIError
	if !errors.As(err, &ae) || ae.Code != "ResourceNotFound" {
		t.Errorf("error = %v, want ARM's own code so the browser can print it", err)
	}
	if got != nil {
		t.Errorf("an error must not come with a list the browser could show as empty: %+v", got)
	}
}
