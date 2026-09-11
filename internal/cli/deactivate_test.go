// Tests for deactivate.go and target.go, driven through the cobra commands so
// the flag wiring is under test too. Several of them exist because ARM's
// activation listing has been seen to omit roles that are genuinely held: a
// named deactivation must not trust it, and a bare one must say so. The fake
// ARM they run against is in fake_test.go.

package cli

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/larsakerlund/pimctl/internal/armclient"
	"github.com/larsakerlund/pimctl/internal/azauth"
)

func TestDownFailsWhenScopeDiscoveryIsIncomplete(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(strconv.FormatBool(partial), func(t *testing.T) {
			f := &fakeARM{t: t, eligibilities: twoLowImpactRoles(), putStatus: "Revoked"}
			f.install()
			shortDeadlines(t, 10*time.Millisecond)
			base := f.srv
			slowScope := f.eligibilities[1].Properties.Scope
			active := mkActivated(
				"Cost Management Contributor",
				f.eligibilities[0].RoleDefinitionGUID(),
				f.eligibilities[0].Properties.Scope,
				"Production",
				time.Now().Add(time.Hour),
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/roleAssignmentScheduleInstances") {
					if !partial || strings.HasPrefix(r.URL.Path, slowScope+"/") {
						<-r.Context().Done()
						return
					}
					writeJSON(t, w, map[string]any{"value": []armclient.Assignment{active}})
					return
				}
				base.Config.Handler.ServeHTTP(w, r)
			}))
			t.Cleanup(srv.Close)
			installSessionOpener(t, func(resolution, *timings, bool) ([]*session, []error, error) {
				tok := &azauth.Token{Context: "contoso", TenantID: "tid-1", PrincipalID: "oid-1", AccessToken: "fake"}
				return []*session{
					{Context: "contoso", Token: tok, Client: armclient.New(srv.URL, tok.AccessToken, srv.Client())},
				}, nil, nil
			})
			out, stderr, err := runCmd(t, "down", "-c", "contoso", "-y")
			if err == nil || ExitCode(err) != ExitFailed {
				t.Fatalf("incomplete deactivation succeeded: out=%q stderr=%q", out, stderr)
			}
			if strings.Contains(out, "no roles are currently activated") {
				t.Fatalf("unread scopes reported empty: %s", out)
			}
			want := 0
			if partial {
				want = 1
			}
			if got := len(f.putBodies()); got != want {
				t.Fatalf("submitted %d roles, want %d", got, want)
			}
		})
	}
}

func TestStatusAndDeactivate(t *testing.T) {
	end := time.Now().Add(58 * time.Minute)
	a := armclient.Assignment{
		ID: "/providers/Microsoft.Management/managementGroups/contoso-prod/providers/Microsoft.Authorization/roleAssignmentScheduleInstances/1",
	}
	a.Properties.AssignmentType = "Activated"
	a.Properties.Scope = "/providers/Microsoft.Management/managementGroups/contoso-prod"
	a.Properties.RoleDefinitionID = "/providers/Microsoft.Authorization/roleDefinitions/434105ed-43f6-45c7-a02f-909b2ba83430"
	a.Properties.EndDateTime = &end
	a.Properties.ExpandedProperties.RoleDefinition = armclient.Named{DisplayName: "Cost Management Contributor"}
	a.Properties.ExpandedProperties.Scope = armclient.Named{
		DisplayName: "Contoso landing zones",
		Type:        "managementgroup",
	}

	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles(), activated: []armclient.Assignment{a}}
	f.install()

	out, _, err := runCmd(t, "status", "-c", "contoso")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out, "Cost Management Contributor") || !strings.Contains(out, "1 activated role(s).") {
		t.Errorf("status output:\n%s", out)
	}
	if !strings.Contains(out, "58m") {
		t.Errorf("remaining time not shown:\n%s", out)
	}

	// list must mark the same role as active.
	out, _, err = runCmd(t, "list", "-c", "contoso")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "until ") {
		t.Errorf("list does not mark the activated role:\n%s", out)
	}

	f.setPutStatus("Revoked")
	out, _, err = runCmd(t, "deactivate", "-c", "contoso", "--all", "-y")
	if err != nil {
		t.Fatalf("deactivate: %v\n%s", err, out)
	}
	if len(f.putBodies()) != 1 {
		t.Fatalf("sent %d deactivation requests, want 1", len(f.putBodies()))
	}
	props := mustObject(t, f.putBodies()[0], "properties")
	if props["requestType"] != "SelfDeactivate" {
		t.Errorf("requestType = %v", props["requestType"])
	}
	if _, ok := props["scheduleInfo"]; ok {
		t.Error("a deactivation must not carry scheduleInfo")
	}
	if !strings.Contains(out, "DEACTIVATED") {
		t.Errorf("deactivation result:\n%s", out)
	}
}

