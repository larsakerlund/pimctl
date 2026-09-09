// Covers root.go: that `version` prints something, that a bad -o is refused
// before any work, and that the help text describes the picker keys pimctl
// actually has. The subcommands' own behaviour is tested in their own files.

package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/larsakerlund/pimctl/internal/picker"
)

func TestVersionCommand(t *testing.T) {
	out, _, err := runCmd(t, "version")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "pimctl ") {
		t.Errorf("version output = %q", out)
	}
}

func TestRejectsUnknownOutputFormat(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()}
	f.install()
	if _, _, err := runCmd(t, "list", "-c", "contoso", "-o", "yaml"); err == nil {
		t.Fatal("an unknown -o value should be rejected")
	}
}

// TestHelpNeverTellsYouToPressSlash walks every command's user-facing help and
// fails if any of it still describes the old huh picker.
//
// That instruction has now been wrong twice: the picker was replaced with an
// fzf-style model where typing filters directly, but "/" mentions survived in
// the root quick start, an Example block and a Long description, so the help
// was teaching a key that does nothing. A grep in a test is the cheapest way to
// stop it coming back a third time.
func TestHelpNeverTellsYouToPressSlash(t *testing.T) {
	stale := []string{
		`"/" filters`,
		`press "/"`,
		"/ to filter",
		"/ filter",
		"type to filter, space toggles", // the huh-era help line.
	}

	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, field := range []struct{ name, text string }{
			{"Short", c.Short},
			{"Long", c.Long},
			{"Example", c.Example},
		} {
			lower := strings.ToLower(field.text)
			for _, bad := range stale {
				if strings.Contains(lower, strings.ToLower(bad)) {
					t.Errorf("%s %s still mentions %q — the picker filters as you type",
						c.CommandPath(), field.name, bad)
				}
			}
		}
		c.Flags().VisitAll(func(f *pflag.Flag) {
			for _, bad := range stale {
				if strings.Contains(strings.ToLower(f.Usage), strings.ToLower(bad)) {
					t.Errorf("%s --%s usage still mentions %q", c.CommandPath(), f.Name, bad)
				}
			}
		})
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(NewRootCmd())
}

// TestHelpTeachesTheCurrentPickerKeys is the positive half: the keys the picker
// really binds have to appear somewhere a user will find them.
func TestHelpTeachesTheCurrentPickerKeys(t *testing.T) {
	root := NewRootCmd()
	var all strings.Builder
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		all.WriteString(c.Short + "\n" + c.Long + "\n" + c.Example + "\n")
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)

	text := strings.ToLower(all.String())
	if !strings.Contains(text, "type to filter") {
		t.Error("no command help tells the user that typing filters the picker")
	}
	// And the picker's own header stays the source of truth for the key list.
	for _, want := range []string{"type to filter", "tab toggle", "ctrl+a all", "enter confirm"} {
		if !strings.Contains(picker.NewModel("x", nil, 5).View(), want) {
			t.Errorf("the picker header no longer mentions %q", want)
		}
	}
}
