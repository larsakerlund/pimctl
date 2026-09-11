// The init command writes portable project requirements from verified Azure
// eligibilities. It never activates roles or stores a personal context name.

package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/larsakerlund/pimctl/internal/armclient"
	"github.com/larsakerlund/pimctl/internal/config"
	"github.com/larsakerlund/pimctl/internal/term"
)

// initOpts selects the output and optional noninteractive source of requirements.
type initOpts struct {
	file   string   // Destination, created exclusively.
	preset string   // Existing personal preset to resolve in the current login.
	at     []string // Exact target scopes, supplied instead of browsing.
	roles  []string // Role-name filters for unattended generation at explicit scopes.
}

// newInitCmd builds the project setup command without doing I/O.
func newInitCmd(opts *globalOpts, d deps) *cobra.Command {
	var o initOpts
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create .pimctl.yaml by choosing project scopes and roles",
		Long:  "Choose the exact scopes and eligible roles this project needs. Writes a new\n.pimctl.yaml in the current directory without activating anything. Commit the\nfile so teammates can run pimctl up using their own login.",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return runInit(cmd, opts, d, o) },
	}
	f := cmd.Flags()
	f.StringVar(&o.file, "file", config.ProjectFile, "write a new requirements file at this path")
	f.StringVar(&o.preset, "from-preset", "", "create requirements from a saved preset using the selected login")
	f.StringArrayVar(&o.at, "at", nil, "exact target ARM scope instead of browsing (repeatable)")
	f.StringArrayVar(&o.roles, "role", nil, "select eligible role names at --at scopes (repeatable)")
	return cmd
}

// checkInit rejects invalid inputs and existing output before opening a login.
func checkInit(o initOpts) error {
	if _, err := os.Lstat(o.file); err == nil {
		return fmt.Errorf("%s already exists; edit it or choose a new --file", o.file)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if o.preset != "" && (len(o.at) > 0 || len(o.roles) > 0) {
		return errors.New("--from-preset cannot be combined with --at or --role")
	}
	if len(o.roles) > 0 && len(o.at) == 0 {
		return errors.New("--role requires --at")
	}
	for _, scope := range o.at {
		if err := config.ValidateScope(scope); err != nil {
			return fmt.Errorf("--at: %w", err)
		}
	}
	if !term.StdinIsTTY() && o.preset == "" && (len(o.at) == 0 || len(o.roles) == 0) {
		return errors.New(
			"init needs a terminal to choose scopes and roles; use --from-preset NAME or --at SCOPE --role NAME unattended",
		)
	}
	return nil
}

// runInit resolves every requirement under one authenticated tenant, writes an
// exclusive output file and reports the next command. Failures write no file.
func runInit(cmd *cobra.Command, opts *globalOpts, d deps, o initOpts) error {
	if err := validateFlags(opts); err != nil {
		return err
	}
	if err := checkInit(o); err != nil {
		return err
	}
	entries, err := presetSelection(o.preset, false)
	if err != nil {
		return err
	}
	rc, err := prepare(cmd, opts, d, nil)
	if err != nil {
		return err
	}
	defer rc.finish(cmd)
	s, err := projectSession(cmd, rc, "")
	if err != nil {
		return err
	}
	p := &config.Project{Tenant: s.Token.TenantID}
	if o.preset != "" {
		for _, e := range entries {
			p.Roles = append(
				p.Roles,
				config.ProjectRole{RoleDefinitionID: armclient.RoleDefinitionGUID(e.RoleDefinitionID), Scope: e.Scope},
			)
		}
		if err := p.Validate(); err != nil {
			return err
		}
		rows, readErr := readProjectRows(cmd, rc, s, p)
		if readErr != nil {
			return readErr
		}
		p.Roles = projectRoles(rows)
	} else {
		rows, pickErr := initRows(cmd, rc, s, o)
		if pickErr != nil {
			return pickErr
		}
		p.Roles = projectRoles(rows)
	}
	if err := config.CreateProject(o.file, p); err != nil {
		return err
	}
	fmt.Fprintf(
		cmd.OutOrStdout(),
		"Created %s · %s. No roles activated.\nCommit this file; run pimctl up when you need project access.\n",
		o.file,
		roleCount(len(p.Roles)),
	)
	return nil
}

// projectRoles serialises only portable target identities and readable comments.
func projectRoles(rows []row) []config.ProjectRole {
	out := make([]config.ProjectRole, 0, len(rows))
	for _, r := range dedupeRows(rows) {
		out = append(
			out,
			config.ProjectRole{
				RoleDefinitionID: r.Elig.RoleDefinitionGUID(),
				Scope:            r.Elig.Properties.Scope,
				Name:             r.Elig.RoleName(),
			},
		)
	}
	return out
}
