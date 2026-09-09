// Working out which cloudctx contexts a command acts on, and saying so. The
// precedence rules live here; acquiring a token for a resolved context is
// internal/azauth's job, and opening a session on it is session.go's.

package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/larsakerlund/pimctl/internal/azauth"
	"github.com/larsakerlund/pimctl/internal/config"
)

// envContext is the variable `cloudctx use <name>` exports into the shell.
// Verified against cloudctx's own source and shell shim.
const envContext = "CLOUDCTX_CONTEXT"

// contextSource records where the resolved context came from, so the user can
// be told which one is about to be acted on and why.
type contextSource string

// The ways a context can be chosen, in the words pimctl uses to report it.
const (
	SourceFlag        contextSource = "-c"             // named on the command line; wins over everything but --bare-az.
	SourceAllContexts contextSource = "--all-contexts" // every context cloudctx knows.
	SourceEnv         contextSource = "$" + envContext // exported by `cloudctx use`, and easy to forget is set.
	SourcePreset      contextSource = "preset"         // the contexts a named preset covers.
	SourceAzLogin     contextSource = "az login"       // the shared store, reported by name because it cannot be seen.
)

// resolution is the outcome of working out which contexts to act on.
type resolution struct {
	// Names are the cloudctx contexts to open. Empty when Bare is set.
	Names []string
	// Bare means "use the ambient az login" — only ever set by an explicit
	// --bare-az, never as a fallback.
	Bare bool
	// Source is how the choice was made, so the notice can say why these
	// contexts and not others.
	Source contextSource
}

// Describe renders a one-line notice for stderr.
//
// The az-login case is filled in later, once a token exists: naming the tenant
// and user needs claims from the token, and asking `az account show` for the
// tenant's display name would cost a second process launch on the fast path.
func (r resolution) Describe() string {
	if r.Bare {
		return ""
	}
	switch len(r.Names) {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf("using context %s (from %s)", r.Names[0], r.Source)
	default:
		return fmt.Sprintf("using %d contexts (from %s): %s", len(r.Names), r.Source, strings.Join(r.Names, ", "))
	}
}

// bareAzMode is what --bare-az was set to: unset, on, or forced.
//
// It is three-valued because the flag means two different things depending on
// where it is run. Outside a context window it says "use the shared az login",
// which is unambiguous. Inside one it contradicts the shell itself, and the
// cost of guessing wrong is acting on another tenant — so plain
// --bare-az is refused there and --bare-az=force is the way to say it anyway.
type bareAzMode string

// The three values --bare-az can hold.
const (
	bareAzUnset bareAzMode = ""      // the flag was not given.
	bareAzOn    bareAzMode = "true"  // --bare-az, the form cobra records for a bare flag.
	bareAzForce bareAzMode = "force" // --bare-az=force: yes, even in a context window.
)

// wanted reports whether the shared az login was asked for at all.
func (m bareAzMode) wanted() bool { return m == bareAzOn || m == bareAzForce }

// valid reports whether the flag holds one of the values pimctl accepts, so a
// typo is refused before anything is minted.
func (m bareAzMode) valid() bool {
	return m == bareAzUnset || m == bareAzOn || m == bareAzForce
}

// errNoContextsYet is --all-contexts on an installed cloudctx whose registry is
// empty. That is a state a new machine is in, not a failure of anything, so it
// says what to do next rather than reporting that nothing could be opened.
var errNoContextsYet = errors.New(
	"cloudctx has no contexts yet — create one with `cloudctx new <name>`, " +
		"or run pimctl without -c to use your `az login`")

// checkContextFlags rejects the flag combinations that contradict each other or
// the shell they are run in, before anything is resolved.
func checkContextFlags(flagContexts []string, allContexts bool, bareAz bareAzMode, inContext string) error {
	switch {
	case bareAz.wanted() && (len(flagContexts) > 0 || allContexts):
		return errors.New("--bare-az cannot be combined with -c/--context or --all-contexts")
	case allContexts && len(flagContexts) > 0:
		return errors.New("--all-contexts cannot be combined with -c/--context")
	case bareAz == bareAzOn && inContext != "":
		// The shell is scoped to a context and the flag says to ignore that.
		// One of the two is a mistake, and pimctl cannot tell which — but
		// guessing wrong means acting on the wrong tenant, so it asks.
		return fmt.Errorf(
			"this shell is scoped to the %s context ($%s), and --bare-az would ignore it.\n"+
				"Run it outside the context window, or pass --bare-az=force if the shared `az login` is really what you want",
			inContext, envContext)
	default:
		return nil
	}
}

