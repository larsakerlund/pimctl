// Tests for the `up`/`activate` command end to end: flag validation before
// any network call, selection, clamping, the exit code each mix of outcomes
// earns, and the JSON keys. The ARM bodies those runs send are pinned in
// request_test.go.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/larsakerlund/pimctl/internal/armclient"
)

func TestActivateBatchTwoRoles(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles(), maxDuration: "PT4H"}
	f.install()
	out, _, err := runCmd(t, "activate", "-c", "contoso",
		"--role", "Cost Management Contributor", "--role", "Resource Policy Contributor",
		"--hours", "1", "--justification", "unit test", "-y")
	if err != nil {
		t.Fatalf("activate: %v\n%s", err, out)
	}
	if len(f.putBodies()) != 2 {
		t.Fatalf("sent %d activation requests, want 2 (the batch path)", len(f.putBodies()))
	}
	for _, body := range f.putBodies() {
		props := mustObject(t, body, "properties")
		if props["requestType"] != "SelfActivate" {
			t.Errorf("requestType = %v", props["requestType"])
		}
		if props["principalId"] != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
			t.Errorf("principalId = %v", props["principalId"])
		}
		if props["justification"] != "unit test" {
			t.Errorf("justification = %v", props["justification"])
		}
		si := mustObject(t, props, "scheduleInfo")
		exp := mustObject(t, si, "expiration")
		if exp["duration"] != "PT1H" {
			t.Errorf("duration = %v, want PT1H (--hours 1 under the PT4H maximum)", exp["duration"])
		}
	}
	if strings.Count(out, "ACTIVATED") < 2 {
		t.Errorf("results table does not report both activations:\n%s", out)
	}
}

func TestActivateClampsToPolicyMaximum(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()[:1], maxDuration: "PT1H"}
	f.install()
	out, _, err := runCmd(t, "activate", "-c", "contoso", "--role", "Cost Management", "--hours", "8", "-j", "x", "-y")
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	props := mustObject(t, f.putBodies()[0], "properties")
	exp := mustObject(t, mustObject(t, props, "scheduleInfo"), "expiration")
	if exp["duration"] != "PT1H" {
		t.Fatalf("duration = %v, want the PT1H policy maximum", exp["duration"])
	}
	if !strings.Contains(out, "capped to the policy maximum PT1H") {
		t.Errorf("the clamp was not reported to the user:\n%s", out)
	}
}

func TestActivateAlreadyActiveIsNotAFailure(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()[:1]}
	f.setPutErr(func(string) (int, string) {
		return 400, `{"error":{"code":"RoleAssignmentExists","message":"The Role assignment already exists."}}`
	})
	f.install()
	out, _, err := runCmd(t, "activate", "-c", "contoso", "--all", "-j", "x", "-y")
	if err != nil {
		t.Fatalf("an already-active role must not fail the run: %v", err)
	}
	if !strings.Contains(out, "ALREADY ACTIVE") {
		t.Errorf("outcome not reported:\n%s", out)
	}
}

func TestActivatePendingApprovalExitsTwo(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()[:1], putStatus: "PendingApproval"}
	f.install()
	out, _, err := runCmd(t, "activate", "-c", "contoso", "--all", "-j", "x", "-y")
	if err == nil {
		t.Fatal("a pending approval should produce a non-zero exit")
	}
	if got := ExitCode(err); got != ExitPending {
		t.Errorf("exit code = %d, want 2", got)
	}
	if !strings.Contains(out, "PENDING APPROVAL") {
		t.Errorf("outcome not reported:\n%s", out)
	}
}

func TestActivateFailureExitsOne(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()[:1]}
	f.setPutErr(func(string) (int, string) {
		return 400, `{"error":{"code":"RoleAssignmentRequestPolicyValidationFailed","message":"The following policy rules failed: [\"MfaRule\"]"}}`
	})
	f.install()
	out, _, err := runCmd(t, "activate", "-c", "contoso", "--all", "-j", "x", "-y")
	if err == nil {
		t.Fatal("a failed activation must produce a non-zero exit")
	}
	if got := ExitCode(err); got != ExitFailed {
		t.Errorf("exit code = %d, want 1", got)
	}
	if !strings.Contains(out, "MfaRule") {
		t.Errorf("the failing policy rule was not shown:\n%s", out)
	}
}