func TestDeactivateErrorMapping(t *testing.T) {
	end := time.Now().Add(58 * time.Minute)
	a := armclient.Assignment{ID: "/x"}
	a.Properties.AssignmentType = "Activated"
	a.Properties.Scope = "/providers/Microsoft.Management/managementGroups/contoso-prod"
	a.Properties.RoleDefinitionID = "/providers/Microsoft.Authorization/roleDefinitions/434105ed-43f6-45c7-a02f-909b2ba83430"
	a.Properties.EndDateTime = &end
	a.Properties.ExpandedProperties.RoleDefinition = armclient.Named{DisplayName: "Cost Management Contributor"}
	a.Properties.ExpandedProperties.Scope = armclient.Named{
		DisplayName: "Contoso landing zones",
		Type:        "managementgroup",
	}

	// RoleAssignmentDoesNotExist must be a failure, not a benign "already gone".
	// Observed live: ARM answers this way for a minute or two after an
	// activation while the assignment propagates, and the role is still held.
	// Reporting success there would tell the user they had dropped access they
	// still have.
	f := &fakeARM{t: t, activated: []armclient.Assignment{a}}
	f.setPutErr(func(string) (int, string) {
		return 400, `{"error":{"code":"RoleAssignmentDoesNotExist","message":"The Role assignment does not exist."}}`
	})
	f.install()
	out, _, err := runCmd(t, "deactivate", "-c", "contoso", "--all", "-y")
	if err == nil {
		t.Fatal("a failed deactivation must exit non-zero — the role is still held")
	}
	if ExitCode(err) != ExitFailed {
		t.Errorf("exit code = %d, want 1", ExitCode(err))
	}
	if !strings.Contains(out, "FAILED") || !strings.Contains(out, "propagating") {
		t.Errorf("outcome not reported usefully:\n%s", out)
	}

	// The five-minute minimum is a failure, but an explained one.
	f2 := &fakeARM{t: t, activated: []armclient.Assignment{a}}
	f2.setPutErr(func(string) (int, string) {
		return 400, `{"error":{"code":"ActiveDurationTooShort","message":"The Active duration is too short. Miniumum Required is 5 minutes."}}`
	})
	f2.install()
	out, _, err = runCmd(t, "deactivate", "-c", "contoso", "--all", "-y")
	if err == nil {
		t.Fatal("expected a non-zero exit")
	}
	if !strings.Contains(out, "at least 5 minutes") {
		t.Errorf("the five-minute rule was not explained:\n%s", out)
	}
}

