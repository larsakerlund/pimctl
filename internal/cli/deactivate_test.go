// Tests for deactivate.go and target.go, driven through the cobra commands so
// the flag wiring is under test too. Several of them exist because ARM's
// activation listing has been seen to omit roles that are genuinely held: a
// named deactivation must not trust it, and a bare one must say so. The fake
// ARM they run against is in fake_test.go.

package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
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

func TestDownIncludesRecordedActivations(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprintf("partial=%t", partial), func(t *testing.T) {
			elig := twoLowImpactRoles()
			f := &fakeARM{t: t, eligibilities: elig}
			if partial {
				a := armclient.Assignment{ID: "/listed"}
				a.Properties.AssignmentType = "Activated"
				a.Properties.Scope = elig[1].Properties.Scope
				a.Properties.RoleDefinitionID = elig[1].Properties.RoleDefinitionID
				a.Properties.ExpandedProperties = elig[1].Properties.ExpandedProperties
				f.activated = []armclient.Assignment{a}
			}
			f.install()
			entry := mkRecordEntry("Contributor", "contoso-prod", time.Hour)
			entry.Start = time.Now().Add(-10 * time.Minute)
			entry.WrittenAt = entry.Start
			entry.Listed = true
			writeRecord(testOwner("contoso"), []recordEntry{entry})
			out, stderr, err := runCmd(t, "down", "-c", "contoso", "-y")
			if err != nil {
				t.Fatalf("down: %v; %s; %s", err, out, stderr)
			}
			want := 1
			if partial {
				want = 2
			}
			if len(f.putBodies()) != want {
				t.Fatalf("made %d deactivation requests, want %d", len(f.putBodies()), want)
			}
			for _, held := range readRecord(testOwner("contoso")) {
				if !held.Revoked() {
					t.Errorf("recorded role was not deactivated: %s", held.Role)
				}
			}
		})
	}
}

// TestDownNamesANarrowerScopeActivation: an activation made at a narrower
// scope than its eligibility (`up --at`) has a key of its own, so --role,
// --scope and --key must resolve against the activations as well as the
// eligibilities. Each case activates Reader at a resource group under a
// subscription-level eligibility, then names it for deactivation.
func TestDownNamesANarrowerScopeActivation(t *testing.T) {
	at := testProjectSubscription + "/resourceGroups/app-rg"
	cases := []struct {
		name string
		args []string
		want []string // scopes a deactivation must be sent at, and no others.
	}{
		{"role reaches both scopes", []string{"--role", "Reader"}, []string{at, testProjectSubscription}},
		{"scope matches the activation", []string{"--role", "Reader", "--scope", "app-rg"}, []string{at}},
		{
			"scope matches the granting scope",
			[]string{"--role", "Reader", "--scope", "Dev"},
			[]string{testProjectSubscription},
		},
		{"key of the activation", []string{"--key", selectionKeyFor("contoso", at, testProjectRole)}, []string{at}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := mkElig("Reader", testProjectRole, testProjectSubscription, "Dev", "Subscription")
			f := &fakeARM{t: t, eligibilities: []armclient.Eligibility{e}}
			installProject(t, f)
			out, stderr, err := runCmd(
				t,
				"up",
				"--role",
				"Reader",
				"--at",
				at,
				"-c",
				"contoso",
				"-j",
				"test",
				"-y",
				"--no-wait",
			)
			if err != nil {
				t.Fatalf("up --at: %v\n%s\n%s", err, out, stderr)
			}
			f.resetPuts()

			args := append([]string{"down", "-c", "contoso", "-y", "--no-wait"}, tc.args...)
			out, stderr, err = runCmd(t, args...)
			if err != nil {
				t.Fatalf("%v: %v\n%s\n%s", args, err, out, stderr)
			}
			puts := f.putBodies()
			got := make([]string, 0, len(puts))
			for _, body := range puts {
				props := mustObject(t, body, "properties")
				if props["requestType"] != "SelfDeactivate" {
					t.Fatalf("requestType = %v", props["requestType"])
				}
				id := mustText(t, props, "roleDefinitionId")
				scope, _, ok := strings.Cut(id, "/providers/Microsoft.Authorization/roleDefinitions/")
				if !ok {
					t.Fatalf("roleDefinitionId not qualified to a scope: %s", id)
				}
				got = append(got, scope)
			}
			slices.Sort(got)
			want := slices.Clone(tc.want)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("deactivated at %v, want %v\n%s", got, want, out)
			}
		})
	}
}

