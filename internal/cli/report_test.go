// Tests for what a run prints: the streamed per-role lines, the results table,
// and the summary that replaces the table when every row has already gone past
// on the same screen.

package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/larsakerlund/pimctl/internal/term"
)

// TestStreamProgressIsTerminalOnly: streaming to a pipe would interleave with
// the table that follows and hand a parser two representations of one run.
func TestStreamProgressIsTerminalOnly(t *testing.T) {
	cmd := NewRootCmd()
	sp := term.NewSpinner(io.Discard, "")
	// Tests never run on a TTY, so every case here must be nil.
	if got := streamProgress(cmd, &globalOpts{output: "table"}, 5, sp, scopeLabeler{}); got != nil {
		t.Error("streaming must be off when stderr is not a terminal")
	}
	if got := streamProgress(cmd, &globalOpts{output: outputJSON}, 5, sp, scopeLabeler{}); got != nil {
		t.Error("streaming must be off under -o json")
	}
}

func TestStreamResultLine(t *testing.T) {
	subScope := "/subscriptions/33333333-3333-3333-3333-333333333333"
	scopes := newScopeLabeler([]scopeRef{{Name: "Contoso QA", ID: subScope}})

	var b bytes.Buffer
	streamResult(&b, term.Palette{}, scopes, result{
		Role:    "Cost Management Contributor",
		Scope:   "/providers/Microsoft.Management/managementGroups/contoso-prod",
		Outcome: OutcomeActivated,
	})
	got := b.String()
	for _, want := range []string{"ACTIVATED", "Cost Management Contributor", "contoso-prod"} {
		if !strings.Contains(got, want) {
			t.Errorf("streamed line %q is missing %q", got, want)
		}
	}
	if strings.Count(got, "\n") != 1 {
		t.Errorf("a streamed result must be exactly one line: %q", got)
	}

	// The scope reads the same here as in the tables. It used to print the
	// leaf id, so one subscription appeared as "33333333" on the streamed line
	// and "Contoso QA" in the table three lines below.
	b.Reset()
	streamResult(&b, term.Palette{}, scopes, result{
		Role: "Contributor", Scope: subScope, ScopeName: "Contoso QA",
		Outcome: OutcomeDeactivated,
	})
	if !strings.Contains(b.String(), "Contoso QA") {
		t.Errorf("a streamed line must name the scope the way the tables do: %q", b.String())
	}
	if strings.Contains(b.String(), "@ 33333333 ") {
		t.Errorf("a streamed line must not fall back to the leaf id: %q", b.String())
	}

	// A failure carries its reason on the same line.
	b.Reset()
	streamResult(&b, term.Palette{}, scopes, result{
		Role: "Owner", Scope: "/subscriptions/abc", Outcome: OutcomeFailed,
		Detail: "MfaRule",
	})
	if !strings.Contains(b.String(), "MfaRule") {
		t.Errorf("a streamed failure must carry its reason: %q", b.String())
	}
}

// TestStreamedRunSummarisesInsteadOfRepeatingTheTable: after one line per role
// on the same screen, the table says nothing new — and on a deactivation it
// says it worse, with an "UNTIL -" column that means nothing.
func TestStreamedRunSummarisesInsteadOfRepeatingTheTable(t *testing.T) {
	results := []result{
		{Role: "Contributor", Scope: "/subscriptions/a", ScopeName: "Contoso QA", Outcome: OutcomeDeactivated},
		{Role: "Reader", Scope: "/subscriptions/b", ScopeName: "Contoso Test", Outcome: OutcomeDeactivated},
	}
	scopes := newScopeLabeler(nil)

	cmd := NewRootCmd()
	var streamed bytes.Buffer
	cmd.SetOut(&streamed)
	if err := printResults(cmd, &globalOpts{output: "table"}, results, false, true, scopes); err != nil {
		t.Fatalf("printResults: %v", err)
	}
	if !strings.Contains(streamed.String(), "2 deactivated") {
		t.Errorf("a streamed run must end with a one-line summary, got %q", streamed.String())
	}
	if strings.Contains(streamed.String(), "UNTIL") {
		t.Errorf("the table must not be repeated after streaming:\n%s", streamed.String())
	}

	// Piped output never saw the streamed lines, so it keeps the table.
	cmd = NewRootCmd()
	var piped bytes.Buffer
	cmd.SetOut(&piped)
	if err := printResults(cmd, &globalOpts{output: "table"}, results, false, false, scopes); err != nil {
		t.Fatalf("printResults: %v", err)
	}
	if !strings.Contains(piped.String(), "RESULT") || !strings.Contains(piped.String(), "Contributor") {
		t.Errorf("output that was not streamed must keep the full table:\n%s", piped.String())
	}
}

