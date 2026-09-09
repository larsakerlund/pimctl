// The root command and what is global to all of it: the persistent flags every
// subcommand inherits, how the build version is resolved, the `pimctl help
// auth` topic, and the help-screen tidying. Each subcommand is built in the
// file named after it.

package cli

import (
	"fmt"
	"runtime/debug"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/larsakerlund/pimctl/internal/cache"
)

// Version is the build version, stamped by the Makefile through
// `-ldflags -X .../internal/cli.Version=...`. A plain `go build` leaves it at
// [devVersion], which is what makes [versionString] fall back to the VCS
// stamp the toolchain records on its own.
var Version = devVersion

// devVersion is the placeholder an unstamped build carries.
const devVersion = "dev"

// globalOpts is the set of persistent flags, in the order they are declared on
// the root command. They decide which tenants a command acts on and how it
// reports, never what it does.
type globalOpts struct {
	contexts    []string // -c, repeatable; cloudctx context names in the order given.
	allContexts bool     // --all-contexts: every context `cloudctx list` reports.
	// bareAz is --bare-az: the shared `az login` store, chosen explicitly.
	// See [bareAzMode] for why it is not a plain bool.
	bareAz    bareAzMode
	output    string // -o: "table" or [outputJSON]; checked by validateOutput.
	refresh   bool   // --refresh: ignore the cached listings and re-read ARM.
	debug     bool   // --debug: print the phase timings to stderr.
	allScopes bool   // --all-scopes: one tenant-wide listing instead of the fan-out.
}

// json reports whether output is machine-readable, which decides where a table
// may be printed and whether a spinner or a streamed line may be printed at all.
func (o *globalOpts) json() bool { return o != nil && o.output == outputJSON }

// NewRootCmd builds the pimctl command tree with the real collaborators.
func NewRootCmd() *cobra.Command { return newRootCmd(defaultDeps()) }

// newRootCmd builds the tree against the given [deps], which is how a test
// substitutes a fake ARM backend for one tree rather than for the process.
func newRootCmd(d deps) *cobra.Command {
	// One options value per command tree, bound to cobra's flags and handed to
	// the commands that read it. Per tree rather than per process because cobra
	// binds a flag to an address once, and a package-level address is shared by
	// every tree a test builds.
	opts := &globalOpts{}
	root := &cobra.Command{
		Use:   "pimctl",
		Short: "Batch-activate your eligible Azure PIM resource roles",
		Long: `Activate the Azure roles you are eligible for, without the portal.

pimctl works with Azure resource roles held through Privileged Identity
Management — the ones granted at a management group, subscription, resource
group or resource.

  See what you can get      pimctl ls
  Take it, one or many      pimctl up            (interactive, type to filter)
  Check what you hold       pimctl status
  Give it back              pimctl down

Roles are activated in batch, not one at a time: pick several in the picker, or
select them with --role/--scope/--key, and pimctl fires them together and
reports each one. A set you use often can be saved as a preset and replayed with
"pimctl up <preset>". Several tenants at once is one flag: -c twice, or
--all-contexts.

Each role's length defaults to whatever its own PIM policy allows; --for caps
it. Roles needing approval are reported as pending, never as granted.

Authentication uses your Azure CLI login. With cloudctx, pass -c <context> or
run inside a context window; otherwise the shared "az login" is used and pimctl
prints which tenant that resolved to. See "pimctl help auth".`,
		Example: `  pimctl ls                                  # what am I eligible for?
  pimctl up                                  # pick interactively, type to filter
  pimctl up daily                            # replay a saved preset
  pimctl up --role Owner --scope contoso-test --for 2h -j "INC-4711" -y
  pimctl status                              # what do I hold, and for how long?
  pimctl down                                # give it all back`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	registerGlobalFlags(root, opts)

	presets, caches, version := newPresetCmd(opts), newCacheCmd(), newVersionCmd()
	root.AddCommand(
		newUpCmd(opts, d),
		newDownCmd(opts, d),
		newListCmd(opts, d),
		newStatusCmd(opts, d),
		newActivateCmd(opts, d),
		newDeactivateCmd(opts, d),
		presets,
		caches,
		newLogoutCmd(),
		version,
	)
	root.AddCommand(newAuthHelpTopic())
	registerCompletions(root, opts)
	// These three touch no tenant, so the global flags mean nothing in them.
	// -o survives on presets, which really does print JSON.
	hideInheritedFlags(presets, "output")
	hideInheritedFlags(caches)
	hideInheritedFlags(version)
	// cobra adds `completion` on Execute, not on AddCommand, so ask for it now
	// or there is nothing to hide the flags on.
	root.InitDefaultCompletionCmd()
	if c, _, err := root.Find([]string{"completion"}); err == nil && c != nil && c != root {
		hideInheritedFlags(c)
	}
	return root
}

// newVersionCmd builds `pimctl version`, which prints what [versionString]
// resolved. It touches no tenant, so [NewRootCmd] hides the global flags on it.
func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the pimctl version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintf(cmd.OutOrStdout(), "pimctl %s\n", versionString())
			return nil
		},
	}
}

