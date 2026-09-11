// Scoped eligibility tests pin ARM ancestry and ownership filtering using a
// local server. They never infer parentage from management-group names.

package armclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestScopedEligibilityIntersectsAncestryAndMembership(t *testing.T) {
	grant := func(schedule, scope string) Eligibility {
		e := Eligibility{}
		e.Properties.Scope = scope
		e.Properties.RoleDefinitionID = scope + "/providers/Microsoft.Authorization/roleDefinitions/role"
		e.Properties.RoleEligibilityScheduleID = schedule
		return e
	}
	mine := grant("my-group-schedule", "/providers/Microsoft.Management/managementGroups/platform")
	other := grant("someone-elses-schedule", mine.Properties.Scope)
	child := grant("my-descendant-schedule", "/subscriptions/sub/resourceGroups/child")
	var filters []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(
			r.URL.Path,
			"/subscriptions/sub/providers/Microsoft.Authorization/roleEligibilityScheduleInstances",
		) {
			t.Errorf("wrong target: %s", r.URL.Path)
		}
		if r.URL.Query().Get("api-version") != APIVersion {
			t.Errorf("wrong version: %s", r.URL.RawQuery)
		}
		filter := r.URL.Query().Get("$filter")
		filters = append(filters, filter)
		rows := []Eligibility{mine, other}
		if filter == "assignedTo('user-oid')" {
			rows = []Eligibility{mine, child}
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"value": rows}); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()
	got, err := New(
		srv.URL,
		"fake",
		srv.Client(),
	).ListEligibilitiesAtScope(context.Background(), "/subscriptions/sub", "user-oid")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Properties.RoleEligibilityScheduleID != "my-group-schedule" {
		t.Fatalf("wrong intersection: %#v", got)
	}
	if len(filters) != 2 || filters[0] != "atScope()" || filters[1] != "assignedTo('user-oid')" {
		t.Fatalf("filters: %v", filters)
	}
}

func TestScopedEligibilityLookupFailureReturnsNoPartialAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Query().Get("$filter"), "assignedTo") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if err := json.NewEncoder(w).
			Encode(map[string]any{"value": []Eligibility{{Properties: EligibilityProperties{RoleEligibilityScheduleID: "source"}}}}); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()
	got, err := New(
		srv.URL,
		"fake",
		srv.Client(),
	).ListEligibilitiesAtScope(context.Background(), "/subscriptions/sub", "user-oid")
	if err == nil || got != nil {
		t.Fatalf("partial answer: %#v, %v", got, err)
	}
}
