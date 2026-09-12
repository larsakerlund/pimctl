// Covers resources.go: the subscription and child listings the init scope
// browser reads — their paths, the api-versions they are pinned to, nextLink
// paging, and that an ARM error reaches the caller as an error rather than as
// an empty list. The browser that consumes them is tested in internal/cli.

package armclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// resourcesFake serves subscriptions in two pages and one flat listing for
// everything else, recording every path and api-version it was asked for.
type resourcesFake struct {
	srv      *httptest.Server
	paths    []string // every request's path, in order.
	versions []string // the api-version each request carried, in order.
	pages    atomic.Int32
}

// newResourcesFake starts the fake and wires a client to it.
func newResourcesFake(t *testing.T) (*resourcesFake, *Client) {
	t.Helper()
	f := &resourcesFake{}
	var srvURL string
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.paths = append(f.paths, r.URL.Path)
		f.versions = append(f.versions, r.URL.Query().Get("api-version"))
		switch {
		case r.URL.Path == "/subscriptions" && r.URL.Query().Get("$skiptoken") == "":
			f.pages.Add(1)
			fmt.Fprintf(w, `{"value":[{"id":"/subscriptions/s1","subscriptionId":"s1","displayName":"Dev"}],`+
				`"nextLink":%q}`, srvURL+"/subscriptions?api-version=2022-12-01&$skiptoken=page2")
		case r.URL.Path == "/subscriptions":
			f.pages.Add(1)
			fmt.Fprint(w, `{"value":[{"id":"/subscriptions/s2","subscriptionId":"s2","displayName":"Prod"}]}`)
		case strings.HasSuffix(strings.ToLower(r.URL.Path), "/resourcegroups"):
			fmt.Fprint(w, `{"value":[{"id":"/subscriptions/s1/resourceGroups/rg1","name":"rg1"}]}`)
		case strings.HasSuffix(strings.ToLower(r.URL.Path), "/resources"):
			fmt.Fprint(w, `{"value":[{"id":"/subscriptions/s1/resourceGroups/rg1/providers/Microsoft.Storage/`+
				`storageAccounts/sa1","name":"sa1","type":"Microsoft.Storage/storageAccounts"}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":{"code":"ResourceNotFound","message":"nothing here"}}`)
		}
	}))
	t.Cleanup(f.srv.Close)
	srvURL = f.srv.URL
	return f, New(f.srv.URL, "fake-token-not-a-real-credential", f.srv.Client())
}

func TestListSubscriptionsFollowsNextLinkUnderItsOwnAPIVersion(t *testing.T) {
	f, c := newResourcesFake(t)
	got, err := c.ListSubscriptions(context.Background())
	if err != nil {
		t.Fatalf("ListSubscriptions: %v", err)
	}
	if f.pages.Load() != 2 {
		t.Errorf("fetched %d page(s), want 2 (nextLink not followed?)", f.pages.Load())
	}
	if len(got) != 2 || got[0].Label() != "Dev" || got[1].Label() != "Prod" {
		t.Fatalf("got %+v, want Dev then Prod across the two pages", got)
	}
	if got[1].ID != "/subscriptions/s2" {
		t.Errorf("second page id = %q", got[1].ID)
	}
	// The Subscriptions resource provider has its own api-version; the PIM
	// one is not accepted there. Asserted literally, against the wire.
	for i, v := range f.versions {
		if v != "2022-12-01" {
			t.Errorf("request %d (%s): api-version = %q, want 2022-12-01", i, f.paths[i], v)
		}
	}
}

func TestListScopeChildrenPicksTheCollectionForTheScope(t *testing.T) {
	for _, tc := range []struct {
		scope, wantPath, wantLabel string
	}{
		{"/subscriptions/s1", "/subscriptions/s1/resourcegroups", "rg1"},
		{"/subscriptions/s1/resourceGroups/rg1", "/subscriptions/s1/resourceGroups/rg1/resources", "sa1"},
	} {
		t.Run(tc.wantLabel, func(t *testing.T) {
			f, c := newResourcesFake(t)
			got, err := c.ListScopeChildren(context.Background(), tc.scope)
			if err != nil {
				t.Fatalf("ListScopeChildren(%s): %v", tc.scope, err)
			}
			if len(f.paths) != 1 || f.paths[0] != tc.wantPath {
				t.Errorf("asked %v, want exactly %s", f.paths, tc.wantPath)
			}
			// The Resources provider's api-version, which is neither PIM's
			// nor the Subscriptions provider's. Asserted literally.
			if f.versions[0] != "2021-04-01" {
				t.Errorf("api-version = %q, want 2021-04-01", f.versions[0])
			}
			if len(got) != 1 || got[0].Label() != tc.wantLabel {
				t.Errorf("got %+v, want one child labelled %s", got, tc.wantLabel)
			}
		})
	}
}

func TestListScopeChildrenSurfacesAnARMError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":{"code":"AuthorizationFailed","message":"no read at this scope"}}`)
	})
	got, err := c.ListScopeChildren(context.Background(), "/subscriptions/s1")
	if err == nil {
		t.Fatalf("a 403 must be an error, not %d children", len(got))
	}
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code != "AuthorizationFailed" {
		t.Errorf("error = %v, want ARM's AuthorizationFailed", err)
	}
	if got != nil {
		t.Errorf("an error must not come with a list: %+v", got)
	}
}

func TestResourceLabelPrefersDisplayNameThenName(t *testing.T) {
	for _, tc := range []struct {
		r    Resource
		want string
	}{
		{Resource{ID: "/subscriptions/s1", Name: "s1", DisplayName: "Dev"}, "Dev"},
		{Resource{ID: "/subscriptions/s1/resourceGroups/rg1", Name: "rg1"}, "rg1"},
		{Resource{ID: "/subscriptions/s1"}, "/subscriptions/s1"},
	} {
		if got := tc.r.Label(); got != tc.want {
			t.Errorf("Label(%+v) = %q, want %q", tc.r, got, tc.want)
		}
	}
}
