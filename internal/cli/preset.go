// The `preset` command group — list, show, delete — over the selections saved
// under $XDG_CONFIG_HOME. Saving one is a side effect of `up`, in activate.go;
// replaying one back into rows is ApplyPreset in row.go.

package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/larsakerlund/pimctl/internal/config"
)

// newPresetCmd is the `preset` parent command. It does nothing on its own —
// its subcommands are the whole of it — so running it prints its own help.
func newPresetCmd(opts *globalOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "preset",
		Short: "Manage saved role selections",
	}
	cmd.AddCommand(newPresetListCmd(opts), newPresetShowCmd(opts), newPresetDeleteCmd())
	return cmd
}

// newPresetListCmd builds `preset list`: every saved preset and how many roles
// it holds. When there are none it names the file it looked in, so an empty
// list cannot be mistaken for pimctl reading the wrong $XDG_CONFIG_HOME.
func newPresetListCmd(opts *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List saved presets",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ps, err := config.LoadPresets()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if opts.json() {
				return encodeJSON(out, ps.Presets)
			}
			names := ps.Names()
			if len(names) == 0 {
				path, err := config.PresetsPath()
				if err != nil {
					return err
				}
				fmt.Fprintf(out, "No presets saved yet (%s).\n", path)
				return nil
			}
			w := newTabWriter(out)
			fmt.Fprintln(w, "NAME\tROLES")
			for _, n := range names {
				fmt.Fprintf(w, "%s\t%d\n", n, len(ps.Presets[n]))
			}
			return w.Flush()
		},
	}
}

// newPresetShowCmd builds `preset show NAME`: the roles saved in one preset,
// with the full scope id alongside the label, since two scopes can share a
// display name. It errors when no preset has that name.
func newPresetShowCmd(opts *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "show NAME",
		Short: "Show the roles saved in a preset",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ps, err := config.LoadPresets()
			if err != nil {
				return err
			}
			entries, ok := ps.Get(args[0])
			if !ok {
				return fmt.Errorf("no preset named %q", args[0])
			}
			out := cmd.OutOrStdout()
			if opts.json() {
				return encodeJSON(out, entries)
			}
			// The same labeller every other table uses: a management group
			// always carries its own name, because three of them here are
			// called "Contoso landing zones" and the bare display name says
			// nothing about which one a preset would elevate on.
			refs := make([]scopeRef, 0, len(entries))
			for _, e := range entries {
				refs = append(refs, scopeRef{Name: e.ScopeName, ID: e.Scope})
			}
			scopes := newScopeLabeler(refs)

			w := newTabWriter(out)
			fmt.Fprintln(w, headContext+"\t"+headRole+"\t"+headScope+"\tSCOPE ID")
			for _, e := range entries {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", e.Context, e.RoleName, scopes.Label(e.ScopeName, e.Scope), e.Scope)
			}
			return w.Flush()
		},
	}
}

// newPresetDeleteCmd builds `preset delete NAME`. It rewrites the presets file
// and errors when no preset has that name, rather than reporting a no-op as a
// deletion.
func newPresetDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete NAME",
		Short: "Delete a preset",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ps, err := config.LoadPresets()
			if err != nil {
				return err
			}
			if !ps.Delete(args[0]) {
				return fmt.Errorf("no preset named %q", args[0])
			}
			if err := config.SavePresets(ps); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted preset %q.\n", args[0])
			return nil
		},
	}
}

// encodeJSON writes v as the indented JSON every `-o json` path emits.
func encodeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
