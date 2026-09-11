// `pimctl down` and `pimctl deactivate`: the flags, what a selection resolves
// to, and the run itself. The ARM call a target becomes is in request.go, the
// Target type in target.go, and the plan and results tables in report.go.

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/larsakerlund/pimctl/internal/config"
	"github.com/larsakerlund/pimctl/internal/term"
)

// deactivateOpts is one deactivation's selection, as the flags gathered it.
// The zero value names nothing, which means the interactive multi-select;
// checkDeactivateFlags rejects that when there is no terminal to show it on.
type deactivateOpts struct {
	projectOpts projectOpts // Explicit project targets; bare down never discovers a file.
	all         bool        // --all: everything currently activated.
	roles       []string    // --role: exact name when one matches, otherwise substring.
	scopes      []string    // --scope: scope id or display-name substring.
	keys        []string    // --key: selection keys, in full or as an unambiguous prefix.
	preset      string      // --preset: give up exactly what a saved selection covers.
	noWait      bool        // --no-wait: send without polling for the final status.
	yes         bool        // -y: skip the confirmation.
	// impliedAll makes a bare invocation mean "everything currently active",
	// which is what `pimctl down` promises.
	impliedAll bool
}

// newDownCmd is the primary spelling. Unlike `deactivate`, a bare `down` means
// "give up everything I hold in this context" — the thing you want at the end
// of a task — still behind a confirmation unless -y.
func newDownCmd(opts *globalOpts, d deps) *cobra.Command {
	var o deactivateOpts
	cmd := &cobra.Command{
		Use:   "down [PRESET]",
		Short: "Give up activated roles — all of them, or a saved preset",
		Long: `Deactivate the roles you currently hold.

With no arguments this selects everything currently activated in the resolved
context and asks for confirmation. Name a preset, or use --role/--scope/--key,
to narrow it.`,
		Example: `  # give up everything you hold in the current context
  pimctl down

  # give up exactly what "daily" covers
  pimctl down daily

  # unattended
  pimctl down -y`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				o.preset = args[0]
			}
			o.impliedAll = o.preset == "" && !o.all &&
				len(o.roles) == 0 && len(o.scopes) == 0 && len(o.keys) == 0
			return runDeactivate(cmd, opts, d, &o)
		},
	}
	addDeactivateFlags(cmd, &o)
	return cmd
}

// newDeactivateCmd builds the explicit spelling. A bare `deactivate` opens the
// multi-select rather than selecting everything, which is the whole difference
// between it and `down`.
func newDeactivateCmd(opts *globalOpts, d deps) *cobra.Command {
	var o deactivateOpts
	cmd := &cobra.Command{
		Use:   "deactivate [PRESET]",
		Short: "Deactivate roles you currently have activated",
		Long: `Give up one or more currently activated Azure resource roles.

With no selection flags pimctl shows an interactive multi-select of the roles
that are activated right now. --role, --scope, --key, --all and --preset select
them without a terminal.`,
		Example: `  pimctl deactivate
  pimctl deactivate daily
  pimctl deactivate --all -y`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				o.preset = args[0]
			}
			return runDeactivate(cmd, opts, d, &o)
		},
	}
	addDeactivateFlags(cmd, &o)
	return cmd
}

// addDeactivateFlags registers the selection flags on cmd and binds them to o,
// so the two spellings of the command cannot drift apart.
func addDeactivateFlags(cmd *cobra.Command, o *deactivateOpts) {
	addProjectFlags(cmd, &o.projectOpts, false)
	f := cmd.Flags()
	f.BoolVar(&o.all, "all", false, "deactivate every activated role")
	f.StringArrayVar(&o.roles, "role", nil, "select roles whose name contains this text (repeatable, case-insensitive)")
	f.StringArrayVar(
		&o.scopes,
		"scope",
		nil,
		"select roles whose scope id or name contains this text (repeatable, case-insensitive)",
	)
	f.StringArrayVar(
		&o.keys,
		"key",
		nil,
		"select roles by the stable key shown in `pimctl list` (repeatable; prefixes allowed)",
	)
	f.StringVar(&o.preset, "preset", "", "select the roles saved in this preset")
	f.BoolVar(&o.noWait, "no-wait", false, "submit the requests without polling for the final status")
	f.BoolVarP(&o.yes, "yes", "y", false, "skip the confirmation prompt")
}

