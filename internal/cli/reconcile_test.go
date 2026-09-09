// Tests for reconcile.go, verify.go and localrecord.go: what pimctl does about
// ARM's listing lag in both directions — a fresh activation believed on the
// strength of its own schedule request, a fresh deactivation held off by a
// tombstone, a 30-minute ceiling bounding both, and nothing ever dropped in
// silence. The record's file format is tested in record_test.go.

package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/larsakerlund/pimctl/internal/armclient"
	"github.com/larsakerlund/pimctl/internal/azauth"
)

// TestUpWritesTheActivationRecord is the test that would have caught the record
// writes becoming dead code.
//
// The unit tests called RecordActivations directly, so they kept passing after
// a refactor replaced the call site — and every `status` for the next minute
// reported freshly activated roles as not existing. This goes through the cobra
// command, so the wiring itself is what is under test.
func TestUpWritesTheActivationRecord(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()}
	f.install()

	if _, _, err := runCmd(t, "up", "-c", "contoso", "--all", "-j", "x", "-y"); err != nil {
		t.Fatalf("up: %v", err)
	}

	got := readRecord("contoso")
	if len(got) != 2 {
		t.Fatalf("the record holds %d entries after activating 2 roles, want 2", len(got))
	}
	roles := map[string]bool{}
	for _, e := range got {
		roles[e.Role] = true
		checkRecordEntry(t, e)
	}
	for _, want := range []string{"Cost Management Contributor", "Resource Policy Contributor"} {
		if !roles[want] {
			t.Errorf("the record is missing %q: %v", want, roles)
		}
	}

	// And status must show them from the record, before any ARM call.
	rows := localActiveRows(&runContext{Sessions: []*session{{
		Context: "contoso", Token: &azauth.Token{Context: "contoso"},
	}}})
	if len(rows) != 2 {
		t.Fatalf("status would show %d roles from the record, want 2", len(rows))
	}
	for _, r := range rows {
		if !r.Unconfirmed() {
			t.Error("a row from the record must not be marked confirmed")
		}
	}
}

// TestDownForgetsFromTheActivationRecord is the matching half.
func TestDownForgetsFromTheActivationRecord(t *testing.T) {
	end := time.Now().Add(time.Hour)
	a := armclient.Assignment{ID: "/x"}
	a.Properties.AssignmentType = "Activated"
	a.Properties.Scope = "/providers/Microsoft.Management/managementGroups/contoso-prod"
	a.Properties.RoleDefinitionID = "/providers/Microsoft.Authorization/roleDefinitions/" + costGUID
	a.Properties.EndDateTime = &end
	a.Properties.ExpandedProperties.RoleDefinition = armclient.Named{DisplayName: "Cost Management Contributor"}
	a.Properties.ExpandedProperties.Scope = armclient.Named{
		DisplayName: "Contoso landing zones",
		Type:        "managementgroup",
	}

	f := &fakeARM{t: t, activated: []armclient.Assignment{a}, putStatus: "Revoked"}
	f.install()

	// Seed the record as an earlier `up` would have.
	entry := recordEntry{
		Context: "contoso", Scope: a.Properties.Scope,
		RoleDefinitionID: a.Properties.RoleDefinitionID,
		Role:             "Cost Management Contributor", End: end,
	}
	entry.Key = recordKey(entry.Context, entry.Scope, entry.RoleDefinitionID)
	writeRecord("contoso", []recordEntry{entry})

	if _, _, err := runCmd(t, "down", "-c", "contoso", "-y"); err != nil {
		t.Fatalf("down: %v", err)
	}
	if got := heldEntries(); len(got) != 0 {
		t.Errorf("the record still holds %d role(s) after deactivating: %+v", len(got), got)
	}
	// The tombstone stays until ARM agrees, or ARM's lagging listing would
	// put the role straight back on the next status.
	tomb := readRecord("contoso")
	if len(tomb) != 1 || !tomb[0].Revoked() {
		t.Errorf("deactivating left no tombstone: %+v", tomb)
	}
}

