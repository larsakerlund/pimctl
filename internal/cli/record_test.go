// Tests for record.go: the on-disk round trip, pruning, and what a corrupt or
// wrong-version file means. Reconciliation against Azure is tested in
// reconcile_test.go.

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/larsakerlund/pimctl/internal/armclient"
	"github.com/larsakerlund/pimctl/internal/store"
)

func mkRecordEntry(role, leaf string, endsIn time.Duration) recordEntry {
	scope := "/providers/Microsoft.Management/managementGroups/" + leaf
	e := recordEntry{
		Context:          "contoso",
		Scope:            scope,
		RoleDefinitionID: "/providers/Microsoft.Authorization/roleDefinitions/" + contribGUID,
		Role:             role,
		ScopeName:        "Contoso landing zones",
		ScopeType:        "ManagementGroup",
		Start:            time.Now(),
		End:              time.Now().Add(endsIn),
		Status:           string(OutcomeActivated),
		WrittenAt:        time.Now(),
	}
	e.Key = recordKey(e.Context, e.Scope, e.RoleDefinitionID)
	return e
}

func TestRecordRoundTrip(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	if got := readRecord(testOwner("contoso")); len(got) != 0 {
		t.Fatalf("a fresh state dir should hold no record, got %d", len(got))
	}

	want := mkRecordEntry("Cost Management Contributor", "contoso-prod", time.Hour)
	writeRecord(testOwner("contoso"), []recordEntry{want})

	got := readRecord(testOwner("contoso"))
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	if got[0].Role != want.Role || got[0].Scope != want.Scope || got[0].Key != want.Key {
		t.Fatalf("round trip lost data: %+v", got[0])
	}
	if len(readRecord(testOwner("globex"))) != 0 {
		t.Error("the record must be per-context")
	}
}

// TestRecordPrunesExpiredEntries: an activation whose window has closed is not
// something we hold, so it must never be shown or persisted.
func TestRecordPrunesExpiredEntries(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	live := mkRecordEntry("Cost Management Contributor", "contoso-prod", time.Hour)
	expired := mkRecordEntry("Owner", "contoso-test", -time.Minute)
	writeRecord(testOwner("contoso"), []recordEntry{live, expired})

	got := readRecord(testOwner("contoso"))
	if len(got) != 1 {
		t.Fatalf("got %d entries, want only the live one", len(got))
	}
	if got[0].Role != "Cost Management Contributor" {
		t.Errorf("kept the wrong entry: %s", got[0].Role)
	}

	// An entry with no end time never expires: a deactivation removes it.
	noEnd := mkRecordEntry("Owner", "contoso-qa", time.Hour)
	noEnd.End = time.Time{}
	writeRecord(testOwner("contoso"), []recordEntry{noEnd})
	if len(readRecord(testOwner("contoso"))) != 1 {
		t.Error("an entry with no end time should be kept")
	}
}

func TestRecordActivationsAndForget(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	end := time.Now().Add(time.Hour)
	scope := "/providers/Microsoft.Management/managementGroups/contoso-prod"
	roleDef := "/providers/Microsoft.Authorization/roleDefinitions/" + contribGUID

	recordActivations([]result{
		{
			Owner: testOwner("contoso"), Context: "contoso", Role: "Contributor", Scope: scope,
			RoleDefinitionID: roleDef, ScopeName: "Contoso landing zones",
			Outcome: OutcomeActivated, Until: &end, RequestID: "/req/1",
		},
		// A failure must not be recorded as something we hold.
		{
			Owner: testOwner("contoso"), Context: "contoso", Role: "Owner", Scope: scope,
			RoleDefinitionID: "/providers/Microsoft.Authorization/roleDefinitions/owner",
			Outcome:          OutcomeFailed,
		},
	})

	got := readRecord(testOwner("contoso"))
	if len(got) != 1 {
		t.Fatalf("got %d entries, want only the activated one", len(got))
	}
	if got[0].Role != "Contributor" || got[0].RequestID != "/req/1" {
		t.Fatalf("entry = %+v", got[0])
	}

	// Re-activating the same role updates rather than duplicating.
	recordActivations([]result{{
		Owner: testOwner("contoso"), Context: "contoso", Role: "Contributor", Scope: scope,
		RoleDefinitionID: roleDef, Outcome: OutcomeAlreadyActive, Until: &end,
	}})
	if got := readRecord(testOwner("contoso")); len(got) != 1 {
		t.Fatalf("re-activation duplicated the entry: %d", len(got))
	}

	// Deactivating stops it being held, and leaves a tombstone so ARM's
	// lagging listing cannot put it back.
	forgetActivations([]result{{
		Owner: testOwner("contoso"), Context: "contoso", Role: "Contributor", Scope: scope,
		RoleDefinitionID: roleDef, Outcome: OutcomeDeactivated,
	}})
	if got := heldEntries(); len(got) != 0 {
		t.Fatalf("the record still holds the role after deactivation: %+v", got)
	}
	tomb := readRecord(testOwner("contoso"))
	if len(tomb) != 1 || !tomb[0].Revoked() {
		t.Fatalf("deactivation left no tombstone: %+v", tomb)
	}

	// Once the ceiling is reached, the tombstone goes too.
	pastCeiling(t)
	if got := readRecord(testOwner("contoso")); len(got) != 0 {
		t.Fatalf("an expired tombstone survived: %+v", got)
	}
}

