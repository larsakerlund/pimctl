// Init tests generate portable requirements from fake Azure responses and
// prove failed setup never writes a project file or activates a role.

package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/larsakerlund/pimctl/internal/armclient"
	"github.com/larsakerlund/pimctl/internal/config"
)

func TestInitAtCreatesMinimalReplayableProject(t *testing.T) {
	e := mkElig("Reader", testProjectRole, testProjectSubscription, "Dev", "Subscription")
	f := &fakeARM{t: t, eligibilities: []armclient.Eligibility{e}}
	path := installProject(t, f)
	out, stderr, err := runCmd(t, "init", "--at", testProjectScope, "--role", "Reader", "-c", "contoso")
	if err != nil {
		t.Fatalf("init: %v\n%s\n%s", err, out, stderr)
	}
	p, err := config.LoadProject(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.Tenant != testProjectTenant || len(p.Roles) != 1 || p.Roles[0].Scope != testProjectScope {
		t.Fatalf("requirements: %#v", p)
	}
	data, err := os.ReadFile(path) //nolint:gosec // temporary file created by this test.
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "contoso") || strings.Contains(string(data), "Schedule") ||
		!strings.Contains(string(data), "# Reader") {
		t.Fatalf("not portable/readable: %s", data)
	}
	if len(f.putBodies()) != 0 {
		t.Fatal("init activated roles")
	}
	if _, _, err := runCmd(t, "init", "--at", testProjectScope, "--role", "Reader", "-c", "contoso"); err == nil {
		t.Fatal("init overwrote file")
	}
	if _, _, err := runCmd(t, "up", "-c", "contoso", "-j", "test", "-y", "--no-wait"); err != nil {
		t.Fatalf("generated file cannot replay: %v", err)
	}
}

func TestInitFromPresetUsesSelectedLogin(t *testing.T) {
	e := mkElig("Reader", testProjectRole, testProjectSubscription, "Dev", "Subscription")
	f := &fakeARM{t: t, eligibilities: []armclient.Eligibility{e}}
	path := installProject(t, f)
	ps := config.Presets{}
	ps.Set(
		"dev",
		[]config.PresetEntry{
			{
				Context:          "someone-elses-context",
				Scope:            testProjectScope,
				RoleDefinitionID: e.Properties.RoleDefinitionID,
			},
		},
	)
	if err := config.SavePresets(&ps); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCmd(t, "init", "--from-preset", "dev", "-c", "contoso"); err != nil {
		t.Fatal(err)
	}
	if _, err := config.LoadProject(path); err != nil {
		t.Fatal(err)
	}
	if len(f.putBodies()) != 0 {
		t.Fatal("init activated roles")
	}
}

func TestInitMissingRoleWritesNoFile(t *testing.T) {
	f := &fakeARM{t: t}
	path := installProject(t, f)
	if _, _, err := runCmd(t, "init", "--at", testProjectScope, "--role", "Reader", "-c", "contoso"); err == nil {
		t.Fatal("accepted missing eligibility")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("wrote failed setup: %v", err)
	}
}

func TestInitRejectsPartlyMatchingRoleFilters(t *testing.T) {
	f := &fakeARM{
		t: t,
		eligibilities: []armclient.Eligibility{
			mkElig("Reader", testProjectRole, testProjectSubscription, "Dev", "Subscription"),
		},
	}
	path := installProject(t, f)
	_, _, err := runCmd(
		t,
		"init",
		"--at",
		testProjectScope,
		"--role",
		"Reader",
		"--role",
		"misspelled-role",
		"-c",
		"contoso",
	)
	if err == nil {
		t.Fatal("silently omitted one requested role")
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("created partial requirements: %v", statErr)
	}
}
