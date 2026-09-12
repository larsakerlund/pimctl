// `pimctl down` and `pimctl deactivate`: the flags, what a selection resolves
// to, and the run itself. The ARM call a target becomes is in request.go, the
// Target type in target.go, the evidence a named run reads ARM's answers
// against in evidence.go, and the plan and results tables in report.go.

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/larsakerlund/pimctl/internal/armclient"
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
			if err := o.takePresetArg(args); err != nil {
				return err
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
			if err := o.takePresetArg(args); err != nil {
				return err
			}
			return runDeactivate(cmd, opts, d, &o)
		},
	}
	addDeactivateFlags(cmd, &o)
	return cmd
}

// takePresetArg adopts a bare PRESET argument as --preset. Naming two
// different presets, one bare and one by flag, is refused with the same
// wording `up` uses rather than letting the argument silently win; the same
// name both ways is accepted.
func (o *deactivateOpts) takePresetArg(args []string) error {
	if len(args) != 1 {
		return nil
	}
	if o.preset != "" && o.preset != args[0] {
		return fmt.Errorf("preset given twice: %q and --preset %q", args[0], o.preset)
	}
	o.preset = args[0]
	return nil
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
		"select roles by the stable key shown in 'pimctl list' (repeatable; prefixes allowed)",
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

// runDeactivate resolves the selection into targets, confirms the plan and
// submits one request per target.
//
// A bare `down`, `--all` and the interactive picker act on what is currently
// activated, so they read the activation listing first. A named selection —
// a preset, --project, --role, --scope or --key — carries its own scopes and
// role ids, so its listing runs in the background and is consulted only where
// it changes an answer; see [runNamedDeactivation].
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
	if project != nil || hasExplicitSelection(o) {
		return runNamedDeactivation(cmd, rc, o, presetEntries, selectedScopes)
	}
	ctx := rc.Ctx
	active, failures, err := readActivations(ctx, cmd, rc, selectedScopes)
	if err != nil {
		return err
	}
	if done, doneErr := nothingToDeactivate(cmd, active, failures); done {
		return doneErr
	}
	targets, selErr := selectTargets(active, o)
	if selErr != nil {
		return selErr
	}
	if len(targets) == 0 {
		return errors.New("no activated role matched the selection")
	}
	if sessErr := attachSessions(targets, rc.Sessions); sessErr != nil {
		return sessErr
	}
	return finishDeactivation(cmd, rc, o, targets, active, failures, nil)
}

// runNamedDeactivation is the named half of [runDeactivate]: the listing
// starts in the background, the targets are resolved from the eligibility
// rows and the local record, and the requests go out without waiting for it.
//
// The listing costs a per-scope fan-out of seconds, and for a named selection
// it changes only two things: how a "no such assignment" answer reads, and
// whether a filter that matched nothing else can still find an activation the
// record does not have. Both are handled where they arise — the first in
// [activeEvidence.deactivate], the second in [selectNamedTargets] — so the
// common run, where every named role is one this machine activated, pays for
// no listing at all. Its failures are therefore not the run's: each role's
// outcome is ARM's answer to its own request, and a scope the listing could
// not read is noted under the results if the listing has landed by then.
func runNamedDeactivation(
	cmd *cobra.Command,
	rc *runContext,
	o *deactivateOpts,
	presetEntries []config.PresetEntry,
	scopes []activationScope,
) error {
	ev := startActiveEvidence(rc, scopes)
	reportContextFailures(cmd.ErrOrStderr(), rc.Failures)
	targets, err := selectNamedTargets(cmd, rc, o, presetEntries, ev)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return errors.New("no activated role matched the selection")
	}
	if sessErr := attachSessions(targets, rc.Sessions); sessErr != nil {
		return sessErr
	}
	return finishDeactivation(cmd, rc, o, targets, ev.candidates(), rc.Failures, ev)
}