// checkDeactivateFlags rejects the flag combinations that cannot work without a
// terminal, before any network call.
func checkDeactivateFlags(o *deactivateOpts) error {
	if err := checkSelectionFlags(o.all, o.preset, o.roles, o.scopes, o.keys); err != nil {
		return err
	}
	hasSelection := o.projectOpts.selected() || o.all || o.impliedAll || o.preset != "" ||
		len(o.roles) > 0 || len(o.scopes) > 0 || len(o.keys) > 0
	if !hasSelection && !term.StdinIsTTY() {
		return errNoTTY("role selection")
	}
	if !o.yes && !term.StdinIsTTY() {
		return errNoConfirmTTY
	}
	return nil
}

// runDeactivate reads what is currently activated, resolves the selection into
// targets, confirms the plan and submits one request per target.
//
// It returns an error when the flags cannot work without a terminal, when no
// activated role matched the selection, when a target has no session for its
// context, or when the run itself did not fully succeed; reportRun decides
// which exit code that last one carries. Each role that lands is written to the
// activation record as it does, so an interrupt partway through still leaves
// behind what actually happened.
func runDeactivate(cmd *cobra.Command, opts *globalOpts, d deps, o *deactivateOpts) error {
	project, presetEntries, err := prepareDeactivation(cmd, opts, o)
	if err != nil {
		return err
	}

	rc, err := prepare(cmd, opts, d, presetContexts(presetEntries))
	if err != nil {
		return err
	}
	defer rc.finish(cmd)
	presetEntries, selectedScopes, err := projectDeactivation(cmd, rc, project, presetEntries)
	if err != nil {
		return err
	}
	ctx := rc.Ctx
	active, failures, err := readActivations(ctx, cmd, rc, selectedScopes)
	if err != nil {
		return err
	}
	if done, doneErr := noDeactivationTargets(cmd, active, o, failures, project); done {
		return doneErr
	}
	targets, selErr := selectTargets(ctx, cmd, rc, active, o, presetEntries)
	if selErr != nil {
		return selErr
	}
	if len(targets) == 0 {
		return errors.New("no activated role matched the selection")
	}
	if sessErr := attachSessions(targets, rc.Sessions); sessErr != nil {
		return sessErr
	}
	return finishDeactivation(cmd, rc, o, targets, active, failures)
}

// finishDeactivation confirms and executes resolved targets, preserving ordinary
// deactivation result reporting and per-role activation-record updates.
func finishDeactivation(
	cmd *cobra.Command,
	rc *runContext,
	o *deactivateOpts,
	targets []target,
	active []activeRow,
	failures []error,
) error {
	ctx, opts := rc.Ctx, rc.Opts
	multi := len(rc.Sessions) > 1
	scopes := deactivateScopeLabeler(targets, active)

	proceed, err := confirmPlan(cmd, opts, confirmOpts{
		Yes: o.yes,
		// A bare `down` is the end-of-task gesture and still asks, because
		// nothing was picked by hand.
		All:    true,
		Roles:  len(targets),
		Prompt: fmt.Sprintf("Deactivate %s?", roleCount(len(targets))),
	}, func(w io.Writer) { printDeactivatePlan(w, targets, multi, scopes) })
	if err != nil {
		return err
	}
	if !proceed {
		return nil
	}

	sp := term.NewSpinner(cmd.ErrOrStderr(), fmt.Sprintf("deactivating %s…", roleCount(len(targets))))
	stream := streamProgress(cmd, opts, len(targets), sp, scopes)
	results := executeDeactivations(ctx, targets, o.noWait, rc.Timeouts.poll, onEachResult(stream))
	sp.Stop()
	return reportRun(cmd, opts, results, failures, multi, streamedTo(stream), scopes)
}

