// Project command regressions use fake ARM and temporary files. They cover
// scope and tenant safety without activating anything in a live tenant.

package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/larsakerlund/pimctl/internal/armclient"
	"github.com/larsakerlund/pimctl/internal/config"
)

const (
	testProjectTenant       = "11111111-1111-4111-8111-111111111111"
	testProjectRole         = "22222222-2222-4222-8222-222222222222"
	testProjectSubscription = "/subscriptions/33333333-3333-4333-8333-333333333333"
	testProjectScope        = testProjectSubscription + "/resourceGroups/dev"
)

func installProject(t *testing.T, f *fakeARM) string {
	t.Helper()
	f.install()
	open := fakeOpener
	installSessionOpener(t, func(res resolution, tm *timings, refresh bool) ([]*session, []error, error) {
		sessions, failures, err := open(res, tm, refresh)
		for _, s := range sessions {
			s.Token.TenantID = testProjectTenant
		}
		return sessions, failures, err
	})
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, config.ProjectFile)
}

func writeProject(t *testing.T, path string) {
	t.Helper()
	roles := []config.ProjectRole{{RoleDefinitionID: testProjectRole, Scope: testProjectScope}}
	if err := config.CreateProject(path, &config.Project{Tenant: testProjectTenant, Roles: roles}); err != nil {
		t.Fatal(err)
	}
}

func TestProjectUpNarrowsInheritedEligibility(t *testing.T) {
	source := "/providers/Microsoft.Management/managementGroups/platform"
	e := mkElig("Reader", testProjectRole, source, "Platform", "ManagementGroup")
	e.Properties.Condition = "@Resource[example:Name] StringEquals 'dev'"
	e.Properties.ConditionVersion = "2.0"
	f := &fakeARM{t: t, eligibilities: []armclient.Eligibility{e}}
	path := installProject(t, f)
	writeProject(t, path)
	out, stderr, err := runCmd(t, "up", "-c", "contoso", "-j", "Development", "-y", "--no-wait")
	if err != nil {
		t.Fatalf("up: %v\n%s\n%s", err, out, stderr)
	}
	puts := f.putBodies()
	if len(puts) != 1 {
		t.Fatalf("PUTs: %d", len(puts))
	}
	props := mustObject(t, puts[0], "properties")
	for key, want := range map[string]string{
		"roleDefinitionId":                testProjectScope + "/providers/Microsoft.Authorization/roleDefinitions/" + testProjectRole,
		"linkedRoleEligibilityScheduleId": e.Properties.RoleEligibilityScheduleID,
		"principalId":                     "oid-1", "condition": e.Properties.Condition, "conditionVersion": "2.0",
	} {
		if got := mustText(t, props, key); got != want {
			t.Errorf("%s=%q, want %q", key, got, want)
		}
	}
	if !strings.Contains(stderr, source) || !strings.Contains(stderr, path) {
		t.Fatalf("missing source/file disclosure: %s", stderr)
	}
}

func TestProjectUpPreflightSubmitsNothing(t *testing.T) {
	for _, kind := range []string{"missing role", "wrong tenant", "ticket"} {
		t.Run(kind, func(t *testing.T) {
			f := &fakeARM{
				t: t,
				eligibilities: []armclient.Eligibility{
					mkElig("Reader", testProjectRole, testProjectSubscription, "Dev", "Subscription"),
				},
			}
			path := installProject(t, f)
			p := &config.Project{
				Tenant: testProjectTenant,
				Roles:  []config.ProjectRole{{RoleDefinitionID: testProjectRole, Scope: testProjectScope}},
			}
			switch kind {
			case "missing role":
				p.Roles = append(
					p.Roles,
					config.ProjectRole{
						RoleDefinitionID: "44444444-4444-4444-8444-444444444444",
						Scope:            testProjectScope,
					},
				)
			case "wrong tenant":
				p.Tenant = "55555555-5555-4555-8555-555555555555"
			case "ticket":
				f.enabledRules = []string{"Ticketing"}
			}
			if err := config.CreateProject(path, p); err != nil {
				t.Fatal(err)
			}
			_, _, err := runCmd(t, "up", "-c", "contoso", "-j", "Development", "-y", "--no-wait")
			if err == nil || len(f.putBodies()) != 0 {
				t.Fatalf("preflight: err=%v, PUTs=%d", err, len(f.putBodies()))
			}
			if kind == "wrong tenant" && f.eligCount() != 0 {
				t.Fatal("looked up roles before tenant validation")
			}
		})
	}
}

