// Turning the activation length the user typed into a duration: --for and the
// two deprecated spellings it supersedes. Capping that duration to each role's
// own policy maximum happens later, in [BuildPlan].

package cli

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/larsakerlund/pimctl/internal/armclient"
)

// goDurationRE matches the friendly forms: 2h, 90m, 1h30m, 45s, 1h30m10s.
var goDurationRE = regexp.MustCompile(`^(?:\d+h)?(?:\d+m)?(?:\d+s)?$`)

// parseFriendlyDuration accepts either the everyday form people actually type
// (2h, 90m, 1h30m) or the ISO-8601 form ARM speaks (PT2H30M).
func parseFriendlyDuration(s string) (time.Duration, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return 0, errors.New("empty duration")
	}
	upper := strings.ToUpper(trimmed)
	if strings.HasPrefix(upper, "P") {
		return armclient.ParseISODuration(upper)
	}
	lower := strings.ToLower(trimmed)
	if !goDurationRE.MatchString(lower) {
		return 0, fmt.Errorf("%q is not a duration I understand (try 2h, 90m, 1h30m or PT2H30M)", s)
	}
	d, err := time.ParseDuration(lower)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration I understand (try 2h, 90m, 1h30m or PT2H30M)", s)
	}
	return d, nil
}

// requestedDuration turns --for (or the deprecated --hours/--duration) into a
// duration. Zero means "use each role's policy maximum" — which is why an
// explicitly given zero or negative value has to be rejected rather than
// quietly meaning the longest window the policy allows.
func requestedDuration(cmd *cobra.Command, forVal string, hours float64, iso string) (time.Duration, error) {
	changed := func(name string) bool { return cmd != nil && cmd.Flags().Changed(name) }
	given := 0
	for _, n := range []string{"for", "hours", "duration"} {
		if changed(n) {
			given++
		}
	}
	if given > 1 {
		return 0, errors.New("give only one of --for, --hours or --duration (--for supersedes the other two)")
	}

	switch {
	case changed("for"):
		d, err := parseFriendlyDuration(forVal)
		if err != nil {
			return 0, fmt.Errorf("--for: %w", err)
		}
		if d <= 0 {
			return 0, errors.New("--for must be greater than zero (omit it to use each role's policy maximum)")
		}
		return d, nil
	case changed("hours"):
		if hours <= 0 {
			return 0, errors.New("--hours must be greater than zero (omit the flag to use each role's policy maximum)")
		}
		return time.Duration(hours * float64(time.Hour)), nil
	case changed("duration"):
		d, err := armclient.ParseISODuration(iso)
		if err != nil {
			return 0, fmt.Errorf("--duration: %w", err)
		}
		if d <= 0 {
			return 0, errors.New(
				"--duration must be greater than zero (omit the flag to use each role's policy maximum)",
			)
		}
		return d, nil
	}
	return 0, nil
}