// readActivations reads Azure and local activation evidence, retaining recorded
// roles as deactivation candidates even when omitted from the listing. It
// verifies pending records and reports every failed or incomplete read.
func readActivations(
	ctx context.Context,
	cmd *cobra.Command,
	rc *runContext,
	scopes []activationScope,
) (active []activeRow, failures []error, err error) {
	local := readLocalRecord(rc)
	sp := term.NewSpinner(cmd.ErrOrStderr(), "reading active roles…")
	active, listErrs, slowScopes := listActivations(ctx, rc, scopes)
	local.verdicts = verifyConfirming(ctx, rc, active, slowScopes)
	active = deactivationCandidates(local, active)
	sp.Stop()
	if abortedEarly(ctx) {
		return nil, nil, ctx.Err()
	}
	failures = slices.Concat(rc.Failures, listErrs)
	if len(slowScopes) > 0 {
		failures = append(
			failures,
			fmt.Errorf(
				"activation state is unknown at %d scope(s): %s",
				len(slowScopes),
				strings.Join(labelScopes(rc, slowScopes), ", "),
			),
		)
	}
	reportUnconfirmedScopes(cmd, rc, slowScopes)
	reportContextFailures(cmd.ErrOrStderr(), failures)
	return active, failures, nil
}

// nothingToDeactivate handles an empty listing the user did not narrow.
//
// Only a bare "give me everything" depends on the listing being complete; a
// named selection is attempted regardless — see selectTargets.
func nothingToDeactivate(
	cmd *cobra.Command,
	active []activeRow,
	o *deactivateOpts,
	failures []error,
) (handled bool, err error) {
	if len(active) > 0 || hasExplicitSelection(o) {
		return false, nil
	}
	if len(failures) > 0 {
		fmt.Fprintln(cmd.ErrOrStderr(), "Could not determine all active roles; deactivation is incomplete.")
		return true, partialFailureError(failures)
	}
	reportNothingActive(cmd)
	return true, nil
}

// reportNothingActive explains an empty listing without claiming it is the
// whole truth: ARM has been seen to drop roles the user genuinely still holds.
func reportNothingActive(cmd *cobra.Command) {
	fmt.Fprintln(cmd.OutOrStdout(), "Nothing to deactivate — no roles are currently activated.")
	fmt.Fprintln(cmd.ErrOrStderr(),
		"note: ARM's activation listing has been seen to omit roles that are genuinely still held.\n"+
			"If you believe you still hold something, name it: `pimctl down <preset>`, --role/--scope, or --key.")
}

// hasExplicitSelection reports whether the user named what to deactivate,
// rather than asking for whatever is currently listed as active.
func hasExplicitSelection(o *deactivateOpts) bool {
	return o.preset != "" || len(o.roles) > 0 || len(o.scopes) > 0 || len(o.keys) > 0
}

// selectTargets resolves what to deactivate.
//
// A named selection does not trust the activation listing. ARM has been
// observed dropping genuinely-active roles from both
// roleAssignmentScheduleInstances and roleAssignmentSchedules for ten minutes
// at a stretch, with no revocation in the request log — during which
// `deactivate --all` reported nothing to do and exited 0 while the roles were
// still held. So when the user names roles, pimctl asks ARM about those roles
// and reports ARM's answer per role, rather than concluding from a listing that
// can be wrong.
func selectTargets(
	ctx context.Context,
	cmd *cobra.Command,
	rc *runContext,
	active []activeRow,
	o *deactivateOpts,
	presetEntries []config.PresetEntry,
) ([]target, error) {
	activeTargets := make([]target, 0, len(active))
	for _, a := range active {
		activeTargets = append(activeTargets, targetFromActive(a))
	}

	switch {
	case o.preset != "" || o.projectOpts.selected():
		// A preset carries scope and role id, so it needs no listing at all.
		selected := make([]target, 0, len(presetEntries))
		for _, e := range presetEntries {
			selected = append(selected, targetFromPreset(e))
		}
		return mergeTargets(selected, activeTargets), nil

	case len(o.keys) > 0, len(o.roles) > 0, len(o.scopes) > 0:
		// Match against eligibility, which is cached and complete, then merge
		// in anything the activation listing did show.
		rows, listErrs, _ := readEligibilities(ctx, cmd, rc)
		if len(listErrs) > 0 {
			reportContextFailures(cmd.ErrOrStderr(), listErrs)
		}
		var chosen []row
		var err error
		if len(o.keys) > 0 {
			if chosen, err = selectByKeys(rows, o.keys); err != nil {
				return nil, err
			}
		} else {
			var report string
			chosen, report = selectByName(rows, o.roles, o.scopes)
			fmt.Fprintln(cmd.ErrOrStderr(), report)
		}
		selected := make([]target, 0, len(chosen))
		for _, r := range chosen {
			selected = append(selected, targetFromEligible(r))
		}
		return mergeTargets(selected, activeTargets), nil

	case o.all, o.impliedAll:
		return activeTargets, nil
	}

	picked, err := selectInteractiveActive(active, multipleActiveContexts(active), scopeLabelerForActive(active))
	if err != nil {
		return nil, err
	}
	out := make([]target, 0, len(picked))
	for _, p := range picked {
		out = append(out, targetFromActive(p))
	}
	return out, nil
}