// versionString reports the build version.
//
// The Makefile stamps internal/cli.Version. A plain `go build` or `go install`
// stamps nothing, so rather than printing a bare "dev" it falls back to the
// build info the Go toolchain records automatically — the module version, and
// the VCS revision and dirty flag for a local build.
func versionString() string {
	if Version != "" && Version != devVersion {
		return Version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return devVersion
	}
	var revision, modified string
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value
		}
	}
	// Prefer the revision: for a local build the module version is a
	// pseudo-version that already contains it, so printing both is noise.
	if revision != "" {
		const shortRevision = 8
		if len(revision) > shortRevision {
			revision = revision[:shortRevision]
		}
		if modified == "true" {
			revision += "+dirty"
		}
		return revision
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	return devVersion
}

// SetVersion lets main pass a version stamped into its own package, so
// `-ldflags "-X main.version=..."` — the form most people reach for — works as
// well as the Makefile's `-X internal/cli.Version=...`.
func SetVersion(v string) {
	if v = strings.TrimSpace(v); v != "" {
		Version = v
	}
}

// newAuthHelpTopic is a cobra help topic: `pimctl help auth`. Authentication is
// the thing people need explained once and then never again, so it does not
// belong in the root help every command prints.
func newAuthHelpTopic() *cobra.Command {
	return &cobra.Command{
		Use:   "auth",
		Short: "How pimctl authenticates (help topic)",
		Long: `pimctl never logs in itself. It asks the Azure CLI for an ARM access
token and talks to management.azure.com directly. The Azure CLI is the only
thing it needs: with a plain "az login" everything works except -c and
--all-contexts, which are cloudctx's own features.

Which login is used, in order:

  --bare-az            forces the shared "az login" store and wins over
                       everything below, including a preset's own contexts
  -c <name>            a cloudctx context, repeatable for several tenants
  --all-contexts       every context "cloudctx list" reports
  a preset's contexts  "pimctl up daily" opens exactly what the preset names
  $` + envContext + `    set by "cloudctx use <name>" in this shell
  az login             the shared ~/.azure store, used when nothing above applies

When the shared login is used, pimctl prints one line naming the tenant and user
it resolved to, so every run says which login it used.

cloudctx is optional, and worth installing if you work across tenants: it
keeps one Azure CLI store per context, so a token for one tenant can never be
used against another. Without it, pimctl uses the one "az login" you have and
says which tenant that is.

Caching. The access token is cached per context under $XDG_CACHE_HOME/pimctl
(0600 in a 0700 directory, refused if anything widens that) and reused while at
least five minutes of validity remain — otherwise every command pays for an
"az" process launch. The eligible-role listing is cached for ` + cache.TTLHuman + `,
and each role's PIM policy for 24 hours. "--refresh" bypasses them, and
"pimctl cache clear" deletes them.

Activation state is never cached, but pimctl does keep a record of the
activations it performed, under $XDG_STATE_HOME/pimctl. That is not a cache —
it is what keeps "status" right while Azure's listing catches up — so
"cache clear" leaves it alone. "cache clear --all" deletes it too.

If ARM rejects a cached token, pimctl drops it and retries once with a fresh
one, so a revoked or policy-invalidated token heals itself.

Troubleshooting a login: "cloudctx login <context>", or "az login". A
Conditional Access challenge is reported with the exact command to run. To end a
session rather than start one: "az logout", or "cloudctx exec <ctx> -- az
logout" for one context's store.

The token cache is pimctl's own, not part of a cloudctx context: deleting a
context does not remove it, and a context repointed at another tenant is caught
on read — every cached token carries the tenant it was minted for, and one that
no longer matches is dropped and re-minted. "pimctl cache clear" removes them
all.`,
		Args: cobra.NoArgs,
		// A help topic has no RunE: cobra prints Long for `pimctl help auth`.
	}
}