// lagScope is the management group the propagation tests act on.
const lagScope = "/providers/Microsoft.Management/managementGroups/contoso-prod"

// lagFixtures builds one eligibility and the matching activation, so a test can
// hand ARM either "not activated yet" or "still activated" for the same role.
func lagFixtures(t *testing.T) (armclient.Eligibility, armclient.Assignment, recordEntry) {
	t.Helper()
	end := time.Now().Add(time.Hour)
	elig := mkElig("Cost Management Contributor", costGUID, lagScope, "Contoso landing zones", "managementgroup")

	a := armclient.Assignment{ID: "/x"}
	a.Properties.AssignmentType = "Activated"
	a.Properties.Scope = lagScope
	a.Properties.RoleDefinitionID = lagScope + "/providers/Microsoft.Authorization/roleDefinitions/" + costGUID
	a.Properties.EndDateTime = &end
	a.Properties.ExpandedProperties.RoleDefinition = armclient.Named{DisplayName: "Cost Management Contributor"}
	a.Properties.ExpandedProperties.Scope = armclient.Named{
		DisplayName: "Contoso landing zones", Type: "managementgroup", ID: lagScope,
	}

	e := recordEntry{
		Context: "contoso", Scope: lagScope,
		RoleDefinitionID: a.Properties.RoleDefinitionID,
		Role:             "Cost Management Contributor",
		ScopeName:        "Contoso landing zones", ScopeType: "ManagementGroup",
		RequestID: lagScope + "/providers/Microsoft.Authorization/roleAssignmentScheduleRequests/req-1",
		End:       end, Status: string(OutcomeActivated), WrittenAt: time.Now(),
	}
	e.Key = recordKey(e.Context, e.Scope, e.RoleDefinitionID)
	return elig, a, e
}

// TestFreshActivationSurvivesTheListingLag: ARM's per-scope read lags an
// activation by up to two minutes — measured on a real tenant, a PUT accepted at
// 13:12:02Z was absent at 13:13:42 and present at 13:14:06. Reconciling against
// that read would drop the role pimctl had just activated and announce it as no
// longer held, which is the most alarming thing this command can say.
func TestFreshActivationSurvivesTheListingLag(t *testing.T) {
	elig, _, entry := lagFixtures(t)
	f := &fakeARM{t: t, eligibilities: []armclient.Eligibility{elig}}
	f.install()
	writeRecord("contoso", []recordEntry{entry})

	out, errOut, err := runCmd(t, "status", "-c", "contoso", "--wait")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out, "Cost Management Contributor") {
		t.Errorf("a role activated moments ago was dropped:\n%s", out)
	}
	if !strings.Contains(out, "~") {
		t.Errorf("the row must be marked as still confirming:\n%s", out)
	}
	if strings.Contains(errOut, "no longer held") {
		t.Errorf("a role ARM has yet to list is not a role lost: %q", errOut)
	}
	if got := heldEntries(); len(got) != 1 {
		t.Errorf("reconciliation dropped the fresh entry: %+v", got)
	}
}

// TestFreshDeactivationBeatsTheListingLag is the same lag in the other
// direction: ARM keeps listing a role for a minute or two after it is given up,
// and believing that listing would report access the user has already dropped.
func TestFreshDeactivationBeatsTheListingLag(t *testing.T) {
	elig, active, entry := lagFixtures(t)
	f := &fakeARM{t: t, eligibilities: []armclient.Eligibility{elig}, activated: []armclient.Assignment{active}}
	f.install()
	entry.Status = recordRevoked
	writeRecord("contoso", []recordEntry{entry})

	out, errOut, err := runCmd(t, "status", "-c", "contoso", "--wait")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if strings.Contains(out, "Cost Management Contributor") {
		t.Errorf("a role given up here came back from ARM's stale listing:\n%s", out)
	}
	if strings.Contains(errOut, "activated elsewhere") {
		t.Errorf("this machine's own deactivation was reported as someone else's activation: %q", errOut)
	}
	if got := heldEntries(); len(got) != 0 {
		t.Errorf("reconciliation put the revoked role back: %+v", got)
	}
}