// TestActivateWithoutTTYExplainsItself covers the degradation requirement: with
// no selection flags and no terminal, pimctl must say what to pass instead.
func TestActivateWithoutTTYExplainsItself(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()}
	f.install()
	_, _, err := runCmd(t, "activate", "-c", "contoso")
	if err == nil {
		t.Fatal("expected an error when stdin is not a terminal")
	}
	msg := err.Error()
	if !strings.Contains(msg, "stdin is not a terminal") {
		t.Errorf("error does not say why: %q", msg)
	}
	for _, flag := range []string{"--role", "--scope", "--all", "--preset"} {
		if !strings.Contains(msg, flag) {
			t.Errorf("error does not mention %s: %q", flag, msg)
		}
	}
}

func TestActivateWithoutYesAndWithoutTTY(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()}
	f.install()
	_, _, err := runCmd(t, "activate", "-c", "contoso", "--all", "-j", "x")
	if err == nil || !strings.Contains(err.Error(), "-y") {
		t.Fatalf("expected a confirmation-prompt error mentioning -y, got %v", err)
	}
}

func TestConflictingSelectionFlags(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()}
	f.install()
	if _, _, err := runCmd(t, "activate", "-c", "contoso", "--all", "--role", "x", "-y"); err == nil {
		t.Error("--all with --role should be rejected")
	}
	if _, _, err := runCmd(t, "activate", "-c", "contoso", "--preset", "p", "--all", "-y"); err == nil {
		t.Error("--preset with --all should be rejected")
	}
}

// TestActivateSavePresetFailureStillPrintsResults covers the case where ARM has
// already granted every role but the preset file cannot be written. The user
// must still be told what activated; hiding that behind a filesystem error
// invites a blind retry of an activation that already succeeded.
func TestActivateSavePresetFailureStillPrintsResults(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()}
	f.install()

	// Point the config dir at a path that cannot be created: a regular file.
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocked")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", blocker)

	out, errOut, err := runCmd(t, "activate", "-c", "contoso", "--all", "-j", "x", "--save-preset", "daily", "-y")
	if err != nil {
		t.Fatalf("a preset-save failure must not fail a successful activation: %v", err)
	}
	if strings.Count(out, "ACTIVATED") < 2 {
		t.Errorf("activation results were hidden by the preset error:\n%s", out)
	}
	if !strings.Contains(errOut, "could not save preset") {
		t.Errorf("the preset failure was not reported: %q", errOut)
	}
}

// TestActivatePollTimeoutExitsOne pins HIGH-5: a request that never reaches a
// terminal status is a failure (exit 1), not "pending approval" (exit 2).
func TestActivatePollTimeoutExitsOne(t *testing.T) {
	tm := defaultTimeouts()
	tm.poll = 10 * time.Millisecond
	installTimeouts(t, tm)

	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()[:1], putStatus: "PendingProvisioning"}
	f.install()
	out, _, err := runCmd(t, "activate", "-c", "contoso", "--all", "-j", "x", "-y")
	if err == nil {
		t.Fatal("a request stuck below a terminal status must exit non-zero")
	}
	if got := ExitCode(err); got != ExitFailed {
		t.Errorf("exit code = %d, want 1 — exit 2 means pending approval, which this is not", got)
	}
	if !strings.Contains(out, "STILL PENDING") || !strings.Contains(out, "access is not held") {
		t.Errorf("the outcome must say the access is not held:\n%s", out)
	}
	if strings.Contains(err.Error(), "waiting for approval") {
		t.Errorf("a timeout must not be described as an approval queue: %q", err.Error())
	}
}