func TestProjectDownTargetsOnlyExactRequirementsDespiteDroppedListing(t *testing.T) {
	f := &fakeARM{
		t: t,
		eligibilities: []armclient.Eligibility{
			mkElig("Reader", testProjectRole, testProjectSubscription, "Dev", "Subscription"),
		},
	}
	path := installProject(t, f)
	writeProject(t, path)
	out, stderr, err := runCmd(t, "down", "--project", "-c", "contoso", "-y", "--no-wait")
	if err != nil {
		t.Fatalf("down: %v\n%s\n%s", err, out, stderr)
	}
	puts := f.putBodies()
	if len(puts) != 1 {
		t.Fatalf("PUTs: %d", len(puts))
	}
	props := mustObject(t, puts[0], "properties")
	if got := mustText(t, props, "requestType"); got != "SelfDeactivate" {
		t.Fatal(got)
	}
	if got := mustText(t, props, "roleDefinitionId"); !strings.HasPrefix(got, testProjectScope+"/") {
		t.Fatalf("wrong target: %s", got)
	}
}

func TestMissingProjectDownAndStatusNeverFallBack(t *testing.T) {
	f := &fakeARM{t: t}
	path := installProject(t, f)
	writeProject(t, path)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"down", "status"} {
		_, _, err := runCmd(t, command, "--project", "-c", "contoso", "-y")
		if command == "status" {
			_, _, err = runCmd(t, command, "--project", "-c", "contoso")
		}
		if err == nil || !strings.Contains(err.Error(), "no .pimctl.yaml") {
			t.Fatalf("%s: %v", command, err)
		}
	}
	if f.eligCount() != 0 || len(f.putBodies()) != 0 {
		t.Fatal("missing file reached Azure")
	}
}

func TestProjectFileDoesNotChangeBareStatusOrExplicitSelection(t *testing.T) {
	f := &fakeARM{
		t: t,
		eligibilities: []armclient.Eligibility{
			mkElig("Reader", testProjectRole, testProjectSubscription, "Dev", "Subscription"),
		},
	}
	path := installProject(t, f)
	if err := writeFile(path, "broken: ["); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCmd(t, "status", "-c", "contoso"); err != nil {
		t.Fatalf("status read project: %v", err)
	}
	if _, _, err := runCmd(t, "down", "-c", "contoso", "-y"); err != nil {
		t.Fatalf("down read project: %v", err)
	}
	if _, _, err := runCmd(t, "up", "--role", "Reader", "-c", "contoso", "-j", "test", "-y", "--no-wait"); err != nil {
		t.Fatalf("explicit selection read project: %v", err)
	}
	if _, _, err := runCmd(
		t,
		"up",
		"-c",
		"contoso",
		"-j",
		"test",
		"-y",
	); err == nil ||
		!strings.Contains(err.Error(), "invalid") {
		t.Fatalf("malformed default fell back: %v", err)
	}
}

func TestProjectUpPreservesAlreadyActiveWindow(t *testing.T) {
	e := mkElig("Reader", testProjectRole, testProjectSubscription, "Dev", "Subscription")
	until := time.Now().Add(time.Hour)
	active := armclient.Assignment{}
	active.Properties.Scope = testProjectScope
	active.Properties.RoleDefinitionID = testProjectScope + "/providers/Microsoft.Authorization/roleDefinitions/" + testProjectRole
	active.Properties.EndDateTime = &until
	active.Properties.Status = "Provisioned"
	active.Properties.AssignmentType = "Activated"
	f := &fakeARM{
		t:             t,
		eligibilities: []armclient.Eligibility{e},
		activated:     []armclient.Assignment{active},
		enabledRules:  []string{"Ticketing", "Justification"},
	}
	path := installProject(t, f)
	writeProject(t, path)
	out, stderr, err := runCmd(t, "up", "-c", "contoso", "-j", "test", "-y", "-o", "json")
	if err != nil {
		t.Fatalf("up: %v\n%s\n%s", err, out, stderr)
	}
	if len(f.putBodies()) != 0 || !strings.Contains(out, "ALREADY ACTIVE") || !strings.Contains(out, "until") {
		t.Fatalf("did not preserve window: PUTs=%d, %s", len(f.putBodies()), out)
	}
}

// projectTestTransport observes fake ARM requests without changing the production
// client or executing any real Azure CLI command.
type projectTestTransport struct {
	base    http.RoundTripper
	inspect func(*http.Request) error
}

func (p projectTestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := p.inspect(r); err != nil {
		return nil, err
	}
	return p.base.RoundTrip(r)
}