// TestCeilingLetsAzureWin: the record's precedence is bounded, not absolute.
// Once the ceiling is reached ARM is the authority again, in both directions.
func TestCeilingLetsAzureWin(t *testing.T) {
	t.Run("an activation ARM never confirms is dropped and reported", func(t *testing.T) {
		elig, _, entry := lagFixtures(t)
		f := &fakeARM{t: t, eligibilities: []armclient.Eligibility{elig}}
		f.install()
		writeRecord("contoso", []recordEntry{entry})
		pastCeiling(t)

		out, errOut, err := runCmd(t, "status", "-c", "contoso", "--wait")
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if strings.Contains(out, "Cost Management Contributor") {
			t.Errorf("the record outranked ARM past the ceiling:\n%s", out)
		}
		if !strings.Contains(errOut, "no longer held") {
			t.Errorf("the drop must be reported, not silent: %q", errOut)
		}
		if !strings.Contains(errOut, "Cost Management Contributor") {
			t.Errorf("the report must name the role: %q", errOut)
		}
	})

	t.Run("a deactivation ARM still lists comes back", func(t *testing.T) {
		elig, active, entry := lagFixtures(t)
		f := &fakeARM{t: t, eligibilities: []armclient.Eligibility{elig}, activated: []armclient.Assignment{active}}
		f.install()
		entry.Status = recordRevoked
		writeRecord("contoso", []recordEntry{entry})
		pastCeiling(t)

		out, _, err := runCmd(t, "status", "-c", "contoso", "--wait")
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if !strings.Contains(out, "Cost Management Contributor") {
			t.Errorf("an expired tombstone kept hiding a role ARM reports as held:\n%s", out)
		}
	})
}

// TestListingLagIsSettledByTheRequestNotAClock is the fix for the worst
// observed failure: on a day when ARM's per-scope listing ran 4.5 minutes
// behind, the three-minute grace expired mid-lag and `status` printed "No roles
// are currently activated." three times over while az rest showed both roles
// held — and said nothing about dropping them. No constant can be right here;
// the role's own schedule request can, and it says Provisioned immediately.
func TestListingLagIsSettledByTheRequestNotAClock(t *testing.T) {
	elig, _, entry := lagFixtures(t)
	f := &fakeARM{t: t, eligibilities: []armclient.Eligibility{elig}, putStatus: "Provisioned"}
	f.install()
	// Ten minutes: far past any fixed window pimctl might have chosen, and far
	// past the 4.5-minute lag actually observed.
	entry.WrittenAt = time.Now().Add(-10 * time.Minute)
	writeRecord("contoso", []recordEntry{entry})

	out, errOut, err := runCmd(t, "status", "-c", "contoso", "--wait")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if strings.Contains(out, "No roles are currently activated") {
		t.Fatalf("status reported an empty tenant while the request says provisioned:\n%s", out)
	}
	if !strings.Contains(out, "Cost Management Contributor") {
		t.Errorf("a provisioned role was dropped because the listing lagged:\n%s", out)
	}
	if strings.Contains(errOut, "no longer held") {
		t.Errorf("a provisioned role must not be reported as lost: %q", errOut)
	}
	if got := heldEntries(); len(got) != 1 {
		t.Errorf("reconciliation dropped an entry ARM says is provisioned: %+v", got)
	}
	// The row survived on evidence, not on a timer that had not run out yet.
	if f.requestGetCount() == 0 {
		t.Error("pimctl kept the row without asking ARM about the request")
	}
}