// finishDeactivation confirms and executes resolved targets, preserving ordinary
// deactivation result reporting and per-role activation-record updates.
//
// ev is the evidence a named run reads ARM's answers against, and nil for a
// run that read the listing up front; see [activeEvidence.deactivate].
func finishDeactivation(
	cmd *cobra.Command,
	rc *runContext,
	o *deactivateOpts,
	targets []target,
	active []activeRow,
	failures []error,
	ev *activeEvidence,
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
	var results []result
	if ev == nil {
		results = executeDeactivations(ctx, targets, o.noWait, rc.Timeouts.poll, onEachResult(stream))
	} else {
		// The same runner, bound and ordering as executeDeactivations; only
		// the per-item call differs, in how it reads a "no such assignment".
		results = submitAll(targets, onEachResult(stream), func(t target) result {
			return ev.deactivate(ctx, t, o.noWait, rc.Timeouts.poll)
		})
	}
	sp.Stop()
	// Requests have already been sent to ARM, so nothing below may return
	// early — the user must always see which roles landed and the right exit
	// code.
	runErr := reportRun(cmd, opts, results, failures, multi, streamedTo(stream), scopes)
	if ev != nil {
		reportListingGaps(cmd.ErrOrStderr(), rc, ev)
	}
	return runErr
}