// TestReconcileDropsWhatAzureDoesNotConfirm is the correctness guarantee: the
// record is additive to ARM and never outranks it.
func TestReconcileDropsWhatAzureDoesNotConfirm(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	// This is about what ARM's answer is worth once it has had time to be
	// right; the propagation window has its own tests.
	pastCeiling(t)

	stale := mkRecordEntry("Owner", "contoso-test", time.Hour)
	writeRecord(testOwner("contoso"), []recordEntry{stale})

	// Azure reports a different role entirely: one activated in the portal.
	end := time.Now().Add(30 * time.Minute)
	portal := armclient.Assignment{ID: "/x"}
	portal.Properties.AssignmentType = "Activated"
	portal.Properties.Scope = "/providers/Microsoft.Management/managementGroups/contoso-prod"
	portal.Properties.RoleDefinitionID = "/providers/Microsoft.Authorization/roleDefinitions/" + costGUID
	portal.Properties.EndDateTime = &end
	portal.Properties.ExpandedProperties.RoleDefinition = armclient.Named{DisplayName: "Cost Management Contributor"}
	portal.Properties.ExpandedProperties.Scope = armclient.Named{
		DisplayName: "Contoso landing zones", Type: "managementgroup",
	}

	reconcileRecord(testOwner("contoso"), []activeRow{{Context: "contoso", Assignment: portal}}, nil, nil)

	got := readRecord(testOwner("contoso"))
	if len(got) != 1 {
		t.Fatalf("got %d entries, want exactly what Azure reported", len(got))
	}
	if got[0].Role != "Cost Management Contributor" {
		t.Errorf("reconciliation kept the wrong entry: %s", got[0].Role)
	}
	if got[0].Role == "Owner" {
		t.Error("an entry Azure did not confirm survived reconciliation")
	}

	// Reconciling against nothing empties the record.
	reconcileRecord(testOwner("contoso"), nil, nil, nil)
	if got := readRecord(testOwner("contoso")); len(got) != 0 {
		t.Errorf("reconciling against an empty listing left %d entries", len(got))
	}
}

func TestReconcileIgnoresOtherContexts(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	writeRecord(testOwner("globex"), []recordEntry{mkRecordEntry("Owner", "contoso-test", time.Hour)})

	// A fan-out that returned only contoso's rows must not empty globex.
	reconcileRecord(testOwner("contoso"), nil, nil, nil)
	if got := readRecord(testOwner("globex")); len(got) != 1 {
		t.Errorf("reconciling contoso disturbed globex: %d entries", len(got))
	}
}

func TestCorruptRecordIsAnEmptyRecord(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	writeRecord(testOwner("contoso"), []recordEntry{mkRecordEntry("Owner", "contoso-test", time.Hour)})

	path, err := recordPath(testOwner("contoso"))
	if err != nil {
		t.Fatal(err)
	}
	if err = writeFile(path, "not json"); err != nil {
		t.Fatal(err)
	}
	if got := readRecord(testOwner("contoso")); len(got) != 0 {
		t.Errorf("a corrupt record should read as empty, got %d entries", len(got))
	}
}

func TestStateDirHonoursXDG(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/tmp/xdg-state-test")
	got, err := stateDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != "/tmp/xdg-state-test/pimctl" {
		t.Errorf("StateDir() = %q", got)
	}
	// The record is a note of something that happened, not a re-derivable
	// copy, so it must not land in the cache directory by default.
	t.Setenv("XDG_STATE_HOME", "")
	got, err = stateDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, "/.local/state/pimctl") {
		t.Errorf("StateDir() = %q, want ~/.local/state/pimctl", got)
	}
}

