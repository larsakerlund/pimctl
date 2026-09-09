// Tests for the justification policy: when the prompt is reached at all, and
// what goes out when it is not. The prompt's own drawing and prefill are
// exercised on a real pty by scripts/*.exp, not from here.

package cli

import (
	"testing"

	"github.com/larsakerlund/pimctl/internal/armclient"
)

func planWithJustification(required ...bool) []*planItem {
	items := make([]*planItem, 0, len(required))
	for _, req := range required {
		rules := []string{"MultiFactorAuthentication"}
		if req {
			rules = append(rules, "Justification")
		}
		items = append(items, &planItem{Settings: &armclient.RoleSettings{EnabledRules: rules}})
	}
	return items
}

func TestJustificationDemand(t *testing.T) {
	required, total := justificationDemand(planWithJustification(true, false, true))
	if required != 2 || total != 3 {
		t.Errorf("got %d of %d, want 2 of 3", required, total)
	}
	if required, total := justificationDemand(planWithJustification(false, false)); required != 0 || total != 2 {
		t.Errorf("got %d of %d, want 0 of 2", required, total)
	}
	// A role whose policy could not be read is not counted either way.
	mixed := append(planWithJustification(true), &planItem{})
	if required, total := justificationDemand(mixed); required != 1 || total != 1 {
		t.Errorf("got %d of %d, want 1 of 1 — a role with no policy must not be counted", required, total)
	}
}

// TestResolveJustificationSkipsThePromptWhenNoPolicyWantsOne covers the quiet
// branch: stopping to ask for text nobody reads is the friction that makes a
// tool feel slow.
func TestResolveJustificationSkipsThePromptWhenNoPolicyWantsOne(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// Nothing stored, nothing required: the default goes out silently. If this
	// tried to prompt it would block, since there is no terminal here.
	got, err := resolveJustification("", true, planWithJustification(false, false))
	if err != nil {
		t.Fatal(err)
	}
	if got != defaultJustification {
		t.Errorf("justification = %q, want the default", got)
	}

	// A remembered value does not change that. It is a prefill for the prompt,
	// and sending it unasked put last week's sentence into this week's PIM
	// audit log — a record someone reads during an incident review, saying the
	// wrong thing with nobody having typed it.
	if saveErr := saveLastJustification("landing zone work"); saveErr != nil {
		t.Fatal(saveErr)
	}
	got, err = resolveJustification("", true, planWithJustification(false))
	if err != nil {
		t.Fatal(err)
	}
	if got != defaultJustification {
		t.Errorf("justification = %q; the remembered text must not go out unasked", got)
	}
}

// TestResolveJustificationPrefersTheFlag: -j always wins and never prompts,
// even when a policy requires a justification.
func TestResolveJustificationPrefersTheFlag(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	got, err := resolveJustification("INC-4711", true, planWithJustification(true, true))
	if err != nil {
		t.Fatal(err)
	}
	if got != "INC-4711" {
		t.Errorf("justification = %q, want the flag's value", got)
	}
}

// TestResolveJustificationNonInteractiveNeverPrompts: the prompt is only ever
// reachable from the interactive flow.
func TestResolveJustificationNonInteractiveNeverPrompts(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	got, err := resolveJustification("", false, planWithJustification(true, true))
	if err != nil {
		t.Fatal(err)
	}
	if got != defaultJustification {
		t.Errorf("justification = %q", got)
	}
}