// attachSessions gives every target the session for its context.
func attachSessions(targets []target, sessions []*session) error {
	for i := range targets {
		if targets[i].Session == nil {
			targets[i].Session = sessionFor(sessions, targets[i].Context)
		}
		if targets[i].Session == nil {
			return fmt.Errorf("no session for context %q", targets[i].Context)
		}
	}
	return nil
}

// deactivateScopeLabeler builds the scope labels from the targets and from
// everything currently active, so a scope reads the same as it does in `status`.
func deactivateScopeLabeler(targets []target, active []activeRow) scopeLabeler {
	refs := make([]scopeRef, 0, len(targets)+len(active))
	for _, t := range targets {
		refs = append(refs, scopeRef{Name: t.ScopeName, ID: t.Scope})
	}
	for _, a := range active {
		refs = append(refs, scopeRef{Name: a.Assignment.ScopeName(), ID: a.Assignment.Properties.Scope})
	}
	return newScopeLabeler(refs)
}

// noDeactivationTargets preserves ordinary empty-selection output while allowing
// explicit project targets to reach ARM even when its listing dropped a row.
func noDeactivationTargets(
	cmd *cobra.Command,
	active []activeRow,
	o *deactivateOpts,
	failures []error,
	project *config.Project,
) (bool, error) {
	if project != nil {
		return false, nil
	}
	return nothingToDeactivate(cmd, active, o, failures)
}

// projectDeactivation adapts exact project targets after checking the tenant.
func projectDeactivation(
	cmd *cobra.Command,
	rc *runContext,
	project *config.Project,
	entries []config.PresetEntry,
) ([]config.PresetEntry, []activationScope, error) {
	if project == nil {
		return entries, nil, nil
	}
	s, err := projectSession(cmd, rc, project.Tenant)
	if err != nil {
		return nil, nil, err
	}
	fmt.Fprintln(
		cmd.ErrOrStderr(),
		"Deactivating the exact roles in this file. Any other project using those same activations is affected.",
	)
	return projectEntries(project, s), projectScopes(project, s), nil
}

// prepareDeactivation validates selectors and reads local configuration before
// authentication. A missing explicitly requested project never falls back.
func prepareDeactivation(
	cmd *cobra.Command,
	opts *globalOpts,
	o *deactivateOpts,
) (*config.Project, []config.PresetEntry, error) {
	if err := validateFlags(opts); err != nil {
		return nil, nil, err
	}
	project, err := o.projectOpts.load(cmd, false, hasExplicitSelection(o) || o.all)
	if err != nil {
		return nil, nil, err
	}
	if project != nil {
		o.impliedAll = false
	}
	if flagErr := checkDeactivateFlags(o); flagErr != nil {
		return nil, nil, flagErr
	}
	presetEntries, err := presetSelection(
		o.preset,
		o.all || len(o.roles) > 0 || len(o.scopes) > 0 || len(o.keys) > 0,
	)
	if err != nil {
		return nil, nil, err
	}

	return project, presetEntries, nil
}