// registerGlobalFlags binds the persistent flags to one options value. They are
// the flags every command shares: which tenants to act on, and how to report.
func registerGlobalFlags(root *cobra.Command, opts *globalOpts) {
	pf := root.PersistentFlags()
	pf.StringArrayVarP(
		&opts.contexts,
		"context",
		"c",
		nil,
		"cloudctx context to act on (repeatable; defaults to $"+envContext+")",
	)
	pf.BoolVar(&opts.allContexts, "all-contexts", false, "act on every context reported by `cloudctx list`")
	// A string flag with NoOptDefVal, so `--bare-az` still works as a bare
	// switch while `--bare-az=force` can say something the switch cannot.
	pf.StringVar(
		(*string)(&opts.bareAz),
		"bare-az",
		string(bareAzUnset),
		"use the shared `az login` store instead of a cloudctx context (=force to allow it inside a context window)",
	)
	pf.Lookup("bare-az").NoOptDefVal = string(bareAzOn)
	pf.StringVarP(&opts.output, "output", "o", "table", "output format: table or json")
	pf.BoolVar(&opts.refresh, "refresh", false, "ignore the cached role listing and re-read it from Azure")
	pf.BoolVar(&opts.debug, "debug", false, "print a timing breakdown of each phase to stderr")
	pf.BoolVar(
		&opts.allScopes,
		"all-scopes",
		false,
		"read active roles with one tenant-wide ARM call instead of per-scope queries (slower, and observed to silently omit rows)",
	)
}

// hideInheritedFlags stops a command from advertising global flags that do
// nothing in it: `pimctl version --all-scopes` and `pimctl preset list -c foo`
// read as if they might mean something, and a help screen listing five
// irrelevant flags is a help screen people stop reading.
//
// keep names the ones that do apply — `preset list` really does honour -o json.
func hideInheritedFlags(cmd *cobra.Command, keep ...string) {
	inherited := cmd.HelpFunc() // resolved before we replace it, so no recursion.
	cmd.SetHelpFunc(func(c *cobra.Command, args []string) {
		globals := c.Root().PersistentFlags()
		var hidden []*pflag.Flag
		globals.VisitAll(func(f *pflag.Flag) {
			if f.Hidden || slices.Contains(keep, f.Name) {
				return
			}
			f.Hidden = true
			hidden = append(hidden, f)
		})
		defer func() {
			for _, f := range hidden {
				f.Hidden = false
			}
		}()
		inherited(c, args)
	})
	for _, sub := range cmd.Commands() {
		hideInheritedFlags(sub, keep...)
	}
}

// validateFlags checks the global flags every command shares, before any token
// is minted or any listing read: a typo costs no ARM call rather than failing
// after the work is done.
func validateFlags(opts *globalOpts) error {
	if err := validateOutput(opts); err != nil {
		return err
	}
	return validateBareAz(opts)
}

// validateBareAz rejects a --bare-az value pimctl does not understand, before
// anything is minted: a typo like --bare-az=yes must not read as "unset" and
// quietly use a context.
func validateBareAz(opts *globalOpts) error {
	if !opts.bareAz.valid() {
		return fmt.Errorf("unknown --bare-az value %q (use --bare-az or --bare-az=force)", string(opts.bareAz))
	}
	return nil
}

// validateOutput rejects an unrecognised -o value, naming the two formats
// pimctl has.
func validateOutput(opts *globalOpts) error {
	switch opts.output {
	case "table", outputJSON:
		return nil
	default:
		return fmt.Errorf("unknown output format %q (want table or json)", opts.output)
	}
}