// TestRevokedRequestDropsTheEntryAndSaysSo: the same evidence has to be able to
// end an entry, or the record would only ever grow. A drop is never silent —
// silence is what made the original failure so hard to see.
func TestRevokedRequestDropsTheEntryAndSaysSo(t *testing.T) {
	elig, _, entry := lagFixtures(t)
	f := &fakeARM{t: t, eligibilities: []armclient.Eligibility{elig}, putStatus: "Revoked"}
	f.install()
	entry.WrittenAt = time.Now().Add(-time.Minute)
	writeRecord("contoso", []recordEntry{entry})

	out, errOut, err := runCmd(t, "status", "-c", "contoso", "--wait")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if strings.Contains(out, "Cost Management Contributor") {
		t.Errorf("a revoked request must not keep its row:\n%s", out)
	}
	if !strings.Contains(errOut, "no longer held") || !strings.Contains(errOut, "Cost Management Contributor") {
		t.Errorf("the drop must be named on stderr: %q", errOut)
	}
	if got := heldEntries(); len(got) != 0 {
		t.Errorf("a revoked entry survived reconciliation: %+v", got)
	}
}

// TestCeilingExpiryDropsTheEntryAndSaysSo: the ceiling is the backstop for an
// entry that cannot be checked at all. It still may not expire in silence.
func TestCeilingExpiryDropsTheEntryAndSaysSo(t *testing.T) {
	elig, _, entry := lagFixtures(t)
	f := &fakeARM{t: t, eligibilities: []armclient.Eligibility{elig}}
	f.install()
	writeRecord("contoso", []recordEntry{entry})
	pastCeiling(t)

	out, errOut, err := runCmd(t, "status", "-c", "contoso", "--wait")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if strings.Contains(out, "Cost Management Contributor") {
		t.Errorf("an entry past the ceiling must not outrank ARM:\n%s", out)
	}
	if !strings.Contains(errOut, "no longer held") {
		t.Errorf("expiry at the ceiling must be reported, not silent: %q", errOut)
	}
}

// TestConfirmedRowKeepsItsAgeAndLosesTheMarker: reconciliation used to re-stamp
// WrittenAt from every listed row, so a role confirmed hours ago looked freshly
// activated and kept the "~ just activated here" marker for ever.
func TestConfirmedRowKeepsItsAgeAndLosesTheMarker(t *testing.T) {
	elig, active, entry := lagFixtures(t)
	f := &fakeARM{
		t: t, eligibilities: []armclient.Eligibility{elig},
		activated: []armclient.Assignment{active},
	}
	f.install()
	written := time.Now().Add(-time.Minute).Truncate(time.Second)
	entry.WrittenAt = written
	writeRecord("contoso", []recordEntry{entry})

	if _, _, err := runCmd(t, "status", "-c", "contoso", "--wait"); err != nil {
		t.Fatalf("status: %v", err)
	}

	got := heldEntries()
	if len(got) != 1 {
		t.Fatalf("the confirmed role left %d entries, want 1: %+v", len(got), got)
	}
	if !got[0].Listed {
		t.Error("an entry ARM listed must be marked as listed")
	}
	if !got[0].WrittenAt.Truncate(time.Second).Equal(written) {
		t.Errorf("reconciliation re-stamped WrittenAt: %v, want %v", got[0].WrittenAt, written)
	}

	// And the next instant render carries no "just activated" marker.
	rows := localActiveRows(&runContext{Sessions: []*session{{
		Context: "contoso", Token: &azauth.Token{Context: "contoso"},
	}}})
	if len(rows) != 1 {
		t.Fatalf("the record produced %d rows, want 1", len(rows))
	}
	if rows[0].State == RowConfirming {
		t.Error("a role ARM has confirmed must not still read as just activated")
	}
	cmd := NewRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	printActiveTable(cmd, rows)
	if strings.Contains(out.String(), "~") {
		t.Errorf("the confirming marker outlived the confirmation:\n%s", out.String())
	}
}