// reportListingGaps names on w the scopes a named run's background listing
// could not read, under a note saying what that does and does not mean, and
// prints nothing while the listing has not landed or read everything. It goes
// after the results table because the table is ARM's per-request answer and
// this is only a caveat on how a "no such assignment" was read, and to stderr
// so `-o json` stdout stays machine-readable.
func reportListingGaps(w io.Writer, rc *runContext, ev *activeEvidence) {
	errs, unconfirmed := ev.gaps()
	if len(errs)+len(unconfirmed) == 0 {
		return
	}
	fmt.Fprintf(
		w,
		"note: the activation listing could not read %d scope(s); "+
			"each role's result above is ARM's own answer to its request:\n",
		len(errs)+len(unconfirmed),
	)
	for _, err := range errs {
		fmt.Fprintf(w, "  %v\n", err)
	}
	for _, label := range labelScopes(rc, unconfirmed) {
		fmt.Fprintf(w, "  %s: ARM did not answer within the deadline\n", label)
	}
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
// named selection never comes here, because it is attempted regardless — see
// [runNamedDeactivation].
func nothingToDeactivate(cmd *cobra.Command, active []activeRow, failures []error) (handled bool, err error) {
	if len(active) > 0 {
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

// selectTargets resolves a bare `down`, `--all` or the interactive picker
// against what the listing and the record show held. A named selection is
// resolved by [selectNamedTargets] instead.
func selectTargets(active []activeRow, o *deactivateOpts) ([]target, error) {
	if o.all || o.impliedAll {
		activeTargets := make([]target, 0, len(active))
		for _, a := range active {
			activeTargets = append(activeTargets, targetFromActive(a))
		}
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

// selectNamedTargets resolves a preset, --project, --role, --scope or --key
// selection without waiting for the activation listing.
//
// A named selection does not trust the activation listing. ARM has been
// observed dropping genuinely-active roles from both
// roleAssignmentScheduleInstances and roleAssignmentSchedules for ten minutes
// at a stretch, with no revocation in the request log — during which
// `deactivate --all` reported nothing to do and exited 0 while the roles were
// still held. So when the user names roles, pimctl asks ARM about those roles
// and reports ARM's answer per role, rather than concluding from a listing that
// can be wrong.
//
// It does not wait for the listing either: a preset carries scope and role id,
// and --role, --scope and --key match against the eligibility rows and the
// record, both of which are on hand. What the record holds is merged into the
// targets so their session and window come along, and so a "no such
// assignment" for a role this machine saw held reads as propagation. The
// listing is waited for only when the filters matched nothing, in case the
// activation they name is one the record does not have — see [withActiveRows].
// It returns selectByKeys' refusal of a key that matches nothing or too much.
func selectNamedTargets(
	cmd *cobra.Command,
	rc *runContext,
	o *deactivateOpts,
	presetEntries []config.PresetEntry,
	ev *activeEvidence,
) ([]target, error) {
	if o.preset != "" || o.projectOpts.selected() {
		// A preset carries scope and role id, so it needs no listing at all.
		selected := make([]target, 0, len(presetEntries))
		for _, e := range presetEntries {
			selected = append(selected, targetFromPreset(e))
		}
		return mergeTargets(selected, evidenceTargets(ev.candidates())), nil
	}

	// Match against eligibility, which is cached and complete, plus every
	// activation the record holds at a scope no eligibility carries.
	rows, listErrs, _ := readEligibilityRows(rc.Ctx, cmd, rc)
	if len(listErrs) > 0 {
		reportContextFailures(cmd.ErrOrStderr(), listErrs)
	}
	chosen, report, err := matchNamedRows(withActiveRows(rows, ev.candidates()), o)
	if len(chosen) == 0 {
		// Nothing on hand matches. The activation named may be one only the
		// listing knows — made at a narrower scope from another machine, say
		// — so this is the one place a named run waits for it, and the match
		// is answered from the listing as a bare `down` would have read it.
		sp := term.NewSpinner(cmd.ErrOrStderr(), "reading active roles…")
		active, _ := ev.wait(-1)
		sp.Stop()
		if abortedEarly(rc.Ctx) {
			return nil, rc.Ctx.Err()
		}
		chosen, report, err = matchNamedRows(withActiveRows(rows, active), o)
	}
	if report != "" {
		fmt.Fprintln(cmd.ErrOrStderr(), report)
	}
	if err != nil {
		return nil, err
	}
	selected := make([]target, 0, len(chosen))
	for _, r := range chosen {
		selected = append(selected, targetFromEligible(r))
	}
	return mergeTargets(selected, evidenceTargets(ev.candidates())), nil
}

// matchNamedRows applies --key, or --role and --scope, to rows. report is the
// line selectByName wants printed, and empty for --key; err is selectByKeys'
// refusal of a key that matches nothing, too much or is too short, which
// selectByName never gives.
func matchNamedRows(rows []row, o *deactivateOpts) (chosen []row, report string, err error) {
	if len(o.keys) > 0 {
		chosen, err = selectByKeys(rows, o.keys)
		return chosen, "", err
	}
	chosen, report = selectByName(rows, o.roles, o.scopes)
	return chosen, report, nil
}

// evidenceTargets renders the rows a named run believes held as targets; see
// [targetFromEvidence] for what each one's SeenActive means.
func evidenceTargets(active []activeRow) []target {
	out := make([]target, 0, len(active))
	for _, a := range active {
		out = append(out, targetFromEvidence(a))
	}
	return out
}

// withActiveRows appends to the eligibility rows one row for each activation
// that no eligibility row already identifies, so --role, --scope and --key
// resolve against both.
//
// An activation made at a narrower scope than its eligibility (`up --at`) has
// a key of its own: the eligibility is at the subscription, the activation at a
// resource group inside it. Matching the flags against eligibilities alone
// finds only the granting scope, so a deactivation there is answered
// RoleAssignmentDoesNotExist and the narrower activation is left held. Folding
// the activations into the candidate rows lets one selectByName or selectByKeys
// pass see both, which also keeps its exact-beats-substring rule joint across
// the two sources: an exact role name among the eligibilities still stops a
// substring from picking up a differently named activation.
func withActiveRows(rows []row, active []activeRow) []row {
	seen := make(map[string]bool, len(rows))
	for _, r := range rows {
		seen[r.Key()] = true
	}
	out := slices.Clone(rows)
	for _, a := range active {
		r := rowFromActive(a)
		if seen[r.Key()] {
			continue
		}
		seen[r.Key()] = true
		out = append(out, r)
	}
	return out
}

// rowFromActive renders an activation as a selectable row, carrying the scope,
// role and display names the selectors match on. The row identifies the same
// (context, scope, role) as targetFromActive does, so a target built from it
// merges with the activation in mergeTargets and inherits its SeenActive.
func rowFromActive(a activeRow) row {
	var e armclient.Eligibility
	e.Properties.Scope = a.Assignment.Properties.Scope
	e.Properties.RoleDefinitionID = a.Assignment.Properties.RoleDefinitionID
	e.Properties.ExpandedProperties = a.Assignment.Properties.ExpandedProperties
	return row{Context: a.Context, Elig: e, Active: &a.Assignment, ActiveState: a.State}
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
