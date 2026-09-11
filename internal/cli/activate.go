// The `up`/`activate` command: its flags, and the order the activation
// pipeline runs in — select, plan, justify, confirm, submit, report. The
// request bodies are in request.go, the policy lookup and the duration clamp in
// plan.go, and every table this prints in report.go.

package cli

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/larsakerlund/pimctl/internal/armclient"
	"github.com/larsakerlund/pimctl/internal/config"
	"github.com/larsakerlund/pimctl/internal/term"
)

// activateOpts is one `up`/`activate` invocation's flags, already parsed. The
// zero value is the fully interactive run: pick the roles in the terminal, take
// each role's policy maximum as the duration, and prompt for a justification
// only if a policy asks for one.
type activateOpts struct {
	verified     map[string]entryVerdict // Project activations verified through their own schedule requests.
	projectOpts  projectOpts             // Project requirements or an explicit discovery bypass.
	requirements *config.Project         // Resolved local requirements; nil for ordinary selection.
	at           string                  // Exact activation target; --scope remains a selection filter.
	all          bool                    // --all: every eligible role, which on this tenant is over a hundred.
	roles        []string                // --role: exact name when one matches, otherwise substring.
	scopes       []string                // --scope: scope id or display-name substring.
	// keys are selection keys from `pimctl list`, in full or as any
	// unambiguous prefix of at least four characters.
	keys       []string
	preset     string // --preset: a saved selection, which also supplies the contexts.
	savePreset string // --save-preset: save this run's selection under that name.
	// forDuration, hours and duration are the three spellings of the
	// activation length — only one may be given, and the latter two are
	// deprecated. All empty or zero means each role's policy maximum.
	forDuration string
	hours       float64 // --hours, deprecated.
	duration    string  // --duration, deprecated.
	// justification is -j. It lands in the PIM audit log, and is required when
	// there is no terminal to prompt on.
	justification string
	// ticketNumber and ticketSystem are only read when a role's policy enables
	// the Ticketing rule, which is the one rule pimctl cannot satisfy on the
	// user's behalf.
	ticketNumber string
	ticketSystem string // as above.
	// noWait submits the requests without polling them to a terminal status,
	// so a role's reported outcome is whatever ARM said on the way in.
	noWait bool
	yes    bool // -y: skip the confirmation, which an unattended run needs.
	// force lifts the maxNonInteractive cap on an unattended --role/--scope
	// selection.
	force bool
}

// newUpCmd is the primary spelling; newActivateCmd keeps the original name
// working for existing scripts, docs and the Claude skill.
func newUpCmd(opts *globalOpts, d deps) *cobra.Command {
	c := newActivateCmd(opts, d)
	c.Use = "up [PRESET]"
	c.Short = "Activate project roles, a saved preset, or an interactive selection"
	c.Aliases = nil
	c.Args = cobra.MaximumNArgs(1)
	c.Example = `  # pick interactively in the current context (just type to filter)
  pimctl up

  # a saved set, in exactly the contexts the preset names
  pimctl up daily

  # one role, for two hours, unattended
  pimctl up --role "Cost Management Contributor" --scope contoso-test --for 2h -j "INC-4711" -y`
	return c
}