// checkRecordEntry asserts the fields an entry needs to be useful on its own:
// enough identity to match a role, a window that can expire, a scope type to
// render, and an honest account of where its start time came from.
func checkRecordEntry(t *testing.T, e recordEntry) {
	t.Helper()
	if e.Scope == "" || e.RoleDefinitionID == "" || e.Key == "" {
		t.Errorf("entry is missing identity fields: %+v", e)
	}
	if e.End.IsZero() {
		t.Errorf("entry has no end time, so it can never be pruned: %+v", e)
	}
	// The scope type is what `status` renders in its TYPE column and what tells
	// a subscription from a management group; it was written empty and only
	// filled in later, by a listing that might never come.
	if e.ScopeType == "" {
		t.Errorf("entry has no scope type: %+v", e)
	}
	// The start must be the window ARM opened, not the moment pimctl got round
	// to writing the file.
	if e.StartSource != startFromARM {
		t.Errorf("entry did not take its start from ARM: %+v", e)
	}
	if e.Start.IsZero() || e.Start.After(e.End) {
		t.Errorf("entry has a nonsensical window: %+v", e)
	}
}

// TestRecordSaysWhereItsStartTimeCameFrom: when ARM does not report a start,
// the record still needs one, and marks it as this machine's guess rather than
// passing off a local clock reading as ARM's answer.
func TestRecordSaysWhereItsStartTimeCameFrom(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	end := time.Now().Add(time.Hour)
	armStart := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	scope := "/providers/Microsoft.Management/managementGroups/contoso-prod"
	roleDef := "/providers/Microsoft.Authorization/roleDefinitions/" + costGUID

	recordActivations([]result{{
		Context: "contoso", Role: "Cost Management Contributor", Scope: scope,
		ScopeName: "Contoso landing zones", ScopeType: "ManagementGroup",
		RoleDefinitionID: roleDef, Outcome: OutcomeActivated,
		Since: &armStart, Until: &end,
	}})
	got := readRecord("contoso")
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	if got[0].StartSource != startFromARM || !got[0].Start.Equal(armStart) {
		t.Errorf("an ARM start time was not used verbatim: %+v", got[0])
	}
	if got[0].ScopeType != "ManagementGroup" {
		t.Errorf("the scope type was lost: %+v", got[0])
	}

	// Without one — an already-active role ARM told us nothing about — the
	// entry falls back and says so.
	recordActivations([]result{{
		Context: "contoso", Role: "Reader", Scope: scope,
		RoleDefinitionID: "/providers/Microsoft.Authorization/roleDefinitions/" + contribGUID,
		Outcome:          OutcomeAlreadyActive, Until: &end,
	}})
	for _, e := range readRecord("contoso") {
		if e.Role != "Reader" {
			continue
		}
		if e.StartSource != startFromLocal {
			t.Errorf("a guessed start time must be labelled as one: %+v", e)
		}
		if time.Since(e.Start) > time.Minute {
			t.Errorf("the fallback start should be about now: %+v", e)
		}
	}
}

// TestDebugNamesTheConfirmationStep: the request read-backs are ARM calls on
// the critical path of `status`, so --debug must attribute them instead of
// letting them swell "(other)".
func TestDebugNamesTheConfirmationStep(t *testing.T) {
	elig, _, entry := lagFixtures(t)
	f := &fakeARM{t: t, eligibilities: []armclient.Eligibility{elig}, putStatus: "Provisioned"}
	f.install()
	entry.WrittenAt = time.Now().Add(-time.Minute)
	writeRecord("contoso", []recordEntry{entry})

	_, errOut, err := runCmd(t, "status", "-c", "contoso", "--wait", "--debug")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(errOut, "confirm 1 activation(s) against their requests") {
		t.Errorf("the confirmation step is unnamed in the breakdown:\n%s", errOut)
	}
}