// TestInterruptedRunReportsHonestly pins MED-10: a cancelled run must not claim
// it waited out the poll timeout, and roles it never got to must be reported as
// skipped rather than as ARM failures.
func TestInterruptedRunReportsHonestly(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles(), putStatus: "PendingProvisioning"}
	f.install()

	root := newRootCmd(testDeps())
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs([]string{"activate", "-c", "contoso", "--all", "-j", "x", "-y"})

	// Cancel while the activations are polling.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	err := root.ExecuteContext(ctx)

	if err == nil {
		t.Fatal("an interrupted run must exit non-zero")
	}
	text := out.String()
	if strings.Contains(text, "still PendingProvisioning after 2m0s") {
		t.Errorf("an interrupted run must not claim it waited out the poll timeout:\n%s", text)
	}
	if !strings.Contains(text, "ABORTED") && !strings.Contains(text, "SKIPPED") {
		t.Errorf("interrupted roles were not marked as such:\n%s", text)
	}
	if strings.Contains(text, "context canceled") {
		t.Errorf("cancellation leaked as a raw error, indistinguishable from an ARM rejection:\n%s", text)
	}
}

func TestInterruptOutcomesCountAsFailure(t *testing.T) {
	if got := exitCodeFor([]result{{Outcome: OutcomeAborted}}); got != ExitFailed {
		t.Errorf("ABORTED exit code = %d, want 1", got)
	}
	if got := exitCodeFor([]result{{Outcome: OutcomeSkipped}}); got != ExitFailed {
		t.Errorf("SKIPPED exit code = %d, want 1", got)
	}
	if !(result{Outcome: OutcomeAborted}).IsInterrupted() {
		t.Error("ABORTED should report as interrupted")
	}
}

// TestActivateJSONUsesLowerCamelKeys pins MED-6: one jq vocabulary across
// list, status and activate.
func TestActivateJSONUsesLowerCamelKeys(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()[:1]}
	f.install()
	out, _, err := runCmd(t, "activate", "-c", "contoso", "--all", "-j", "x", "-y", "-o", "json")
	if err != nil {
		t.Fatalf("activate -o json: %v", err)
	}
	var results []map[string]any
	if err := json.Unmarshal([]byte(out), &results); err != nil {
		t.Fatalf("stdout is not valid JSON (a table may have leaked into it): %v\n%s", err, out)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results", len(results))
	}
	for _, want := range []string{"context", "role", "scopeName", "scope", "outcome"} {
		if _, ok := results[0][want]; !ok {
			t.Errorf("missing lowerCamel key %q; got %v", want, keysOf(results[0]))
		}
	}
	for _, bad := range []string{"Context", "Role", "ScopeName", "RequestID", "Outcome"} {
		if _, ok := results[0][bad]; ok {
			t.Errorf("exported Go field name %q leaked into the JSON", bad)
		}
	}
	if _, ok := results[0]["requestId"]; !ok {
		t.Errorf("requestId missing; got %v", keysOf(results[0]))
	}
}

// TestActivateHoursZeroRejected pins MED-9 end to end.
func TestActivateHoursZeroRejected(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()[:1]}
	f.install()
	_, _, err := runCmd(t, "activate", "-c", "contoso", "--all", "-j", "x", "--hours", "0", "-y")
	if err == nil {
		t.Fatal("--hours 0 must be rejected, not read as 'use the policy maximum'")
	}
	if len(f.putBodies()) != 0 {
		t.Error("nothing should have been sent to ARM")
	}
}