// TestDownRejectsPresetGivenTwice: two different presets, one bare and one by
// flag, are refused with the wording `up` uses; the same name both ways is not.
func TestDownRejectsPresetGivenTwice(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()}
	f.install()
	want := `preset given twice: "daily" and --preset "other"`
	for _, verb := range []string{"down", "deactivate"} {
		_, _, err := runCmd(t, verb, "daily", "--preset", "other", "-c", "contoso", "-y")
		if err == nil || err.Error() != want {
			t.Errorf("%s daily --preset other: err = %v, want %q", verb, err, want)
		}
	}
	if _, _, err := runCmd(t, "down", "daily", "--preset", "daily", "-c", "contoso", "-y"); err != nil &&
		strings.Contains(err.Error(), "given twice") {
		t.Errorf("the same preset named both ways was refused: %v", err)
	}
}

// TestDownKeyUsageNamesNoValue: pflag renders backticked text in a usage string
// as the flag's value name, so the --key usage must not carry any.
func TestDownKeyUsageNamesNoValue(t *testing.T) {
	f := &fakeARM{t: t}
	f.install()
	out, _, err := runCmd(t, "down", "--help")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "--key pimctl list") || !strings.Contains(out, "--key stringArray") {
		t.Errorf("--key usage:\n%s", out)
	}
}

// TestNamedDownDoesNotWaitForTheListing: a named selection carries its own
// scopes and role ids, so its requests go out on the eligibility rows and the
// record alone. With every per-scope listing blocked for the whole run, the
// request still reaches ARM well inside the soft deadline the listing would
// have cost it. What happens afterwards differs: a filter selection waits out
// what is left of that deadline, and no longer, in case the listing names an
// activation nothing on hand did; a preset names its scopes outright, so it
// waits for nothing and the command itself finishes inside the same bound.
func TestNamedDownDoesNotWaitForTheListing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		widens bool // whether the selection is one the listing could add to.
	}{
		{"role", []string{"--role", "Cost Management Contributor"}, true},
		{"preset", []string{"daily"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeARM{t: t, eligibilities: twoLowImpactRoles(), putStatus: "Revoked"}
			f.install()
			writePreset(t, "daily", "contoso")
			base := f.srv
			// startedAt and firstPut time the request rather than the command:
			// what a named down promises is that the fan-out is not on the way
			// to ARM, which is true whatever it does with the answer after.
			var startedAt, firstPut atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/roleAssignmentScheduleInstances") {
					<-r.Context().Done()
					return
				}
				if r.Method == http.MethodPut {
					firstPut.CompareAndSwap(0, time.Now().UnixNano()-startedAt.Load())
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

			start := time.Now()
			startedAt.Store(start.UnixNano())
			args := append([]string{"down", "-c", "contoso", "-y"}, tc.args...)
			out, stderr, err := runCmd(t, args...)
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("%v: %v\n%s\n%s", args, err, out, stderr)
			}
			limit := defaultScopeSoftDeadline / 2
			if sent := time.Duration(firstPut.Load()); sent == 0 || sent > limit {
				t.Errorf("the deactivation reached ARM after %v, on a blocked listing (limit %v)", sent, limit)
			}
			if tc.widens {
				// The filter is re-run against the listing once the request is
				// out, so the command may wait the rest of the deadline — but
				// not past it.
				limit = defaultScopeSoftDeadline + time.Second
			}
			if elapsed > limit {
				t.Errorf("a named down waited %v on the blocked listing (limit %v)", elapsed, limit)
			}
			if len(f.putBodies()) != 1 || !strings.Contains(out, "DEACTIVATED") {
				t.Errorf("sent %d requests; results:\n%s", len(f.putBodies()), out)
			}
			// A selection that widens gives the listing the rest of its own
			// deadline, which is the moment the blocked scopes are given up on
			// too, so whether their names make it into the note is a race and
			// not something to pin. A preset never looks.
			if !tc.widens && strings.Contains(stderr, "could not read") {
				t.Errorf("a listing that never landed was reported as read:\n%s", stderr)
			}
		})
	}
}