// TestReconcileKeepsEntriesAtUnconfirmedScopes: a scope whose listing was cut
// short is unknown, not empty. Dropping its entries would silently under-report
// what the user holds, which for an access tool is worse than a slow answer.
func TestReconcileKeepsEntriesAtUnconfirmedScopes(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	pastCeiling(t)

	held := mkRecordEntry("Owner", "contoso-test", time.Hour)
	writeRecord(testOwner("contoso"), []recordEntry{held})

	// ARM returned nothing, but contoso-test timed out rather than answering.
	// Scopes are matched by id: the printed label is a rendering of this, never
	// the identity itself.
	reconcileRecord(
		testOwner("contoso"),
		nil,
		[]activationScope{{Context: "contoso", ID: "/providers/Microsoft.Management/managementGroups/contoso-test"}},
		nil,
	)

	got := readRecord(testOwner("contoso"))
	if len(got) != 1 {
		t.Fatalf("an entry at an unconfirmed scope was dropped: %d entries left", len(got))
	}
	if got[0].Role != "Owner" {
		t.Errorf("kept the wrong entry: %s", got[0].Role)
	}

	// Once that scope answers cleanly, the stale entry goes.
	reconcileRecord(testOwner("contoso"), nil, nil, nil)
	if got := readRecord(testOwner("contoso")); len(got) != 0 {
		t.Errorf("a confirmed-empty scope should have dropped the entry, %d left", len(got))
	}
}

// TestAlreadyActiveRerunKeepsTheRequestID is the fix for a defect found at the
// wire: running `up` again on a role that is already active took the ALREADY
// ACTIVE path, which sends no request and so has no request id, and the emptier
// entry replaced the one written when the role was actually activated. With the
// request id gone the confirmation step skips the role — removing exactly the
// evidence that keeps `status` right through ARM's listing lag, which the
// second `up` had just proved was still running.
func TestAlreadyActiveRerunKeepsTheRequestID(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	scope := "/providers/Microsoft.Management/managementGroups/contoso-prod"
	roleDef := "/providers/Microsoft.Authorization/roleDefinitions/" + costGUID
	end := time.Now().Add(time.Hour)
	armStart := time.Now().Add(-30 * time.Minute).UTC().Truncate(time.Second)

	// What `up` writes when it really activates: a request id, ARM's own start.
	recordActivations([]result{{
		Owner: testOwner("contoso"), Context: "contoso", Role: "Cost Management Contributor", Scope: scope,
		ScopeName: "Contoso landing zones", ScopeType: "ManagementGroup",
		RoleDefinitionID: roleDef, Outcome: OutcomeActivated,
		RequestID: scope + "/providers/Microsoft.Authorization/roleAssignmentScheduleRequests/req-1",
		Since:     &armStart, Until: &end,
	}})

	// What a rerun writes: no request id, no ARM start, ALREADY ACTIVE.
	recordActivations([]result{{
		Owner: testOwner("contoso"), Context: "contoso", Role: "Cost Management Contributor", Scope: scope,
		ScopeName: "Contoso landing zones", ScopeType: "ManagementGroup",
		RoleDefinitionID: roleDef, Outcome: OutcomeAlreadyActive, Until: &end,
	}})

	got := readRecord(testOwner("contoso"))
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1: %+v", len(got), got)
	}
	if got[0].RequestID == "" {
		t.Error("the rerun erased the request id, so the role can no longer be confirmed")
	}
	if got[0].StartSource != startFromARM || !got[0].Start.Equal(armStart) {
		t.Errorf("the rerun replaced ARM's start time with this machine's clock: %+v", got[0])
	}
	if got[0].Status != string(OutcomeActivated) {
		t.Errorf("status = %q; ALREADY ACTIVE says less than the outcome it replaced", got[0].Status)
	}
}

// TestNewActivationReplacesTheOldEntry is the other half: a request id that
// differs from the recorded one is a genuinely new activation, whose window and
// unlisted state are its own. Merging into the old entry would leave it marked
// as listed and let reconciliation drop a role that is held.
func TestNewActivationReplacesTheOldEntry(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	scope := "/providers/Microsoft.Management/managementGroups/contoso-prod"
	roleDef := "/providers/Microsoft.Authorization/roleDefinitions/" + costGUID
	first := mkRecordEntry("Cost Management Contributor", "contoso-prod", time.Hour)
	first.RoleDefinitionID = roleDef
	first.RequestID = "req-1"
	first.Listed = true
	first.Key = recordKey(first.Context, first.Scope, first.RoleDefinitionID)
	writeRecord(testOwner("contoso"), []recordEntry{first})

	end := time.Now().Add(2 * time.Hour)
	recordActivations([]result{{
		Owner: testOwner("contoso"), Context: "contoso", Role: "Cost Management Contributor", Scope: scope,
		ScopeName: "Contoso landing zones", ScopeType: "ManagementGroup",
		RoleDefinitionID: roleDef, Outcome: OutcomeActivated,
		RequestID: "req-2", Until: &end,
	}})

	got := readRecord(testOwner("contoso"))
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1: %+v", len(got), got)
	}
	if got[0].RequestID != "req-2" {
		t.Errorf("request id = %q, want the new one", got[0].RequestID)
	}
	if got[0].Listed {
		t.Error("a new activation is not listed yet; carrying that over would let reconciliation drop it")
	}
}

