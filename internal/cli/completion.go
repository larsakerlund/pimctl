// Shell completion for the values that are tedious to type. Every source here
// is local, so a TAB press never mints a token or calls ARM; generating the
// completion script itself is cobra's own `completion` command.

package cli

import (
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/larsakerlund/pimctl/internal/azauth"
	"github.com/larsakerlund/pimctl/internal/cache"
	"github.com/larsakerlund/pimctl/internal/config"
)

// registerCompletions wires shell completion for the values that are tedious to
// type: context names, preset names and selection keys.
//
// Every source is local — cloudctx's own registry, the presets file, the
// eligibility cache — so pressing TAB never triggers a network call or a token
// acquisition.
func registerCompletions(root *cobra.Command, opts *globalOpts) {
	// RegisterFlagCompletionFunc only fails when the flag is unknown or already
	// registered, both of which are compile-time facts about this file.
	root.RegisterFlagCompletionFunc("context", completeContexts) //nolint:errcheck // see above
	for _, c := range root.Commands() {
		if c.Flags().Lookup("preset") != nil {
			c.RegisterFlagCompletionFunc("preset", completePresets) //nolint:errcheck // see above
		}
		if c.Flags().Lookup("key") != nil {
			c.RegisterFlagCompletionFunc("key", completeKeysWith(opts)) //nolint:errcheck // see above
		}
		switch c.Name() {
		case "up", "down", "activate", "deactivate":
			c.ValidArgsFunction = completePresetArg
		}
	}
	for _, c := range root.Commands() {
		if c.Name() != "preset" {
			continue
		}
		for _, sub := range c.Commands() {
			switch sub.Name() {
			case "show", "delete":
				sub.ValidArgsFunction = completePresetArg
			}
		}
	}
}

// completeContexts offers the cloudctx context names, or nothing when there are
// none to offer. It goes through [azauth.ListContexts], so it reads the same
// `cloudctx list --names` the rest of pimctl does.
//
// Every failure — no cloudctx, one older than pimctl requires, a registry that
// will not be read — completes to nothing. A TAB press is not the place to
// explain any of that; the command the user then runs says it properly.
func completeContexts(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	if !azauth.CloudctxInstalled() {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names, err := azauth.ListContexts(azauth.DefaultRunner)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}

// completePresets offers the saved preset names for --preset.
func completePresets(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return presetNames(), cobra.ShellCompDirectiveNoFileComp
}

// completePresetArg offers preset names for the one positional argument
// `pimctl up <preset>` takes, and nothing once it has been given.
func completePresetArg(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return presetNames(), cobra.ShellCompDirectiveNoFileComp
}

// presetNames returns each preset as "name\tdescription", the form cobra shows
// as a completion with help text. A presets file that cannot be read completes
// to nothing: a TAB press is not the place to report an error.
func presetNames() []string {
	ps, err := config.LoadPresets()
	if err != nil {
		return nil
	}
	out := ps.Names()
	for i, n := range out {
		if entries, ok := ps.Get(n); ok {
			out[i] = n + "\t" + describePreset(entries)
		}
	}
	return out
}

// describePreset is the one-line summary shown beside a preset name: which
// contexts it covers and how many roles it holds.
func describePreset(entries []config.PresetEntry) string {
	if len(entries) == 0 {
		return "empty"
	}
	ctxs := presetContexts(entries)
	return strings.Join(ctxs, ",") + ": " + pluralRoles(len(entries))
}

// pluralRoles renders a role count with the right noun, so a one-role preset
// does not describe itself as "1 roles".
func pluralRoles(n int) string {
	if n == 1 {
		return "1 role"
	}
	return strconv.Itoa(n) + " roles"
}

// completeKeysWith returns the completion function for --key, closed over the
// flag state it needs to know which contexts' caches to read. Keys come from
// the cache only: completing them otherwise would mean authenticating and
// calling ARM on a TAB press.
func completeKeysWith(opts *globalOpts) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return completeKeys(opts)
	}
}

// completeKeys is the body of the closure [completeKeysWith] returns, taking
// the flag state explicitly so a test can call it without a command tree.
func completeKeys(opts *globalOpts) ([]string, cobra.ShellCompDirective) {
	res, err := resolveContexts(opts.contexts, opts.allContexts, opts.bareAz, nil, nil)
	if err != nil || res.Bare {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var out []string
	for _, name := range res.Names {
		cached, _, ok := cache.Read(name)
		if !ok {
			continue
		}
		for _, e := range cached {
			r := row{Context: name, Elig: e}
			out = append(out, r.SelectionKey()+"\t"+r.Elig.RoleName()+" @ "+scopeLeaf(e.Properties.Scope))
		}
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}