func inspectProjectRequests(t *testing.T, inspect func(*http.Request) error) {
	t.Helper()
	open := fakeOpener
	installSessionOpener(t, func(res resolution, tm *timings, refresh bool) ([]*session, []error, error) {
		sessions, failures, err := open(res, tm, refresh)
		for _, s := range sessions {
			s.Client.HTTP.Transport = projectTestTransport{base: s.Client.HTTP.Transport, inspect: inspect}
		}
		return sessions, failures, err
	})
}

func TestProjectLookupFailureIsNotMissingEligibility(t *testing.T) {
	f := &fakeARM{
		t: t,
		eligibilities: []armclient.Eligibility{
			mkElig("Reader", testProjectRole, testProjectSubscription, "Dev", "Subscription"),
		},
	}
	path := installProject(t, f)
	writeProject(t, path)
	inspectProjectRequests(t, func(r *http.Request) error {
		if strings.Contains(r.URL.Path, "roleEligibilityScheduleInstances") {
			return errors.New("lookup unavailable")
		}
		return nil
	})
	_, _, err := runCmd(t, "up", "-c", "contoso", "-j", "test", "-y", "--no-wait")
	if err == nil || !strings.Contains(err.Error(), "could not verify") ||
		strings.Contains(err.Error(), "no matching eligible") {
		t.Fatalf("wrong failure: %v", err)
	}
	if len(f.putBodies()) != 0 {
		t.Fatal("submitted after failed preflight")
	}
}

func TestProjectUsesSourcePolicyAndPrintsBeforeNetwork(t *testing.T) {
	f := &fakeARM{
		t: t,
		eligibilities: []armclient.Eligibility{
			mkElig("Reader", testProjectRole, testProjectSubscription, "Dev", "Subscription"),
		},
	}
	path := installProject(t, f)
	writeProject(t, path)
	var out, errb lockedBuffer
	inspectProjectRequests(t, func(r *http.Request) error {
		if !strings.Contains(errb.String(), path) {
			t.Error("network before selected file was printed")
		}
		if strings.Contains(r.URL.Path, "roleManagementPolicyAssignments") &&
			!strings.HasPrefix(r.URL.Path, testProjectSubscription+"/providers/") {
			t.Errorf("policy read at target instead of granting scope: %s", r.URL.Path)
		}
		return nil
	})
	root := newRootCmd(testDeps())
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs([]string{"up", "-c", "contoso", "-j", "test", "-y", "--no-wait"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestProjectUpVerifiesRecentlyActivatedRecord(t *testing.T) {
	f := &fakeARM{
		t: t,
		eligibilities: []armclient.Eligibility{
			mkElig("Reader", testProjectRole, testProjectSubscription, "Dev", "Subscription"),
		},
	}
	path := installProject(t, f)
	writeProject(t, path)
	if _, _, err := runCmd(t, "up", "-c", "contoso", "-j", "test", "-y"); err != nil {
		t.Fatal(err)
	}
	f.resetPuts()
	out, stderr, err := runCmd(t, "up", "-c", "contoso", "-j", "test", "-y")
	if err != nil {
		t.Fatalf("repeat: %v\n%s\n%s", err, out, stderr)
	}
	if len(f.putBodies()) != 0 || !strings.Contains(out, "ALREADY ACTIVE") || f.requestGetCount() == 0 {
		t.Fatalf("did not verify recorded activation: %s\n%s", out, stderr)
	}
}

func TestProjectUpHonoursRecentDeactivationTombstone(t *testing.T) {
	e := mkElig("Reader", testProjectRole, testProjectSubscription, "Dev", "Subscription")
	until := time.Now().Add(time.Hour)
	active := armclient.Assignment{}
	active.Properties.Scope = testProjectScope
	active.Properties.RoleDefinitionID = testProjectScope + "/providers/Microsoft.Authorization/roleDefinitions/" + testProjectRole
	active.Properties.AssignmentType = "Activated"
	active.Properties.EndDateTime = &until
	f := &fakeARM{t: t, eligibilities: []armclient.Eligibility{e}, activated: []armclient.Assignment{active}}
	path := installProject(t, f)
	writeProject(t, path)
	if _, _, err := runCmd(t, "down", "--project", "-c", "contoso", "-y"); err != nil {
		t.Fatal(err)
	}
	f.resetPuts()
	if _, _, err := runCmd(t, "up", "-c", "contoso", "-j", "test", "-y"); err != nil {
		t.Fatal(err)
	}
	if len(f.putBodies()) != 1 {
		t.Fatal("stale Azure listing suppressed reactivation after down")
	}
}
