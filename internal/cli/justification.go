// The justification policy: whether a run has to ask for one, what it sends
// when it does not, and remembering the last one for next time. It lands in the
// PIM audit log, so pimctl asks whenever a policy demands one and never invents
// anything beyond saying where the request came from. Drawing the prompt itself
// is interactive.go's job.

package cli

import (
	"fmt"
	"strings"

	"github.com/larsakerlund/pimctl/internal/config"
	"github.com/larsakerlund/pimctl/internal/term"
)

// justificationDemand counts how many of the selected roles have PIM's
// Justification rule enabled.
func justificationDemand(plan []*planItem) (required, total int) {
	for _, item := range plan {
		if item.Settings == nil {
			continue
		}
		total++
		if item.Settings.RequiresJustification() {
			required++
		}
	}
	return required, total
}

// resolveJustification decides what to send, and whether to ask.
//
// Most roles' policies do not require a justification, and stopping to ask for
// one nobody reads is the kind of friction that makes a tool feel slow. So the
// prompt appears only when at least one selected role actually demands it. An
// explicit -j always wins and never prompts.
//
// The remembered text is a prefill for that prompt and nothing more. Sending it
// unasked put last week's sentence into this week's PIM audit log — a record
// someone reads during an incident review, and one that said the wrong thing
// with no one having typed it. When nobody is asked, the neutral default goes
// out instead, which claims only what it can: that pimctl made the request.
func resolveJustification(flag string, interactive bool, plan []*planItem) (string, error) {
	if flag != "" {
		return flag, nil
	}
	required, total := justificationDemand(plan)
	if !interactive || !term.StdinIsTTY() || required == 0 {
		return defaultJustification, nil
	}

	last := ""
	if st, err := config.LoadState(); err == nil {
		last = st.LastJustification
	}
	subtitle := fmt.Sprintf("required by %d of %s you selected", required, roleCount(total))
	j, err := promptJustification(last, subtitle)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(j) == "" {
		// An empty answer to a prompt that had to be shown: the policy demands
		// something, so the default goes rather than nothing.
		return defaultJustification, nil
	}
	return j, nil
}

// defaultJustification is what goes out when nothing better is known. It says
// where the request came from, which is the only useful thing an unprompted
// justification can say.
const defaultJustification = "pimctl activation"

// saveLastJustification remembers j in the state file under $XDG_CONFIG_HOME so
// the next interactive run can offer it as the prompt's prefill, and skips the
// write when it is already what is stored. It returns the error from reading or
// writing that file; the caller reports it as a warning, because by the time
// this is reached the roles are already activated.
func saveLastJustification(j string) error {
	st, err := config.LoadState()
	if err != nil {
		return err
	}
	if st.LastJustification == j {
		return nil
	}
	st.LastJustification = j
	return config.SaveState(st)
}
