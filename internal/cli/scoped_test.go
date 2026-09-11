// Scoped selection tests pin source ambiguity, condition preservation and
// narrowed preset replay. They use fake ARM rather than a live tenant.

package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/larsakerlund/pimctl/internal/armclient"
)

func TestScopedEligibilityRejectsAmbiguityAndExpiredSources(t *testing.T) {
	a := mkElig("Reader", testProjectRole, testProjectSubscription, "Dev", "Subscription")
	b := mkElig(
		"Reader",
		testProjectRole,
		"/providers/Microsoft.Management/managementGroups/platform",
		"Platform",
		"ManagementGroup",
	)
	if _, err := chooseScopedEligibility(
		[]armclient.Eligibility{a, b},
		testProjectRole,
		testProjectScope,
		"",
	); err == nil {
		t.Fatal("chose arbitrary granting policy")
	}
	chosen, err := chooseScopedEligibility(
		[]armclient.Eligibility{a, b},
		testProjectRole,
		testProjectScope,
		a.Properties.RoleEligibilityScheduleID,
	)
	if err != nil || chosen.Properties.Scope != testProjectSubscription {
		t.Fatalf("explicit source: %#v, %v", chosen, err)
	}
	past := time.Now().Add(-time.Hour)
	a.Properties.EndDateTime = &past
	chosen, err = chooseScopedEligibility([]armclient.Eligibility{a, b}, testProjectRole, testProjectScope, "")
	if err != nil || chosen.Properties.Scope != b.Properties.Scope {
		t.Fatalf("expired source: %#v, %v", chosen, err)
	}
}

func TestScopedEligibilityFoldsOnlyEquivalentGrants(t *testing.T) {
	a := mkElig("Reader", testProjectRole, testProjectSubscription, "Dev", "Subscription")
	b := a
	b.Properties.MemberType = "Direct"
	b.Properties.RoleEligibilityScheduleID += "-direct"
	chosen, err := chooseScopedEligibility([]armclient.Eligibility{a, b}, testProjectRole, testProjectScope, "")
	if err != nil || chosen.Properties.MemberType != "Direct" {
		t.Fatalf("equivalent grants: %#v, %v", chosen, err)
	}
	b.Properties.Condition = "constrained"
	if _, err := chooseScopedEligibility(
		[]armclient.Eligibility{a, b},
		testProjectRole,
		testProjectScope,
		"",
	); err == nil {
		t.Fatal("folded different constraints")
	}
}

func TestAtNarrowedPresetRoundTrip(t *testing.T) {
	e := mkElig("Reader", testProjectRole, testProjectSubscription, "Dev", "Subscription")
	f := &fakeARM{t: t, eligibilities: []armclient.Eligibility{e}}
	installProject(t, f)
	for _, args := range [][]string{
		{"up", "--role", "Reader", "--at", testProjectScope, "--save-preset", "dev"},
		{"up", "dev"},
	} {
		args = append(args, "-c", "contoso", "-j", "test", "-y", "--no-wait")
		out, stderr, err := runCmd(t, args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s\n%s", args, err, out, stderr)
		}
	}
	if _, _, err := runCmd(t, "down", "dev", "-c", "contoso", "-y", "--no-wait"); err != nil {
		t.Fatal(err)
	}
	puts := f.putBodies()
	if len(puts) != 3 {
		t.Fatalf("PUTs=%d", len(puts))
	}
	for _, body := range puts {
		props := mustObject(t, body, "properties")
		if id := mustText(t, props, "roleDefinitionId"); !strings.HasPrefix(id, testProjectScope+"/") {
			t.Fatalf("replay widened target: %s", id)
		}
		if props["requestType"] == "SelfActivate" &&
			mustText(t, props, "linkedRoleEligibilityScheduleId") != e.Properties.RoleEligibilityScheduleID {
			t.Fatalf("changed source: %#v", props)
		}
	}
}
