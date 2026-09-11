// Scope navigation for init uses readable ARM names and permits a pasted ID
// when the caller cannot browse a parent. Browsing never changes Azure state.

package cli

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"

	"github.com/larsakerlund/pimctl/internal/armclient"
	"github.com/larsakerlund/pimctl/internal/cache"
	"github.com/larsakerlund/pimctl/internal/config"
	"github.com/larsakerlund/pimctl/internal/picker"
	"github.com/larsakerlund/pimctl/internal/term"
)

// initRows collects one or more scopes, then verifies selections can be replayed
// without embedding a user's source schedule in the generated file.
func initRows(cmd *cobra.Command, rc *runContext, s *session, o initOpts) ([]row, error) {
	var out []row
	targets := o.at
	for {
		if len(targets) == 0 {
			scope, err := browseInitScope(cmd, rc, s)
			if err != nil {
				return nil, err
			}
			targets = []string{scope}
		}
		for _, scope := range targets {
			rows, err := initScopeRows(cmd, rc, s, scope, o.roles)
			if err != nil {
				return nil, err
			}
			out = append(out, rows...)
		}
		if len(o.at) > 0 {
			return out, nil
		}
		i, err := chooseInitOption("Project requirements", []string{"Write .pimctl.yaml", "Add another scope"})
		if err != nil {
			return nil, err
		}
		if i == 0 {
			return out, nil
		}
		targets = nil
	}
}

// initScopeRows offers eligible roles at or above scope, then resolves each
// selected role by portable identity. Ambiguity prevents creating a broken file.
func initScopeRows(cmd *cobra.Command, rc *runContext, s *session, scope string, filters []string) ([]row, error) {
	sp := term.NewSpinner(cmd.ErrOrStderr(), "reading eligible project roles…")
	roles, err := scopedEligibilities(rc.Ctx, s, scope, rc.Refresh)
	sp.Stop()
	if err != nil {
		return nil, err
	}
	var rows []row
	for _, e := range roles {
		if eligibleNow(e, time.Now()) {
			rows = append(rows, targetedRow(s, e, scope))
		}
	}
	rows = dedupeRows(rows)
	if len(rows) == 0 {
		return nil, fmt.Errorf("no eligible roles found at or above %s; no file written", scope)
	}
	if len(filters) > 0 {
		rows, err = filterInitRows(cmd, rows, filters)
		if err != nil {
			return nil, err
		}
	} else {
		rows, err = selectInteractive(rows, false, scopeLabelerForRows(rows))
		if err != nil {
			return nil, err
		}
	}
	if len(rows) == 0 {
		return nil, errors.New("no roles selected; no file written")
	}
	for _, r := range rows {
		if _, err := chooseScopedEligibility(roles, r.Elig.RoleDefinitionGUID(), scope, ""); err != nil {
			return nil, err
		}
	}
	return rows, nil
}

// chooseInitOption runs the inline single-choice picker and preserves cancellation.
func chooseInitOption(title string, labels []string) (int, error) {
	items := make([]picker.Item, len(labels))
	for i, label := range labels {
		items[i] = picker.Item{Label: label, Haystack: strings.ToLower(label)}
	}
	i, err := picker.RunSingle(title, items, multiSelectHeight)
	if errors.Is(err, picker.ErrCancelled) {
		return 0, &exitCodeError{code: ExitInterrupted, msg: err.Error()}
	}
	return i, err
}

// browseInitScope walks visible subscriptions and resource groups. Parent-list
// failures remain visible and leave the explicit-ID route available.
func browseInitScope(cmd *cobra.Command, rc *runContext, s *session) (string, error) {
	scope := ""
	for {
		sp := term.NewSpinner(cmd.ErrOrStderr(), "reading scopes…")
		resources, err := initChildren(rc, s, scope)
		sp.Stop()
		if err != nil {
			fmt.Fprintf(
				cmd.ErrOrStderr(),
				"Could not browse scopes: %v\nPaste a scope ID to verify access there.\n",
				err,
			)
		}
		next, selected, pickErr := navigateInitScope(cmd, rc, s, scope, resources)
		if pickErr != nil {
			return "", pickErr
		}
		if selected {
			return next, nil
		}
		scope = next
	}
}

