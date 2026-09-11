// Saved narrowed selections retain the grant scope for replay. Ordinary
// presets still match the eligibility listing and preserve missing-role rules.

package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/larsakerlund/pimctl/internal/armclient"
	"github.com/larsakerlund/pimctl/internal/config"
	"github.com/larsakerlund/pimctl/internal/term"
)

// resolveNarrowedPreset verifies saved narrower targets against ARM ancestry
// before supplying them to the ordinary preset matcher. It returns lookup or
// ambiguity errors before submission, never silently widens a target.
func resolveNarrowedPreset(
	cmd *cobra.Command,
	rc *runContext,
	rows []row,
	entries []config.PresetEntry,
) ([]row, error) {
	for _, e := range entries {
		if e.EligibilityScope == "" || strings.EqualFold(e.EligibilityScope, e.Scope) {
			continue
		}
		if err := config.ValidateScope(e.Scope); err != nil {
			return nil, err
		}
		s := sessionFor(rc.Sessions, contextLabel(e.Context))
		if s == nil {
			continue
		}
		sp := term.NewSpinner(cmd.ErrOrStderr(), "verifying saved activation scope…")
		eligible, err := scopedEligibilities(rc.Ctx, s, e.Scope, rc.Refresh)
		sp.Stop()
		if err != nil {
			return nil, err
		}
		var sources []armclient.Eligibility
		for _, source := range eligible {
			if strings.EqualFold(source.Properties.Scope, e.EligibilityScope) {
				sources = append(sources, source)
			}
		}
		chosen, err := chooseScopedEligibility(sources, e.RoleDefinitionID, e.Scope, "")
		if err != nil {
			return nil, fmt.Errorf("saved target %s: %w", e.Scope, err)
		}
		rows = append(rows, targetedRow(s, chosen, e.Scope))
	}
	return rows, nil
}

// readActivationEligibility defers activation discovery for narrowed targets
// until selection is resolved. Ordinary selection keeps its background fan-out.
func readActivationEligibility(
	cmd *cobra.Command,
	rc *runContext,
	o *activateOpts,
	entries []config.PresetEntry,
) ([]row, []error, *activeFuture) {
	narrowed := o.at != ""
	for _, e := range entries {
		if e.EligibilityScope != "" && !strings.EqualFold(e.EligibilityScope, e.Scope) {
			narrowed = true
		}
	}
	if !narrowed {
		return readEligibilities(rc.Ctx, cmd, rc)
	}
	rows, errs, _ := readEligibilityRows(rc.Ctx, cmd, rc)
	return rows, errs, nil
}