// TestRerunStillConfirmsAgainstTheRequest walks the whole defect through the
// commands: activate, re-run into ALREADY ACTIVE, then ask for status with the
// listing still empty. The role must survive, and it can only survive by
// pimctl reading its schedule request back — so the fake counts the GET.
func TestRerunStillConfirmsAgainstTheRequest(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()[:1], putStatus: "Provisioned"}
	f.install()

	if _, _, err := runCmd(t, "up", "-c", "contoso", "--all", "-j", "x", "-y"); err != nil {
		t.Fatalf("up: %v", err)
	}
	// ARM now answers every activation with "already exists", which is what a
	// second `up` on a held role gets.
	f.setPutErr(func(string) (int, string) {
		return 400, `{"error":{"code":"RoleAssignmentExists","message":"The Role assignment already exists."}}`
	})
	out, _, err := runCmd(t, "up", "-c", "contoso", "--all", "-j", "x", "-y")
	if err != nil {
		t.Fatalf("rerun: %v (%s)", err, out)
	}
	if !strings.Contains(out, "ALREADY ACTIVE") {
		t.Fatalf("the rerun did not take the already-active path:\n%s", out)
	}

	before := f.requestGetCount()
	// The listing stays empty, as it is during ARM's propagation lag, so the
	// row can only be kept on the strength of its request.
	statusOut, errOut, err := runCmd(t, "status", "-c", "contoso", "--wait")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if f.requestGetCount() == before {
		t.Error("the confirmation step skipped the role, which is what losing the request id caused")
	}
	if !strings.Contains(statusOut, "Cost Management Contributor") {
		t.Errorf("the role was dropped while its request says provisioned:\n%s", statusOut)
	}
	if strings.Contains(errOut, "no longer held") {
		t.Errorf("a held role was reported as lost: %q", errOut)
	}
}

// TestRecordLivesInTheContextStore: what this machine activated in a context
// belongs to that context, so the record sits inside its cloudctx store
// and goes away with `cloudctx delete`. A record written before the move is
// migrated the first time the store answers, because a record left behind would
// go on describing roles under a context that no longer exists.
func TestRecordLivesInTheContextStore(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv(envContext, "")

	// A record from before the contract, in pimctl's own state directory.
	dir, err := stateDir()
	if err != nil {
		t.Fatal(err)
	}
	blob, err := json.Marshal(recordFile{
		Version: recordVersion,
		Owner:   testOwner("contoso"),
		Entries: []recordEntry{mkRecordEntry("Cost Management Contributor", "contoso-prod", time.Hour)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(dir, store.DirMode); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(dir, "active-"+testOwner("contoso").FileName()+".json")
	if err = os.WriteFile(oldPath, blob, 0o600); err != nil {
		t.Fatal(err)
	}

	// A cloudctx that reports a store for the context: the record belongs
	// inside it from here on.
	installFakeRunner(t, []string{"contoso"})

	got, err := recordPath(testOwner("contoso"))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(fakeContextStore("contoso"), "pimctl", "active-"+testOwner("contoso").FileName()+".json")
	if got != want {
		t.Errorf("recordPath = %q, want %q", got, want)
	}
	if _, err = os.Stat(oldPath); err == nil {
		t.Error("the record was copied rather than moved; two records for one context disagree eventually")
	}
	entries := readRecord(testOwner("contoso"))
	if len(entries) != 1 || entries[0].Role != "Cost Management Contributor" {
		t.Errorf("the migrated record lost its entries: %+v", entries)
	}

	// The shared az login belongs to no context, so its record stays put.
	bare, err := recordPath(testOwner(""))
	if err != nil {
		t.Fatal(err)
	}
	if bare != filepath.Join(dir, "active-"+testOwner("").FileName()+".json") {
		t.Errorf("the bare record moved to %q; it has no context to move into", bare)
	}
}