// TestAlreadyActiveKeepsKnownEndTime pins LOW-19: the existing window's end time
// was already fetched during listing, so ALREADY ACTIVE should show it rather
// than a dash that forces a second `pimctl status`.
func TestAlreadyActiveKeepsKnownEndTime(t *testing.T) {
	end := time.Now().Add(42 * time.Minute).Truncate(time.Minute)
	elig := twoLowImpactRoles()[:1]

	a := armclient.Assignment{ID: "/x"}
	a.Properties.AssignmentType = "Activated"
	a.Properties.Scope = elig[0].Properties.Scope
	a.Properties.RoleDefinitionID = elig[0].Properties.RoleDefinitionID
	a.Properties.EndDateTime = &end
	a.Properties.ExpandedProperties.RoleDefinition = armclient.Named{DisplayName: elig[0].RoleName()}
	a.Properties.ExpandedProperties.Scope = armclient.Named{
		DisplayName: "Contoso landing zones",
		Type:        "managementgroup",
	}

	f := &fakeARM{t: t, eligibilities: elig, activated: []armclient.Assignment{a}}
	f.setPutErr(func(string) (int, string) {
		return 400, `{"error":{"code":"RoleAssignmentExists","message":"The Role assignment already exists."}}`
	})
	f.install()

	out, _, err := runCmd(t, "activate", "-c", "contoso", "--all", "-j", "x", "-y")
	if err != nil {
		t.Fatalf("already-active must not fail the run: %v", err)
	}
	if !strings.Contains(out, "ALREADY ACTIVE") {
		t.Fatalf("outcome:\n%s", out)
	}
	if !strings.Contains(out, end.Local().Format("2006-01-02 15:04")) {
		t.Errorf("the known end time was dropped from the UNTIL column:\n%s", out)
	}
}

// TestUpAndDownAreAliasesOfActivateAndDeactivate keeps the new primary verbs and
// the original names behaving identically.
func TestUpAndDownAreAliasesOfActivateAndDeactivate(t *testing.T) {
	for _, verb := range []string{"up", "activate"} {
		f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()[:1]}
		f.install()
		out, _, err := runCmd(t, verb, "-c", "contoso", "--all", "-j", "x", "-y")
		if err != nil {
			t.Fatalf("%s: %v", verb, err)
		}
		if !strings.Contains(out, "ACTIVATED") {
			t.Errorf("%s did not activate:\n%s", verb, out)
		}
		if len(f.putBodies()) != 1 {
			t.Errorf("%s sent %d requests", verb, len(f.putBodies()))
		}
	}

	for _, verb := range []string{"down", "deactivate"} {
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

		f := &fakeARM{t: t, activated: []armclient.Assignment{a}, putStatus: "Revoked"}
		f.install()
		args := []string{verb, "-c", "contoso", "-y"}
		if verb == "deactivate" {
			args = append(args, "--all") // bare `deactivate` still shows the picker.
		}
		out, _, err := runCmd(t, args...)
		if err != nil {
			t.Fatalf("%s: %v", verb, err)
		}
		if !strings.Contains(out, "DEACTIVATED") {
			t.Errorf("%s did not deactivate:\n%s", verb, out)
		}
	}
}

// TestUnattendedSelectionIsCapped guards the blast radius: a loose --role must
// not silently fire at dozens of roles.
func TestUnattendedSelectionIsCapped(t *testing.T) {
	elig := make([]armclient.Eligibility, 0, 12)
	for i := range 12 {
		elig = append(elig, mkElig("Storage Blob Data Contributor", "guid",
			fmt.Sprintf("/subscriptions/sub-%02d", i), fmt.Sprintf("Sub %02d", i), "subscription"))
	}
	f := &fakeARM{t: t, eligibilities: elig}
	f.install()

	_, errOut, err := runCmd(t, "up", "-c", "contoso", "--role", "Storage", "-j", "x", "-y")
	if err == nil {
		t.Fatal("12 roles from one loose --role should be refused without --force")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("the error should name the escape hatch: %v", err)
	}
	if len(f.putBodies()) != 0 {
		t.Error("nothing should have been sent")
	}
	if !strings.Contains(errOut, "matched 12 of 12") {
		t.Errorf("the match report is missing: %q", errOut)
	}

	// --force lets it through.
	f.resetPuts()
	if _, _, err := runCmd(t, "up", "-c", "contoso", "--role", "Storage", "-j", "x", "-y", "--force"); err != nil {
		t.Fatalf("--force: %v", err)
	}
	if len(f.putBodies()) != 12 {
		t.Errorf("--force sent %d, want 12", len(f.putBodies()))
	}
}