// newActivateCmd builds the command both `up` and `activate` are spellings of:
// the flags, the help text, and the one argument form (a bare preset name) that
// duplicates --preset. It performs no I/O; [runActivate] does the work.
func newActivateCmd(opts *globalOpts, d deps) *cobra.Command {
	var o activateOpts
	cmd := &cobra.Command{
		Use:   "activate [PRESET]",
		Short: "Activate eligible Azure resource roles, in batch",
		Long: `Activate one or more of your eligible Azure resource roles.

With no selection flags pimctl uses the nearest .pimctl.yaml when present.
Run pimctl init to create one, or use --no-project to bypass it. Bare down and
status keep their usual meaning; use --project to select this file explicitly.

Without a project file pimctl shows an interactive multi-select of every
eligible role: type to filter, tab to toggle, ctrl+a for everything matching,
enter to confirm.
--role, --scope, --key, --all and --preset select roles without a terminal.

A bare PRESET argument is the same as --preset PRESET, and opens exactly the
contexts that preset names.

The activation length defaults to each role's own policy maximum. --for caps it:
a request longer than the policy allows is clamped to the maximum and the clamp
is reported per role.`,
		Example: `  pimctl activate
  pimctl activate daily
  pimctl activate --role Owner --scope contoso-test --for 90m -j "INC-4711" -y`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				if o.preset != "" && o.preset != args[0] {
					return fmt.Errorf("preset given twice: %q and --preset %q", args[0], o.preset)
				}
				o.preset = args[0]
			}
			return runActivate(cmd, opts, d, &o)
		},
	}
	f := cmd.Flags()
	addProjectFlags(cmd, &o.projectOpts, true)
	f.StringVar(&o.at, "at", "", "activate selected eligible roles at this exact ARM scope")
	f.BoolVar(&o.all, "all", false, "select every eligible role")
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
	f.StringVar(&o.savePreset, "save-preset", "", "save the selection under this preset name")
	f.StringVar(
		&o.forDuration,
		"for",
		"",
		"how long to activate for: 2h, 90m, 1h30m or PT2H30M (default: each role's policy maximum)",
	)
	f.BoolVar(&o.force, "force", false, "allow a non-interactive selection of more than 10 roles")
	f.Float64Var(&o.hours, "hours", 0, "")
	f.StringVar(&o.duration, "duration", "", "")
	// MarkDeprecated only fails when the flag does not exist; both are defined
	// two lines up, so the error is unreachable.
	f.MarkDeprecated("hours", "use --for instead, e.g. --for 2h")         //nolint:errcheck // see above
	f.MarkDeprecated("duration", "use --for instead, e.g. --for PT2H30M") //nolint:errcheck // see above
	f.StringVarP(&o.justification, "justification", "j", "", "justification sent with every activation")
	f.StringVar(&o.ticketNumber, "ticket-number", "", "ticket number, for roles whose policy requires ticketing")
	f.StringVar(&o.ticketSystem, "ticket-system", "", "ticket system name, used with --ticket-number")
	f.BoolVar(&o.noWait, "no-wait", false, "submit the requests without polling for the final status")
	f.BoolVarP(&o.yes, "yes", "y", false, "skip the confirmation prompt")
	return cmd
}

// checkActivateFlags rejects, before any network call, the flag combinations
// that cannot work — so `pimctl up < /dev/null` says it needs a terminal
// without first paying for a token and a listing.
func checkActivateFlags(o *activateOpts) error {
	if err := checkSelectionFlags(o.all, o.preset, o.roles, o.scopes, o.keys); err != nil {
		return err
	}
	hasSelection := o.requirements != nil || o.all || o.preset != "" || len(o.roles) > 0 || len(o.scopes) > 0 ||
		len(o.keys) > 0
	if !hasSelection && !term.StdinIsTTY() {
		return errNoTTY("role selection")
	}
	if !o.yes && !term.StdinIsTTY() {
		return errNoConfirmTTY
	}
	if !term.StdinIsTTY() && strings.TrimSpace(o.justification) == "" {
		return errors.New("-j/--justification is required when not running interactively.\n" +
			"The justification is written to the PIM audit log, so pimctl will not silently reuse the last one you typed")
	}
	return nil
}