// TestDeactivatePendingApproval pins MED-11: a deactivation queued for a
// decision is reported as pending approval, not as a poll timeout.
func TestDeactivatePendingApproval(t *testing.T) {
	end := time.Now().Add(30 * time.Minute)
	a := armclient.Assignment{ID: "/x"}
	a.Properties.AssignmentType = "Activated"
	a.Properties.Scope = "/providers/Microsoft.Management/managementGroups/contoso-prod"
	a.Properties.RoleDefinitionID = "/providers/Microsoft.Authorization/roleDefinitions/434105ed-43f6-45c7-a02f-909b2ba83430"
	a.Properties.EndDateTime = &end
	a.Properties.ExpandedProperties.RoleDefinition = armclient.Named{DisplayName: "Cost Management Contributor"}
	a.Properties.ExpandedProperties.Scope = armclient.Named{
		DisplayName: "Contoso landing zones",
		Type:        "managementgroup",
	}

	f := &fakeARM{t: t, activated: []armclient.Assignment{a}, putStatus: "PendingApproval"}
	f.install()
	out, _, err := runCmd(t, "deactivate", "-c", "contoso", "--all", "-y")
	if err == nil {
		t.Fatal("expected a non-zero exit for a queued deactivation")
	}
	if got := ExitCode(err); got != ExitPending {
		t.Errorf("exit code = %d, want 2", got)
	}
	if !strings.Contains(out, "PENDING APPROVAL") {
		t.Errorf("a queued deactivation must not be reported as a timeout:\n%s", out)
	}
}

// TestBareDownMeansEverything: `pimctl down` with no arguments gives up every
// active role, which is the end-of-task gesture the verb exists for.
func TestBareDownMeansEverything(t *testing.T) {
	active := make([]armclient.Assignment, 0, 3)
	end := time.Now().Add(time.Hour)
	for _, leaf := range []string{"contoso-prod", "contoso-qa", "contoso-test"} {
		a := armclient.Assignment{ID: "/" + leaf}
		a.Properties.AssignmentType = "Activated"
		a.Properties.Scope = "/providers/Microsoft.Management/managementGroups/" + leaf
		a.Properties.RoleDefinitionID = "/providers/Microsoft.Authorization/roleDefinitions/434105ed"
		a.Properties.EndDateTime = &end
		a.Properties.ExpandedProperties.RoleDefinition = armclient.Named{DisplayName: "Cost Management Contributor"}
		a.Properties.ExpandedProperties.Scope = armclient.Named{
			DisplayName: "Contoso landing zones",
			Type:        "managementgroup",
		}
		active = append(active, a)
	}
	f := &fakeARM{t: t, activated: active, putStatus: "Revoked"}
	f.install()
	out, _, err := runCmd(t, "down", "-c", "contoso", "-y")
	if err != nil {
		t.Fatalf("down: %v", err)
	}
	if n := len(f.putBodies()); n != 3 {
		t.Fatalf("bare `down` sent %d deactivations, want all 3", n)
	}
	if strings.Count(out, "DEACTIVATED") != 3 {
		t.Errorf("results:\n%s", out)
	}
}

// TestNamedDeactivationIgnoresAnEmptyListing is the fix for the observed ARM
// bug: two genuinely active roles vanished from the activation listing for ten
// minutes, so `deactivate --all` reported nothing to do and exited 0 while the
// roles were still held. A *named* selection must ask ARM anyway.
func TestNamedDeactivationIgnoresAnEmptyListing(t *testing.T) {
	// No activations reported at all, though the roles are eligible.
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles(), activated: nil, putStatus: "Revoked"}
	f.install()

	out, _, err := runCmd(t, "down", "-c", "contoso", "--role", "Cost Management Contributor", "-y")
	if err != nil {
		t.Fatalf("down --role against an empty listing: %v", err)
	}
	if len(f.putBodies()) != 1 {
		t.Fatalf(
			"a named deactivation must be attempted regardless of the listing; sent %d requests",
			len(f.putBodies()),
		)
	}
	props := mustObject(t, f.putBodies()[0], "properties")
	if props["requestType"] != "SelfDeactivate" {
		t.Errorf("requestType = %v", props["requestType"])
	}
	if !strings.Contains(out, "DEACTIVATED") {
		t.Errorf("results:\n%s", out)
	}
}