// navigateInitScope renders the navigation choices and returns either a next
// parent to browse or a selected activation target. Cancellation returns an error.
func navigateInitScope(
	cmd *cobra.Command,
	rc *runContext,
	s *session,
	scope string,
	resources []armclient.Resource,
) (nextScope string, selected bool, err error) {
	labels := make([]string, 0, 3+len(resources))
	title := "Choose project scope"
	if scope == "" {
		labels = append(labels, "Paste a scope ID", "Choose from eligible grant scopes")
	} else {
		title = scope
		labels = append(labels, "Use this scope", "Browse another subscription", "Paste a scope ID")
	}
	offset := len(labels)
	for _, r := range resources {
		labels = append(labels, r.Label()+" · "+r.ID)
	}
	i, err := chooseInitOption(title, labels)
	if err != nil {
		return "", false, err
	}
	if i >= offset {
		next := resources[i-offset].ID
		return next, armclient.NormalizeScopeType("", next) == "Resource", nil
	}
	if scope == "" {
		if i == 1 {
			next, grantErr := browseGrantScope(cmd, rc, s)
			return next, true, grantErr
		}
	} else {
		if i == 0 {
			return scope, true, nil
		}
		if i == 1 {
			return "", false, nil
		}
	}
	next, pasteErr := promptInitScope()
	return next, true, pasteErr
}

// initChildren reads navigable scopes through the selected authenticated client.
func initChildren(rc *runContext, s *session, scope string) ([]armclient.Resource, error) {
	if scope == "" {
		return s.Client.ListSubscriptions(rc.Ctx)
	}
	return s.Client.ListScopeChildren(rc.Ctx, scope)
}

// promptInitScope validates a pasted ARM ID inline without requesting a token.
func promptInitScope() (string, error) {
	var scope string
	err := huh.NewInput().
		Title("Target ARM scope ID").
		Description("Management group, subscription, resource group or resource").
		Value(&scope).
		Validate(config.ValidateScope).
		Run()
	if errors.Is(err, huh.ErrUserAborted) {
		return "", &exitCodeError{code: ExitInterrupted, msg: "cancelled"}
	}
	return scope, err
}

// browseGrantScope offers granting scopes, including management groups that the
// subscription resource browser cannot enumerate. It caches only eligibilities.
func browseGrantScope(cmd *cobra.Command, rc *runContext, s *session) (string, error) {
	roles, _, cached := cache.Read(s.owner())
	if !cached || rc.Refresh {
		sp := term.NewSpinner(cmd.ErrOrStderr(), "reading eligible scopes…")
		var err error
		roles, err = s.Client.ListEligibilities(rc.Ctx)
		sp.Stop()
		if err != nil {
			return "", err
		}
		cache.Write(s.owner(), roles)
	}
	var ids, labels []string
	seen := map[string]bool{}
	for _, e := range roles {
		id := e.Properties.Scope
		if seen[strings.ToLower(id)] {
			continue
		}
		seen[strings.ToLower(id)] = true
		ids = append(ids, id)
		labels = append(labels, e.ScopeName()+" · "+id)
	}
	if len(ids) == 0 {
		return "", errors.New("no eligible grant scopes found; use --at with a target scope to verify inherited access")
	}
	i, err := chooseInitOption("Choose an eligible grant scope", labels)
	if err != nil {
		return "", err
	}
	return ids[i], nil
}

// filterInitRows requires each explicit setup filter to match, so a typo in one
// of several requested roles cannot silently create an incomplete project file.
func filterInitRows(cmd *cobra.Command, rows []row, filters []string) ([]row, error) {
	var out []row
	for _, filter := range filters {
		selected, report := selectByName(rows, []string{filter}, nil)
		if len(selected) == 0 {
			return nil, fmt.Errorf("no eligible role matches %q at this scope; no file written", filter)
		}
		fmt.Fprintln(cmd.ErrOrStderr(), report)
		out = append(out, selected...)
	}
	return dedupeRows(out), nil
}