// runActivate is the activation pipeline, in the order the steps depend on one
// another: reject the impossible flag combinations before any network call,
// open a session per context, read the eligibilities, narrow them to a
// selection, build the plan — which is what resolves the policies, and so what
// decides whether a justification is even wanted — confirm, submit, report.
//
// It sends one PUT per selected role, writes the activation record, the last
// justification and, with --save-preset, a preset, and blocks until every
// request reaches a terminal status unless --no-wait was given. Past the
// confirmation nothing may return early: the requests are already in flight, so
// the user must see what happened to each role and get the right exit code.
//
// The returned error is a setup or usage failure only. A role that failed, or
// that is waiting on an approver, is carried out through [reportRun] as an exit
// code instead.
func runActivate(cmd *cobra.Command, opts *globalOpts, d deps, o *activateOpts) error {
	requested, err := prepareActivation(cmd, opts, o)
	if err != nil {
		return err
	}
	presetEntries, err := presetSelection(
		o.preset,
		o.all || len(o.roles) > 0 || len(o.scopes) > 0 || len(o.keys) > 0,
	)
	if err != nil {
		return err
	}

	rc, err := prepare(cmd, opts, d, presetContexts(presetEntries))
	if err != nil {
		return err
	}
	defer rc.finish(cmd)
	if o.requirements != nil {
		return runProjectActivation(cmd, rc, o, requested)
	}
	rows, listErrs, future := readActivationEligibility(cmd, rc, o, presetEntries)
	failures := slices.Concat(rc.Failures, listErrs)
	reportContextFailures(cmd.ErrOrStderr(), failures)

	selected, interactive, err := chooseRows(cmd, rc, rows, o, presetEntries)
	if err != nil {
		return err
	}
	if o.at != "" {
		if len(failures) > 0 {
			return errors.Join(failures...)
		}
		selected, err = narrowRows(cmd, rc, selected, o.at)
		if err != nil {
			return err
		}
		rows = selected
	}
	if future == nil {
		future = startActivationListing(rc.Ctx, rc, targetScopes(selected))
	}
	return finishActivation(cmd, rc, o, requested, rows, selected, interactive, future, failures)
}

// finishActivation plans and submits a resolved selection through the shared
// policy, justification and reporting pipeline. Failures remain per role once
// requests have been sent; project preflight errors abort before submission.
func finishActivation(cmd *cobra.Command, rc *runContext, o *activateOpts, requested time.Duration,
	rows, selected []row, interactive bool, future *activeFuture, failures []error,
) error {
	ctx, opts := rc.Ctx, rc.Opts
	failures = append(failures, markAlreadyActive(selected, future)...)

	// The plan comes first because it carries the policies, and the policies
	// decide whether a justification is even wanted.
	var plan []*planItem
	spPlan := term.NewSpinner(cmd.ErrOrStderr(), "checking activation policies…")
	if o.requirements != nil {
		plan = buildProjectPlan(rc, selected, requested, o.ticketNumber, o.verified)
	} else {
		plan = buildPlan(ctx, selected, rc.Sessions, requested, o.ticketNumber, rc.Refresh)
	}
	spPlan.Stop()
	if o.requirements != nil {
		if err := projectPlanError(plan); err != nil {
			return err
		}
	}
	justification, err := resolveJustification(o.justification, interactive || o.requirements != nil, plan)
	if err != nil {
		return err
	}

	multi := len(rc.Sessions) > 1
	// Built from every eligible role, not just the selection, so a scope reads
	// the same here as in `list` and `status` — and so a single-scope selection
	// still says *which* "Contoso landing zones" is about to be elevated.
	scopes := scopeLabelerForRows(rows)

	proceed, err := confirmPlan(cmd, opts, confirmOpts{
		Yes:         o.yes,
		Interactive: interactive,
		All:         o.all,
		Roles:       activationSubmissionCount(plan),
		Prompt:      fmt.Sprintf("Activate %s?", roleCount(activationSubmissionCount(plan))),
	}, func(w io.Writer) { printPlanTable(w, plan, multi, scopes) })
	if err != nil {
		return err
	}
	if !proceed {
		return nil
	}

	// Activation is as slow as Azure is; it must not look dead while it runs.
	sp := term.NewSpinner(cmd.ErrOrStderr(), fmt.Sprintf("activating %s…", roleCount(len(plan))))
	stream := streamProgress(cmd, opts, len(plan), sp, scopes)
	results := executeActivations(
		ctx, plan, justification, o.ticketNumber, o.ticketSystem, o.noWait, rc.Timeouts.poll,
		onEachResult(stream),
	)
	sp.Stop()

	// An ALREADY ACTIVE result should say how long the existing window has left.
	// That needs the activation listing, so wait for it here — but only if a
	// result actually wants it, and only briefly. By this point the activations
	// have made a full ARM round-trip, so it has almost always landed already.
	if needsActiveWindow(results) {
		if res, ok := future.Wait(rc.Timeouts.activeBackfill); ok {
			failures = append(failures, res.errs...)
			results = fillAlreadyActiveWindows(results, res.rows)
		}
	}

	// Requests have already been sent to ARM, so nothing below may return early
	// — the user must always see which roles activated and the right exit code.
	rememberActivateRun(cmd, activationJustification(plan, justification), o.savePreset, selected)
	return reportRun(cmd, opts, results, failures, multi, streamedTo(stream), scopes)
}

