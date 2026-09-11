// Project-file selection and tenant checks run before activation. This file
// does not change ambient credentials or the defaults of status and down.

package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/larsakerlund/pimctl/internal/cache"
	"github.com/larsakerlund/pimctl/internal/config"
)

// projectOpts selects a project explicitly or bypasses automatic discovery.
type projectOpts struct {
	file      string // Explicit input file; missing files are always errors.
	project   bool   // Require the discovered default file.
	noProject bool   // Bypass discovery for an ordinary interactive up.
}

// addProjectFlags adds explicit selection shared by up, status and down.
// Automatic discovery and its escape hatch apply only to up/activate.
func addProjectFlags(cmd *cobra.Command, p *projectOpts, automatic bool) {
	f := cmd.Flags()
	f.StringVar(&p.file, "file", "", "read project requirements from this file")
	f.BoolVar(&p.project, "project", false, "use the nearest .pimctl.yaml (error if absent)")
	if automatic {
		f.BoolVar(&p.noProject, "no-project", false, "ignore .pimctl.yaml and use ordinary role selection")
	}
}

// selected reports an explicit project request, independently of discovery.
func (p projectOpts) selected() bool { return p.project || p.file != "" }

// load resolves project requirements before credentials or network work. An
// explicit role selection disables automatic discovery but conflicts with an
// explicit project request. A malformed discovered file never falls back.
func (p projectOpts) load(cmd *cobra.Command, automatic, explicitSelection bool) (*config.Project, error) {
	if p.noProject && p.selected() {
		return nil, errors.New("--no-project cannot be combined with --project or --file")
	}
	if p.selected() && explicitSelection {
		return nil, errors.New(
			"--project/--file cannot be combined with a preset, --all, --role, --scope, --key or --at",
		)
	}
	if p.noProject || (!p.selected() && (!automatic || explicitSelection)) {
		return nil, nil //nolint:nilnil // nil means ordinary selection, without a project file.
	}
	path, err := p.path()
	if err != nil {
		return nil, err
	}
	if path == "" {
		if p.selected() {
			return nil, fmt.Errorf("no %s found; run `pimctl init` to create one", config.ProjectFile)
		}
		return nil, nil //nolint:nilnil // nil means ordinary selection, without a project file.
	}
	project, err := config.LoadProject(path)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Using %s · %s\n", abs, roleCount(len(project.Roles)))
	return project, nil
}

// projectSession refuses ambiguous accounts and tenant mismatches before ARM
// requests. The file pins a tenant, never a personal cloudctx context name.
func projectSession(cmd *cobra.Command, rc *runContext, tenant string) (*session, error) {
	if rc.AllScopes {
		return nil, errors.New("project access requires per-scope verification; omit --all-scopes")
	}
	if len(rc.Failures) != 0 {
		return nil, errors.Join(rc.Failures...)
	}
	if len(rc.Sessions) != 1 {
		return nil, errors.New("project access requires one login; select one context with -c")
	}
	s := rc.Sessions[0]
	if tenant != "" && !strings.EqualFold(tenant, s.Token.TenantID) {
		return nil, fmt.Errorf(
			"project requires tenant %s; %s is signed in to tenant %s. Select the matching login with -c <context> or sign in to the required tenant",
			tenant,
			labelOf(s.Context),
			s.Token.TenantID,
		)
	}
	fmt.Fprintf(
		cmd.ErrOrStderr(),
		"Account: %s · tenant %s · %s\n",
		projectUser(s),
		s.Token.TenantID,
		labelOf(s.Context),
	)
	return s, nil
}

// projectUser names the signed-in user without an extra profile lookup.
func projectUser(s *session) string {
	if s.Token.UserPrincipalName != "" {
		return s.Token.UserPrincipalName
	}
	return s.Token.PrincipalID
}

// projectEntries adapts exact requirements to the existing named deactivation
// pipeline. It never uses eligibility sources as deactivation targets.
func projectEntries(p *config.Project, s *session) []config.PresetEntry {
	out := make([]config.PresetEntry, 0, len(p.Roles))
	for _, r := range p.Roles {
		name := r.RoleDefinitionID
		if cached, ok := cache.ReadScoped(s.owner(), r.Scope); ok {
			for _, e := range cached {
				if strings.EqualFold(e.RoleDefinitionGUID(), r.RoleDefinitionID) {
					name = e.RoleName()
					break
				}
			}
		}
		roleID := r.Scope + "/providers/Microsoft.Authorization/roleDefinitions/" + r.RoleDefinitionID
		out = append(out, config.PresetEntry{
			Context: s.Context, Scope: r.Scope,
			RoleDefinitionID: roleID, RoleName: name, ScopeName: scopeLeaf(r.Scope),
		})
	}
	return out
}

// projectScopes returns each exact target once, with the selected account label.
func projectScopes(p *config.Project, s *session) []activationScope {
	out := make([]activationScope, 0, len(p.Roles))
	seen := map[string]bool{}
	for _, r := range p.Roles {
		key := strings.ToLower(r.Scope)
		if !seen[key] {
			out = append(out, activationScope{Context: s.Token.Label(), ID: r.Scope})
			seen[key] = true
		}
	}
	return out
}

// path resolves an explicit filename or discovers the nearest project file.
func (p projectOpts) path() (string, error) {
	if p.file != "" {
		return p.file, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return config.FindProject(cwd)
}
