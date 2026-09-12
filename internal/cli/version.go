// The build version: the variable the Makefile and goreleaser stamp, the one
// line `pimctl version` and `pimctl --version` both print, and how an
// unstamped `go build` or `go install` still reports something true. Wiring
// the command and the flag into the tree is root.go's job.

package cli

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"
)

// Version is the build version, stamped by the Makefile and goreleaser through
// `-ldflags -X .../internal/cli.Version=...`. A plain `go build` leaves it at
// [devVersion], which is what makes [renderVersion] fall back to the module
// version and VCS stamp the toolchain records on its own.
var Version = devVersion

// devVersion is the placeholder an unstamped build carries.
const devVersion = "dev"

// shortCommitLen is how much of a commit hash the version line shows: the
// seven characters `git describe` and GitHub abbreviate to.
const shortCommitLen = 7

// unknownCommit fills the commit slot when the build info records no VCS
// revision, which is what `go install module@version` produces: it builds from
// the module cache, not a checkout.
const unknownCommit = "commit unknown"

// SetVersion lets main pass a version stamped into its own package, so
// `-ldflags "-X main.version=..."` — the form most people reach for — works as
// well as the Makefile's `-X internal/cli.Version=...`.
func SetVersion(v string) {
	if v = strings.TrimSpace(v); v != "" {
		Version = v
	}
}

// newVersionCmd builds `pimctl version`, which prints the line [versionString]
// renders. It touches no tenant, so [NewRootCmd] hides the global flags on it.
func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the pimctl version, commit, Go version and platform",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintf(cmd.OutOrStdout(), "pimctl %s\n", versionString())
			return nil
		},
	}
}

// versionString returns everything after "pimctl " on the version line, for
// this binary: the stamped [Version] or the toolchain's own build info, the
// commit it was built from, and the Go version and platform of the runtime.
func versionString() string {
	info, _ := debug.ReadBuildInfo()
	return renderVersion(Version, info, runtime.Version(), runtime.GOOS+"/"+runtime.GOARCH)
}

// renderVersion formats `v<version> (<commit>, <go>, <os/arch>)` from its
// parts. stamped is the value of [Version]; info is the build info, or nil
// when the binary carries none; goVersion and platform are printed verbatim.
// It is pure so a test can hand it every shape a build stamps.
func renderVersion(stamped string, info *debug.BuildInfo, goVersion, platform string) string {
	return fmt.Sprintf("%s (%s, %s, %s)", normaliseVersion(stamped, info), commitOf(info), goVersion, platform)
}

// normaliseVersion chooses the version and gives it one spelling. The stamp
// wins when there is one; otherwise the module version `go install` records,
// and failing that [devVersion]. A dotted number gets a v prefix, so
// goreleaser's `0.4.0`, the Makefile's `v0.3.0-4-g6051853` and `go install`'s
// `v0.4.0` all print as `v…`; anything else — "dev", or a bare hash from
// `git describe --always` — is printed as it is, since a v in front of it
// would be a lie.
func normaliseVersion(stamped string, info *debug.BuildInfo) string {
	v := stamped
	if v == "" || v == devVersion {
		v = devVersion
		if info != nil && info.Main.Version != "" && info.Main.Version != "(devel)" {
			v = info.Main.Version
		}
	}
	if v[0] >= '0' && v[0] <= '9' && strings.Contains(v, ".") {
		return "v" + v
	}
	return v
}

// commitOf reads the VCS revision and dirty flag the toolchain records for a
// build made inside a checkout, shortened to [shortCommitLen] and suffixed
// `-dirty` when the tree had uncommitted changes — the same spelling
// `git describe --dirty` uses. A build with no revision reports
// [unknownCommit].
func commitOf(info *debug.BuildInfo) string {
	if info == nil {
		return unknownCommit
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
	if revision == "" {
		return unknownCommit
	}
	if len(revision) > shortCommitLen {
		revision = revision[:shortCommitLen]
	}
	if modified == "true" {
		revision += "-dirty"
	}
	return revision
}