// TestStreamedSummaryCountsEveryOutcome: a summary that quietly omits a
// category would hide the one row the user has to act on.
func TestStreamedSummaryCountsEveryOutcome(t *testing.T) {
	var b bytes.Buffer
	printResultSummary(&b, []result{
		{Outcome: OutcomeActivated},
		{Outcome: OutcomeActivated},
		{Outcome: OutcomeActivated},
		{Outcome: OutcomeAlreadyActive},
		{Outcome: OutcomeFailed},
	})
	got := strings.TrimSpace(b.String())
	if got != "3 activated, 1 already active, 1 failed" {
		t.Errorf("summary = %q", got)
	}
}

// TestStreamedRunKeepsTheRecoveryAdvice: the claims-challenge command is the
// only way out of that failure and is not in the streamed line.
func TestStreamedRunKeepsTheRecoveryAdvice(t *testing.T) {
	cmd := NewRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := printResults(cmd, &globalOpts{output: "table"}, []result{{
		Role: "Owner", Scope: "/subscriptions/a", Outcome: OutcomeFailed,
		Recovery: "cloudctx exec contoso -- az login --claims-challenge XYZ",
	}}, false, true, newScopeLabeler(nil))
	if err != nil {
		t.Fatalf("printResults: %v", err)
	}
	if !strings.Contains(out.String(), "--claims-challenge XYZ") {
		t.Errorf("the recovery command must survive the summary path:\n%s", out.String())
	}
}

// TestResultJSONCarriesTheScopeLabel closes the last gap in the "one JSON
// vocabulary" promise: `ls` and `status` emit scopeLabel, and until now `up`
// and `down` did not, so a script reading a result had to rebuild the table's
// own wording from the scope id.
func TestResultJSONCarriesTheScopeLabel(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: worstCaseRows()}
	f.install()

	out, _, err := runCmd(t, "up", "-c", "contoso", "--all", "-j", "x", "-y", "-o", "json")
	if err != nil {
		t.Fatalf("up -o json: %v", err)
	}
	var results []struct {
		ScopeName  string `json:"scopeName"`
		ScopeLabel string `json:"scopeLabel"`
		Scope      string `json:"scope"`
	}
	if err := json.Unmarshal([]byte(out), &results); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if len(results) == 0 {
		t.Fatalf("no results:\n%s", out)
	}
	var sawDisambiguated bool
	for _, r := range results {
		if r.ScopeLabel == "" {
			t.Errorf("a result carries no scopeLabel:\n%s", out)
		}
		// worstCaseRows has two management groups sharing a display name, and
		// a management group is always disambiguated: the label must say which.
		if strings.HasPrefix(r.Scope, "/providers/Microsoft.Management/") {
			want := r.ScopeName + " (" + scopeLeaf(r.Scope) + ")"
			if r.ScopeLabel != want {
				t.Errorf("scopeLabel = %q, want %q", r.ScopeLabel, want)
			}
			sawDisambiguated = true
		}
	}
	if !sawDisambiguated {
		t.Fatal("the fixture no longer covers an ambiguous scope, so this proves nothing")
	}
}