// TestNamedDownWidensToAnActivationOnlyTheListingKnows: `up --at` from another
// machine leaves an activation at a narrower scope than the eligibility, which
// this machine's record cannot know. --role matches the eligibility at once and
// the request for it goes out, and the same filter is then re-run against the
// listing, which names the resource-group activation as well — both are given
// up, and both are reported.
func TestNamedDownWidensToAnActivationOnlyTheListingKnows(t *testing.T) {
	at := testProjectSubscription + "/resourceGroups/app-rg"
	e := mkElig("Reader", testProjectRole, testProjectSubscription, "Dev", "Subscription")
	a := mkActivated("Reader", testProjectRole, at, "app-rg", time.Now().Add(time.Hour))
	a.ID = at + "/providers/Microsoft.Authorization/roleAssignmentScheduleInstances/1"
	// Qualified at the activation scope, as ARM's per-scope listing reports it.
	a.Properties.RoleDefinitionID = at + "/providers/Microsoft.Authorization/roleDefinitions/" + testProjectRole
	f := &fakeARM{
		t:             t,
		eligibilities: []armclient.Eligibility{e},
		activated:     []armclient.Assignment{a},
		putStatus:     "Revoked",
		// The listing lands after the requests have gone out, which is the
		// order this exists to cover.
		activeDelay: 300 * time.Millisecond,
	}
	f.install()

	out, stderr, err := runCmd(t, "down", "-c", "contoso", "-y", "--no-wait", "--role", "Reader")
	if err != nil {
		t.Fatalf("down --role Reader: %v\n%s\n%s", err, out, stderr)
	}
	got := make([]string, 0, 2)
	for _, body := range f.putBodies() {
		props := mustObject(t, body, "properties")
		if props["requestType"] != "SelfDeactivate" {
			t.Fatalf("requestType = %v", props["requestType"])
		}
		id := mustText(t, props, "roleDefinitionId")
		scope, _, ok := strings.Cut(id, "/providers/Microsoft.Authorization/roleDefinitions/")
		if !ok {
			t.Fatalf("roleDefinitionId not qualified to a scope: %s", id)
		}
		got = append(got, scope)
	}
	slices.Sort(got)
	want := []string{at, testProjectSubscription}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("deactivated at %v, want %v\n%s", got, want, out)
	}
	if n := strings.Count(out, "DEACTIVATED"); n != 2 {
		t.Errorf("the results table reported %d deactivations, want 2:\n%s", n, out)
	}
}

// TestNamedDownFindsAnActivationOnlyTheListingKnows: when --role/--scope or
// --key match nothing in the eligibilities and the record, the activation
// named may still be one the listing knows — made at a narrower scope from
// another machine — so that is the one case a named down waits for it.
func TestNamedDownFindsAnActivationOnlyTheListingKnows(t *testing.T) {
	at := testProjectSubscription + "/resourceGroups/app-rg"
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"scope", []string{"--role", "Reader", "--scope", "app-rg"}},
		{"key", []string{"--key", selectionKeyFor("contoso", at, testProjectRole)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := mkElig("Reader", testProjectRole, testProjectSubscription, "Dev", "Subscription")
			a := mkActivated("Reader", testProjectRole, at, "app-rg", time.Now().Add(time.Hour))
			a.ID = at + "/providers/Microsoft.Authorization/roleAssignmentScheduleInstances/1"
			// Qualified at the activation scope, as ARM's per-scope listing
			// reports it, so the request shows which row it was built from.
			a.Properties.RoleDefinitionID = at + "/providers/Microsoft.Authorization/roleDefinitions/" + testProjectRole
			f := &fakeARM{
				t:             t,
				eligibilities: []armclient.Eligibility{e},
				activated:     []armclient.Assignment{a},
				putStatus:     "Revoked",
				activeDelay:   300 * time.Millisecond,
			}
			f.install()

			args := append([]string{"down", "-c", "contoso", "-y", "--no-wait"}, tc.args...)
			out, stderr, err := runCmd(t, args...)
			if err != nil {
				t.Fatalf("%v: %v\n%s\n%s", args, err, out, stderr)
			}
			puts := f.putBodies()
			if len(puts) != 1 {
				t.Fatalf("sent %d deactivations, want 1\n%s", len(puts), out)
			}
			props := mustObject(t, puts[0], "properties")
			if id := mustText(t, props, "roleDefinitionId"); !strings.HasPrefix(id, at+"/") {
				t.Fatalf("deactivated at the wrong scope: %s", id)
			}
		})
	}
}