// chooseRows narrows the eligible roles down to what the user asked for, and
// says so plainly when there is nothing to work with.
func chooseRows(
	cmd *cobra.Command,
	rc *runContext,
	rows []row,
	o *activateOpts,
	presetEntries []config.PresetEntry,
) (selected []row, interactive bool, err error) {
	if len(rows) == 0 {
		return nil, false, errors.New("you have no eligible Azure resource roles in the selected context(s)")
	}
	selected, interactive, err = selectRows(cmd, rc, rows, o, presetEntries)
	if err != nil {
		return nil, false, err
	}
	if len(selected) == 0 {
		return nil, false, errors.New("no eligible role matched the selection")
	}
	return selected, interactive, nil
}

// maxNonInteractive bounds an unattended selection. A bare substring like
// "Contributor" matches 52 of this tenant's 136 roles; firing all of them
// because of one loose flag is not a mistake the tool should make quietly.
const maxNonInteractive = 10

// selectRows resolves the selection flags, falling back to the interactive
// multi-select when none were given.
func selectRows(
	cmd *cobra.Command,
	rc *runContext,
	rows []row,
	o *activateOpts,
	presetEntries []config.PresetEntry,
) (selected []row, interactive bool, err error) {
	errw := cmd.ErrOrStderr()
	switch {
	case o.preset != "":
		rows, err = resolveNarrowedPreset(cmd, rc, rows, presetEntries)
		if err != nil {
			return nil, false, err
		}
		sel, missing := applyPreset(rows, presetEntries)
		for _, m := range missing {
			fmt.Fprintf(errw, "skipping %s — no longer eligible\n", m)
		}
		return sel, false, nil
	case o.all:
		// Conflicting selectors are rejected in checkActivateFlags, before any
		// network call; by here the selection is known to be coherent.
		return rows, false, nil
	case len(o.keys) > 0:
		sel, keyErr := selectByKeys(rows, o.keys)
		return sel, false, keyErr
	case len(o.roles) > 0 || len(o.scopes) > 0:
		sel, report := selectByName(rows, o.roles, o.scopes)
		fmt.Fprintln(errw, report)
		if len(sel) > maxNonInteractive && !o.force {
			return nil, false, fmt.Errorf(
				"the selection matched %d roles, which is more than the %d pimctl will activate unattended.\n"+
					"Narrow it (add --scope, or use --key from `pimctl list`), or pass --force if you really mean all %d",
				len(sel), maxNonInteractive, len(sel))
		}
		return sel, false, nil
	}
	// The dimming comes from the local record, which is available immediately.
	// The ARM fan-out lands seconds after the picker paints, so a picker fed
	// from it would show the marks after the user had already chosen.
	sel, err := selectInteractive(applyLocalRecord(rows, rc), multipleContexts(rows), scopeLabelerForRows(rows))
	return sel, true, err
}

