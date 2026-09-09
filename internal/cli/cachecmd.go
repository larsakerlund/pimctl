// `pimctl cache`: clearing what pimctl can re-derive, saying what it
// deliberately did not clear, and printing the cache directory. The caches
// themselves live in internal/cache and internal/azauth; the activation
// record, which is not one of them, is record.go.

package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/larsakerlund/pimctl/internal/azauth"
	"github.com/larsakerlund/pimctl/internal/cache"
	"github.com/larsakerlund/pimctl/internal/store"
)

// newCacheCmd builds `pimctl cache`, whose help is the one place a user is
// told the whole on-disk layout: the three caches, the activation record that
// is not one of them, and which of them live inside a cloudctx context's store
// rather than under pimctl's own XDG directories.
func newCacheCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cache",
		Short: "Inspect and clear pimctl's local caches",
		Long: `pimctl caches three things on disk:

  * the ARM access token per context, so every command does not pay for a
    cloudctx/az process launch — 0600 in a 0700 directory, and refused on read
    if anything has loosened those permissions
  * the eligible-role listing per context, for ` + cache.TTLHuman + `
  * each role's PIM policy per (scope, role), for 24 hours — two ARM calls that
    otherwise land on the critical path of every activation

Activation state is never cached. What pimctl does keep is a record of the
activations it performed itself, so ` + "`status`" + ` can answer instantly; it is
always reconciled against Azure, which remains the authority.

Where they live: a context's token and activation record go inside that
context's own cloudctx store, $CLOUDCTX_STORE/pimctl/, so ` + "`cloudctx delete`" + `
sweeps them with the rest of that context's credentials. The role listings and
policies, and everything belonging to the shared ` + "`az login`" + `, stay under
$XDG_CACHE_HOME/pimctl and $XDG_STATE_HOME/pimctl (or ~/.cache and
~/.local/state) — as does everything, on a machine without cloudctx.`,
	}
	cmd.AddCommand(newCacheClearCmd(), newCachePathCmd())
	return cmd
}

// newCacheClearCmd builds `pimctl cache clear`, which removes only what pimctl
// can re-derive. --all extends it to the activation record, and the help says
// what that costs before the flag is reached for.
func newCacheClearCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "clear",
		Short: "Delete cached tokens, role listings and policies",
		Long: `Delete everything pimctl re-derives: cached tokens, role listings and
policies. The next command pays the full cost and is correct.

The record of activations pimctl performed is NOT a cache and is left alone.
Azure's own listing runs minutes behind an activation, and that record is what
keeps ` + "`status`" + ` right in the meantime — deleting it makes status under-report
roles you are holding until Azure catches up. --all deletes it too, for when you
want pimctl to forget this machine ever activated anything.`,
		Example: `  pimctl cache clear
  pimctl cache clear && pimctl ls    # force a completely fresh read
  pimctl cache clear --all           # also forget what this machine activated`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runCacheClear(cmd, all) },
	}
	cmd.Flags().BoolVar(&all, "all", false,
		"also delete this machine's record of its own activations (see the warning above)")
	return cmd
}

// newLogoutCmd is a hidden alias people reach for out of habit. It is hidden
// rather than promoted because it does not log anything out: az keeps its own
// session, so the very next command re-authenticates silently. Saying so is the
// whole point of the command existing.
func newLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "logout",
		Short:  "Alias for `cache clear` (does not end your az session)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE:   func(cmd *cobra.Command, _ []string) error { return runCacheClear(cmd, false) },
	}
}

// runCacheClear deletes the cached tokens, eligibility listings and policy
// files, and prints how many of each went. It does not touch the activation
// record unless all is set, because Azure's listing runs minutes behind an
// activation and the record is what keeps `status` right in the meantime.
//
// With all set it deletes every context's record — naming the contexts, since
// the record is cleared machine-wide rather than only for the context this
// command resolved — and warns on stderr that `status` may under-report roles
// that are still held. It returns the first deletion error, in which case some
// of the files may already be gone.
func runCacheClear(cmd *cobra.Command, all bool) error {
	tokens, err := azauth.ClearTokenCache(azauth.DefaultRunner)
	if err != nil {
		return err
	}
	listings, err := cache.Clear()
	if err != nil {
		return err
	}
	policies, err := cache.ClearPolicies()
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Cleared %d cached token(s), %d role listing(s) and %d policy file(s).\n",
		tokens, listings, policies)
	if all {
		records, contexts, recErr := clearRecords()
		if recErr != nil {
			return recErr
		}
		where := ""
		if len(contexts) > 0 {
			// Naming them matters: the record is cleared for every context on
			// the machine, including ones this command never mentioned.
			where = " across " + strings.Join(contexts, ", ")
		}
		fmt.Fprintf(out, "Forgot %d activation(s)%s.\n", records, where)
		fmt.Fprintln(cmd.ErrOrStderr(),
			"warning: until Azure's listing catches up, `pimctl status` may not show roles you are still holding")
	}
	// The cloudctx half of the advice is only advice on a machine that has it.
	logout := "Your az login is unchanged — run `az logout` for that."
	if azauth.CloudctxInstalled() {
		logout = "Your az login is unchanged — run `az logout`, " +
			"or `cloudctx exec <ctx> -- az logout` for one context's store."
	}
	fmt.Fprintln(out, logout)
	return nil
}

// newCachePathCmd builds `pimctl cache path`, so a script can find the cache
// directory without re-implementing the XDG rules. It prints pimctl's own
// directory, not a context's store: the store belongs to cloudctx, which
// answers for it with `cloudctx show <name>`.
func newCachePathCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print the cache directory",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := store.Dir()
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), dir)
			return nil
		},
	}
}