// TestNamedDownReadsDoesNotExistAgainstTheListing: a named down no longer
// reads the listing before it asks ARM, but RoleAssignmentDoesNotExist still
// means what it always did. When the listing — landing after the request —
// shows the role held, the answer is propagation: the request is sent once
// more with that evidence and the second refusal is a failure. When nothing
// shows the role held, ARM's answer stands and the role is NOT ACTIVE.
func TestNamedDownReadsDoesNotExistAgainstTheListing(t *testing.T) {
	a := mkActivated(
		"Cost Management Contributor",
		costGUID,
		"/providers/Microsoft.Management/managementGroups/contoso-prod",
		"Contoso landing zones",
		time.Now().Add(time.Hour),
	)
	a.ID = a.Properties.Scope + "/providers/Microsoft.Authorization/roleAssignmentScheduleInstances/1"
	for _, tc := range []struct {
		name     string
		listed   []armclient.Assignment
		attempts int32
		want     string
		fails    bool
	}{
		{"listed", []armclient.Assignment{a}, 2, "propagating", true},
		{"not listed", nil, 1, "NOT ACTIVE", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeARM{
				t:             t,
				eligibilities: twoLowImpactRoles(),
				activated:     tc.listed,
				activeDelay:   300 * time.Millisecond,
			}
			var attempts atomic.Int32
			f.setPutErr(func(string) (int, string) {
				attempts.Add(1)
				return 400, `{"error":{"code":"RoleAssignmentDoesNotExist","message":"The Role assignment does not exist."}}`
			})
			f.install()

			out, _, err := runCmd(t, "down", "-c", "contoso", "--role", "Cost Management Contributor", "-y")
			switch {
			case tc.fails && (err == nil || ExitCode(err) != ExitFailed):
				t.Fatalf("a role the listing shows held must fail: err = %v\n%s", err, out)
			case !tc.fails && err != nil:
				t.Fatalf("a role nothing shows held must not fail the run: %v\n%s", err, out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("results:\n%s", out)
			}
			if got := attempts.Load(); got != tc.attempts {
				t.Errorf("sent the request %d time(s), want %d", got, tc.attempts)
			}
		})
	}
}

// TestTargetFromEvidenceTrustsOnlyConfirmedRows: a recorded activation the
// listing has not caught up with is this machine's own claim, which a bare
// down checks against its schedule request first; a named down has not, so
// such a target goes out without SeenActive. Every other row counts as held.
func TestTargetFromEvidenceTrustsOnlyConfirmedRows(t *testing.T) {
	for state, want := range map[rowState]bool{RowConfirmed: true, RowUnconfirmed: true, RowConfirming: false} {
		r := activeRow{Context: "contoso", State: state}
		r.Assignment = mkActivated("Reader", testProjectRole, testProjectSubscription, "Dev", time.Now().Add(time.Hour))
		if got := targetFromEvidence(r).SeenActive; got != want {
			t.Errorf("%s: SeenActive = %t, want %t", state, got, want)
		}
	}
}