func TestNamedDeactivationFromPresetIgnoresAnEmptyListing(t *testing.T) {
	// Provisioned for the activation that saves the preset; Revoked is the
	// success status for a deactivation only.
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()}
	f.install()
	if _, _, err := runCmd(t, "up", "-c", "contoso", "--all", "-j", "x", "--save-preset", "daily", "-y"); err != nil {
		t.Fatal(err)
	}

	// The listing goes blind, as ARM did.
	f.activated = nil
	f.resetPuts()
	f.setPutStatus("Revoked")
	out, _, err := runCmd(t, "down", "daily", "-y")
	if err != nil {
		t.Fatalf("down daily against an empty listing: %v", err)
	}
	if len(f.putBodies()) != 2 {
		t.Fatalf("the preset names 2 roles; sent %d deactivations", len(f.putBodies()))
	}
	if strings.Count(out, "DEACTIVATED") != 2 {
		t.Errorf("results:\n%s", out)
	}
}

// TestSpeculativeDeactivationReportsNotActive: when ARM genuinely says the role
// is not held, a named deactivation is a no-op, not a failure.
func TestSpeculativeDeactivationReportsNotActive(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()}
	f.setPutErr(func(string) (int, string) {
		return 400, `{"error":{"code":"RoleAssignmentDoesNotExist","message":"The Role assignment does not exist."}}`
	})
	f.install()

	out, _, err := runCmd(t, "down", "-c", "contoso", "--role", "Cost Management Contributor", "-y")
	if err != nil {
		t.Fatalf("a role that really is not active must not fail the run: %v", err)
	}
	if !strings.Contains(out, "NOT ACTIVE") {
		t.Errorf("outcome:\n%s", out)
	}
}

// TestListedDeactivationStillFailsOnDoesNotExist keeps the earlier fix: when the
// listing *did* show the role active, RoleAssignmentDoesNotExist means the
// assignment is still propagating and the role is still held.
func TestListedDeactivationStillFailsOnDoesNotExist(t *testing.T) {
	end := time.Now().Add(time.Hour)
	a := armclient.Assignment{ID: "/x"}
	a.Properties.AssignmentType = "Activated"
	a.Properties.Scope = "/providers/Microsoft.Management/managementGroups/contoso-prod"
	a.Properties.RoleDefinitionID = "/providers/Microsoft.Authorization/roleDefinitions/434105ed"
	a.Properties.EndDateTime = &end
	a.Properties.ExpandedProperties.RoleDefinition = armclient.Named{DisplayName: "Cost Management Contributor"}
	a.Properties.ExpandedProperties.Scope = armclient.Named{
		DisplayName: "Contoso landing zones",
		Type:        "managementgroup",
	}

	f := &fakeARM{t: t, activated: []armclient.Assignment{a}}
	f.setPutErr(func(string) (int, string) {
		return 400, `{"error":{"code":"RoleAssignmentDoesNotExist","message":"The Role assignment does not exist."}}`
	})
	f.install()

	out, _, err := runCmd(t, "down", "-c", "contoso", "-y")
	if err == nil {
		t.Fatal("a role the listing showed active must fail, not report NOT ACTIVE — it is still held")
	}
	if !strings.Contains(out, "FAILED") || !strings.Contains(out, "propagating") {
		t.Errorf("results:\n%s", out)
	}
}

// TestEmptyListingPrintsTheCaveat: `down` with no selection depends on the
// listing, so it has to warn that the listing can be wrong.
func TestEmptyListingPrintsTheCaveat(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()}
	f.install()
	out, errOut, err := runCmd(t, "down", "-c", "contoso", "-y")
	if err != nil {
		t.Fatalf("down with nothing active: %v", err)
	}
	if !strings.Contains(out, "Nothing to deactivate") {
		t.Errorf("stdout:\n%s", out)
	}
	for _, want := range []string{"still held", "pimctl down <preset>", "--key"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("the caveat should mention %q: %q", want, errOut)
		}
	}
}
