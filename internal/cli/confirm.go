// When pimctl stops and asks before acting, and the selector combinations it
// refuses outright. Drawing the prompt is interactive.go's job, and what the
// confirmation table contains is plan.go's; this file only decides whether to
// ask.

package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/larsakerlund/pimctl/internal/config"
	"github.com/larsakerlund/pimctl/internal/term"
)

// errNoConfirmTTY is returned when the confirmation prompt cannot be shown and
// the user has not opted out of it.
var errNoConfirmTTY = errors.New(
	"stdin is not a terminal, so the confirmation prompt cannot be shown; pass -y to confirm non-interactively",
)

// errNoTTY is the message shown when a command needs an interactive selection
// but stdin is a pipe or file.
func errNoTTY(what string) error {
	// --key comes first because it is the one that cannot be ambiguous, and
	// because this message is mostly read by scripts and agents, which have a
	// `pimctl ls -o json` in hand and no business guessing at name substrings.
	return fmt.Errorf("stdin is not a terminal, so the interactive %s cannot be shown.\n"+
		"Select roles non-interactively instead: --key ABC12345 (from `pimctl ls -o json`), "+
		"--role NAME, --scope ID, --all or --preset NAME (and -y to skip the confirmation)", what)
}

// errPresetConflict is returned when --preset is mixed with the other selectors.
var errPresetConflict = errors.New("--preset cannot be combined with --all, --role, --scope or --key")

// checkSelectionFlags rejects selector combinations that contradict each other.
//
// It runs before anything is minted or read, so a plain typo like `up --all
// --role X` costs no token and no listing: catching it inside the selection
// step would mean a full round trip, and a cache notice printed on the way, to
// reject a command that was never going to work.
func checkSelectionFlags(all bool, preset string, roles, scopes, keys []string) error {
	switch {
	case preset != "" && (all || len(roles) > 0 || len(scopes) > 0 || len(keys) > 0):
		return errPresetConflict
	case all && (len(roles) > 0 || len(scopes) > 0 || len(keys) > 0):
		return errors.New("--all cannot be combined with --role, --scope or --key")
	case len(keys) > 0 && (len(roles) > 0 || len(scopes) > 0):
		return errors.New("--key cannot be combined with --role or --scope")
	default:
		return nil
	}
}

// presetSelection loads --preset, refusing to mix it with the other selectors.
// An empty name selects nothing and is not an error.
func presetSelection(preset string, conflicting bool) ([]config.PresetEntry, error) {
	if preset == "" {
		return nil, nil
	}
	if conflicting {
		return nil, errPresetConflict
	}
	return loadPreset(preset)
}

// confirmBulkThreshold is the size above which an unattended selection is worth
// a second look even when the user did not ask for --all.
const confirmBulkThreshold = 10

// confirmOpts is everything [confirmOpts.NeedsConfirmation] weighs. The zero
// value asks for nothing: no -y, not interactive, not --all and no roles is not
// a run that can do damage.
type confirmOpts struct {
	Yes         bool   // -y was given: the user has already said yes.
	Interactive bool   // the roles came from the picker, so they were chosen by hand.
	All         bool   // --all was given, whatever it turned out to cover.
	Roles       int    // how many roles the selection resolved to.
	Prompt      string // the y/N question, phrased by the command that asks it.
}

// NeedsConfirmation reports whether this run should stop and ask.
//
// -y says not to. An interactive run does not either: the user has just picked
// the roles by hand and typed a justification, so a y/N is a third confirmation
// of a decision already made twice, and the per-role lines that follow show the
// resolved scope and duration. What is left is the unattended run that never
// went through the picker, where one loose --role or an --all can touch far
// more than intended.
func (o confirmOpts) NeedsConfirmation() bool {
	if o.Yes || o.Interactive {
		return false
	}
	return o.All || o.Roles > confirmBulkThreshold
}

// confirmPlan prints the plan and, when NeedsConfirmation says so, asks before
// going ahead. A run that does not need confirming still prints the plan, but
// only to a table run — under -o json it would corrupt the output.
func confirmPlan(
	cmd *cobra.Command,
	g *globalOpts,
	opts confirmOpts,
	printPlan func(io.Writer),
) (bool, error) {
	if !opts.NeedsConfirmation() {
		if !g.json() {
			printPlan(cmd.OutOrStdout())
		}
		return true, nil
	}
	if !term.StdinIsTTY() {
		return false, errNoConfirmTTY
	}
	printPlan(planWriter(cmd, g))
	ok, err := confirm(opts.Prompt)
	if err != nil {
		return false, err
	}
	if !ok {
		fmt.Fprintln(cmd.OutOrStdout(), "Aborted.")
	}
	return ok, nil
}
