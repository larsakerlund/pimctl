// Turning the activation length the user typed with --for into a duration.
// Capping that duration to each role's own policy maximum happens later, in
// [BuildPlan].

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
		d, err := armclient.ParseISODuration(upper)
		if err != nil {
			return 0, err
		}
		// A day count past about 106,751 overflows time.Duration and comes
		// back negative; that is not a duration anyone meant, and letting it
		// through would have the caller report it as "must be greater than
		// zero", which sends the reader at the wrong problem.
		if d < 0 {
			return 0, fmt.Errorf("%q is not a duration I understand (try 2h, 90m, 1h30m or PT2H30M)", s)
		}
		return d, nil
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

// requestedDuration turns --for into a duration. Zero means "use each role's
// policy maximum" — which is why an explicitly given zero or negative value
// has to be rejected rather than quietly meaning the longest window the
// policy allows. The flag is read through cmd's Changed() so that an
// unset flag and an explicitly empty one are told apart.
func requestedDuration(cmd *cobra.Command, forVal string) (time.Duration, error) {
	if cmd == nil || !cmd.Flags().Changed("for") {
		return 0, nil
	}
	d, err := parseFriendlyDuration(forVal)
	if err != nil {
		return 0, fmt.Errorf("--for: %w", err)
	}
	if d <= 0 {
		return 0, errors.New("--for must be greater than zero (omit it to use each role's policy maximum)")
	}
	return d, nil
}
