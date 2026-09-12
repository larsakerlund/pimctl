// Covers version.go: that `version` and --version print the same line, that
// the line carries the Go version and platform, and that every shape a build
// stamps — goreleaser's bare number, the Makefile's git describe, go install's
// module version, an unstamped build — renders to the one v-prefixed form.

package cli

import (
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

func TestVersionCommand(t *testing.T) {
	out, _, err := runCmd(t, "version")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "pimctl ") {
		t.Errorf("version output = %q", out)
	}
	for _, want := range []string{runtime.Version(), runtime.GOOS + "/" + runtime.GOARCH} {
		if !strings.Contains(out, want) {
			t.Errorf("version output %q does not carry %q", out, want)
		}
	}
}

func TestVersionFlagPrintsTheSameLineAsTheCommand(t *testing.T) {
	fromCmd, _, err := runCmd(t, "version")
	if err != nil {
		t.Fatal(err)
	}
	fromFlag, _, err := runCmd(t, "--version")
	if err != nil {
		t.Fatalf("--version: %v", err)
	}
	if fromFlag != fromCmd {
		t.Errorf("--version printed %q, `version` printed %q", fromFlag, fromCmd)
	}
}

func TestRenderVersion(t *testing.T) {
	withVCS := func(mainVersion, revision, modified string) *debug.BuildInfo {
		info := &debug.BuildInfo{}
		info.Main.Version = mainVersion
		if revision != "" {
			info.Settings = append(info.Settings,
				debug.BuildSetting{Key: "vcs.revision", Value: revision},
				debug.BuildSetting{Key: "vcs.modified", Value: modified},
			)
		}
		return info
	}
	cases := []struct {
		name    string
		stamped string
		info    *debug.BuildInfo
		want    string
	}{
		{
			name:    "goreleaser stamps the tag without its v",
			stamped: "0.4.0",
			info:    withVCS("(devel)", "6051853abcdef0123456789", "false"),
			want:    "v0.4.0 (6051853, go1.99.0, linux/amd64)",
		},
		{
			name:    "the Makefile stamps git describe",
			stamped: "v0.3.0-4-g6051853-dirty",
			info:    withVCS("(devel)", "6051853abcdef0123456789", "true"),
			want:    "v0.3.0-4-g6051853-dirty (6051853-dirty, go1.99.0, linux/amd64)",
		},
		{
			name:    "go install records the module version and no commit",
			stamped: devVersion,
			info:    withVCS("v0.4.0", "", ""),
			want:    "v0.4.0 (commit unknown, go1.99.0, linux/amd64)",
		},
		{
			name:    "an unstamped local build has only the commit",
			stamped: "",
			info:    withVCS("(devel)", "abcdef0123456789", "false"),
			want:    "dev (abcdef0, go1.99.0, linux/amd64)",
		},
		{
			name:    "no build info at all",
			stamped: devVersion,
			info:    nil,
			want:    "dev (commit unknown, go1.99.0, linux/amd64)",
		},
		{
			name:    "a bare hash from git describe --always is not given a v",
			stamped: "6051853",
			info:    nil,
			want:    "6051853 (commit unknown, go1.99.0, linux/amd64)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderVersion(tc.stamped, tc.info, "go1.99.0", "linux/amd64")
			if got != tc.want {
				t.Errorf("renderVersion() = %q, want %q", got, tc.want)
			}
		})
	}
}