// resolveContexts works out which contexts to act on, in strict precedence:
// explicit -c, then --all-contexts, then --bare-az, then the contexts a named
// preset covers, then $CLOUDCTX_CONTEXT, and finally an error.
//
// presetContexts is empty unless the command was given a preset.
func resolveContexts(
	flagContexts []string,
	allContexts bool,
	bareAz bareAzMode,
	presetContexts []string,
	getenv func(string) string,
) (resolution, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	inContext := strings.TrimSpace(getenv(envContext))
	if err := checkContextFlags(flagContexts, allContexts, bareAz, inContext); err != nil {
		return resolution{}, err
	}

	if len(flagContexts) > 0 {
		return withSupportedCloudctx(resolution{Names: dedupeStrings(flagContexts), Source: SourceFlag})
	}
	if allContexts {
		names, err := azauth.ListContexts(azauth.DefaultRunner)
		if err != nil {
			return resolution{}, err
		}
		if len(names) == 0 {
			return resolution{}, errNoContextsYet
		}
		return resolution{Names: names, Source: SourceAllContexts}, nil
	}
	if bareAz.wanted() {
		return resolution{Bare: true, Source: SourceAzLogin}, nil
	}
	// A preset knows which contexts it covers; opening anything else would
	// match none of its entries. This is what made `pimctl activate --preset X`
	// fail with "no eligible role matched" whenever -c was omitted.
	if len(presetContexts) > 0 {
		return withSupportedCloudctx(resolution{Names: dedupeStrings(presetContexts), Source: SourcePreset})
	}
	if inContext != "" {
		return withSupportedCloudctx(resolution{Names: []string{inContext}, Source: SourceEnv})
	}
	return resolution{Bare: true, Source: SourceAzLogin}, nil
}

// withSupportedCloudctx passes a resolution through, unless the cloudctx
// installed on this machine is older than pimctl can drive — in which case the
// resolution is replaced by that one error.
//
// It is checked here, before any context is opened, so the answer is one
// sentence about the machine's setup rather than the same sentence repeated per
// context underneath "no context could be used". A missing cloudctx is not
// caught here: `-c` naming a context on a machine that has no cloudctx has its
// own message, which says what pimctl still does without it.
func withSupportedCloudctx(res resolution) (resolution, error) {
	if err := azauth.RequireCloudctx(azauth.DefaultRunner); errors.Is(err, azauth.ErrCloudctxTooOld) {
		return resolution{}, err
	}
	return res, nil
}

// describeAzLogin is the single stderr line printed when the shared `az login`
// is used. Tenant and user come from the token's own claims, so it costs
// nothing; the tenant's display name would need a second `az` invocation and is
// not worth the second on every command.
func describeAzLogin(tenantID, user string) string {
	if tenantID == "" {
		tenantID = "unknown"
	}
	if user == "" {
		return "using az login: tenant " + tenantID
	}
	return "using az login: tenant " + tenantID + " (" + user + ")"
}

// dedupeStrings keeps the first occurrence of each non-empty string, preserving
// order. Contexts are printed back to the user in the order they were given, so
// sorting them here would make the notice disagree with the command line.
func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// presetContexts lists the distinct contexts a preset's entries name, in the
// order they first appear.
func presetContexts(entries []config.PresetEntry) []string {
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Context)
	}
	return dedupeStrings(names)
}

// loadPreset reads one preset by name.
func loadPreset(name string) ([]config.PresetEntry, error) {
	ps, err := config.LoadPresets()
	if err != nil {
		return nil, err
	}
	entries, ok := ps.Get(name)
	if !ok {
		known := ps.Names()
		if len(known) == 0 {
			return nil, fmt.Errorf("no preset named %q (none are saved yet; create one with --save-preset)", name)
		}
		return nil, fmt.Errorf("no preset named %q (known presets: %s)", name, strings.Join(known, ", "))
	}
	return entries, nil
}