func TestJustificationRequiredWhenNotInteractive(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()[:1]}
	f.install()
	_, _, err := runCmd(t, "up", "-c", "contoso", "--all", "-y")
	if err == nil {
		t.Fatal("-j must be required off a TTY: silently reusing the last justification poisons the PIM audit log")
	}
	if !strings.Contains(err.Error(), "audit log") {
		t.Errorf("the error should say why: %v", err)
	}
	if len(f.putBodies()) != 0 {
		t.Error("nothing should have been sent")
	}
}

// TestFlagChecksHappenBeforeAnyNetworkCall: the old code spent the whole fetch
// before telling the user it needed a terminal.
func TestFlagChecksHappenBeforeAnyNetworkCall(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()}
	f.install()
	// Wrap whatever the fake ARM installed, so this records the attempt without
	// replacing the backend the rest of the command would use.
	inner := testDeps().openSessions
	var opened bool
	installSessionOpener(t, func(res resolution, tm *timings, refresh bool) ([]*session, []error, error) {
		opened = true
		return inner(res, tm, refresh)
	})

	if _, _, err := runCmd(t, "up", "-c", "contoso"); err == nil {
		t.Fatal("expected the no-TTY error")
	}
	if opened {
		t.Error("pimctl authenticated before discovering it could not prompt")
	}
}

// TestActivateDoesNotBlockOnTheSlowActiveListing: `up` must reach the plan
// without waiting for the activation listing.
func TestActivateDoesNotBlockOnTheSlowActiveListing(t *testing.T) {
	tm := defaultTimeouts()
	tm.activeBackfill = 100 * time.Millisecond
	installTimeouts(t, tm)
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()[:1], activeDelay: 5 * time.Second}
	f.install()

	start := time.Now()
	out, _, err := runCmd(t, "up", "-c", "contoso", "--all", "-j", "x", "-y")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("up waited %v on the slow listing", elapsed)
	}
	if !strings.Contains(out, "ACTIVATED") {
		t.Errorf("results:\n%s", out)
	}
}

// TestSelectionConflictsFailBeforeAnyNetworkCall: a typo like `up --all --role X`
// used to cost a token mint and a role listing before admitting it could never
// work — and printed a cache notice first, which reads like progress.
func TestSelectionConflictsFailBeforeAnyNetworkCall(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"all with role", []string{"up", "--all", "--role", "Owner", "-j", "x", "-y"}, "--all cannot be combined"},
		{"key with role", []string{"up", "--key", "ab", "--role", "Owner", "-j", "x", "-y"}, "--key cannot be combined"},
		{"preset with all", []string{"up", "--preset", "daily", "--all", "-j", "x", "-y"}, "--preset cannot be combined"},
		{"down all with key", []string{"down", "--all", "--key", "ab", "-y"}, "--all cannot be combined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()}
			f.install()
			// Any ARM call at all is a failure of the point of this test.
			f.srv.Close()

			_, errOut, err := runCmd(t, append(tc.args, "-c", "contoso")...)
			if err == nil {
				t.Fatalf("expected a flag conflict error, got none (stderr %q)", errOut)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestResultJSONCarriesTheKey closes the loop: what `up` reports can be fed
// back to `down` without matching on names.
func TestResultJSONCarriesTheKey(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()}
	f.install()

	out, _, err := runCmd(t, "up", "-c", "contoso", "--all", "-j", "x", "-y", "-o", "json")
	if err != nil {
		t.Fatalf("up -o json: %v", err)
	}
	var results []struct {
		Key   string `json:"key"`
		Until string `json:"until"`
	}
	if err := json.Unmarshal([]byte(out), &results); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	for _, r := range results {
		if r.Key == "" {
			t.Errorf("a result must carry its selection key:\n%s", out)
		}
	}
}