// markAlreadyActive marks the rows the slow activation listing says are already
// held, if that listing has landed. It never waits for it: ARM answers
// RoleAssignmentExists and pimctl reports ALREADY ACTIVE, which is the same
// information a moment later.
func markAlreadyActive(selected []row, future *activeFuture) []error {
	res, ok := future.TryGet()
	if !ok {
		return nil
	}
	active, activeErrs := res.rows, res.errs
	copy(selected, applyActive(slices.Clone(selected), active))
	return activeErrs
}

// needsActiveWindow reports whether any already-active result is missing the
// end time of the window the user already holds.
func needsActiveWindow(results []result) bool {
	for _, r := range results {
		if r.Outcome == OutcomeAlreadyActive && r.Until == nil {
			return true
		}
	}
	return false
}

// fillAlreadyActiveWindows backfills the end time of an existing activation.
func fillAlreadyActiveWindows(results []result, active []activeRow) []result {
	byKey := map[string]armclient.Assignment{}
	for i := range active {
		a := active[i]
		byKey[rowKey(a.Context, a.Assignment.Properties.Scope, a.Assignment.RoleDefinitionGUID())] = a.Assignment
	}
	for i := range results {
		r := &results[i]
		if r.Outcome != OutcomeAlreadyActive || r.Until != nil {
			continue
		}
		if a, ok := byKey[rowKey(r.Context, r.Scope, armclient.RoleDefinitionGUID(r.RoleDefinitionID))]; ok {
			r.Until = a.Properties.EndDateTime
			// The window opened whenever it opened — often hours ago. Taking
			// "now" for a role that was already active would misdate it badly.
			r.Since = a.Properties.StartDateTime
		}
	}
	return results
}

// rememberActivateRun writes back the things a successful run should be able to
// repeat: the justification, and the preset if one was asked for. Both are
// best-effort — the roles are already activated, so a failure here is a warning
// and never an error.
func rememberActivateRun(cmd *cobra.Command, justification, presetName string, selected []row) {
	errw := cmd.ErrOrStderr()
	if err := rememberJustification(justification); err != nil {
		fmt.Fprintf(errw, "warning: could not save the justification for next time: %v\n", err)
	}
	if presetName == "" {
		return
	}
	if err := savePreset(presetName, selected); err != nil {
		fmt.Fprintf(errw, "warning: could not save preset %q: %v\n", presetName, err)
		return
	}
	fmt.Fprintf(errw, "saved preset %q with %d role(s).\n", presetName, len(selected))
}

// savePreset stores a selection under a preset name, replacing whatever was
// saved under that name before. It rewrites the whole presets file under
// $XDG_CONFIG_HOME and returns the error from reading or writing it.
func savePreset(name string, rows []row) error {
	ps, err := config.LoadPresets()
	if err != nil {
		return err
	}
	ps.Set(name, toPresetEntries(rows))
	return config.SavePresets(ps)
}

// prepareActivation validates local selection and duration before opening any
// login. Automatic project discovery is resolved before terminal requirements.
func prepareActivation(cmd *cobra.Command, opts *globalOpts, o *activateOpts) (time.Duration, error) {
	if err := validateFlags(opts); err != nil {
		return 0, err
	}
	if err := resolveActivationProject(cmd, o); err != nil {
		return 0, err
	}
	requested, err := requestedDuration(cmd, o.forDuration, o.hours, o.duration)
	if err != nil {
		return 0, err
	}
	if flagErr := checkActivateFlags(o); flagErr != nil {
		return 0, flagErr
	}
	return requested, nil
}

// activationJustification skips remembered-state updates when every project role
// was already held and no activation request needed a justification.
func activationJustification(plan []*planItem, justification string) string {
	for _, item := range plan {
		if !item.KeepActive {
			return justification
		}
	}
	return ""
}

// rememberJustification writes an actual run's reason; an empty reason means no
// new activation was attempted and leaves the existing prompt prefill alone.
func rememberJustification(justification string) error {
	if justification == "" {
		return nil
	}
	return saveLastJustification(justification)
}
