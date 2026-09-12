// Tests for the activation length flag: the spellings --for accepts and the
// values that must be rejected rather than quietly meaning "the policy
// maximum".

package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestResolveDuration(t *testing.T) {
	cases := []struct {
		name       string
		requested  time.Duration
		max        time.Duration
		want       time.Duration
		wantCapped bool
	}{
		{"no request uses the policy maximum PT1H", 0, time.Hour, time.Hour, false},
		{"no request uses the policy maximum PT10H", 0, 10 * time.Hour, 10 * time.Hour, false},
		{"request under the maximum is honoured", time.Hour, 4 * time.Hour, time.Hour, false},
		{"request over the maximum is clamped", 8 * time.Hour, 4 * time.Hour, 4 * time.Hour, true},
		{"request over a PT1H maximum is clamped hard", 4 * time.Hour, time.Hour, time.Hour, true},
		{"exactly the maximum is not flagged as capped", 90 * time.Minute, 90 * time.Minute, 90 * time.Minute, false},
		{"fractional request under a PT1H30M maximum", 30 * time.Minute, 90 * time.Minute, 30 * time.Minute, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, capped := resolveDuration(c.requested, c.max)
			if got != c.want || capped != c.wantCapped {
				t.Fatalf("ResolveDuration(%v, %v) = (%v, %v), want (%v, %v)",
					c.requested, c.max, got, capped, c.want, c.wantCapped)
			}
		})
	}
}

// durationCmd builds a command whose --for flag reports as explicitly set
// only when the caller says so, mirroring cobra's Changed() semantics.
func durationCmd(t *testing.T, set map[string]string) *cobra.Command {
	t.Helper()
	c := &cobra.Command{Use: "activate"}
	var f string
	c.Flags().StringVar(&f, "for", "", "")
	for k, v := range set {
		if err := c.Flags().Set(k, v); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func TestRequestedDurationUnset(t *testing.T) {
	if d, err := requestedDuration(durationCmd(t, nil), ""); err != nil || d != 0 {
		t.Errorf("no flags -> (%v, %v), want (0, nil) meaning the policy maximum", d, err)
	}
	if d, err := requestedDuration(nil, ""); err != nil || d != 0 {
		t.Errorf("nil command -> (%v, %v), want (0, nil) meaning the policy maximum", d, err)
	}
}

func TestParseFriendlyDuration(t *testing.T) {
	cases := map[string]time.Duration{
		"2h":       2 * time.Hour,
		"90m":      90 * time.Minute,
		"1h30m":    90 * time.Minute,
		"45s":      45 * time.Second,
		"1h30m10s": time.Hour + 30*time.Minute + 10*time.Second,
		"PT2H30M":  150 * time.Minute,
		"pt1h":     time.Hour,
		"P1D":      24 * time.Hour,
		" 2h ":     2 * time.Hour, //nolint:gocritic // the padding is what this case exercises
		"2H":       2 * time.Hour,
	}
	for in, want := range cases {
		got, err := parseFriendlyDuration(in)
		if err != nil {
			t.Errorf("ParseFriendlyDuration(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseFriendlyDuration(%q) = %v, want %v", in, got, want)
		}
	}
	for _, bad := range []string{"", "two hours", "2 hours", "h", "2x", "-2h", "PT", "2h30"} {
		if _, err := parseFriendlyDuration(bad); err == nil {
			t.Errorf("ParseFriendlyDuration(%q) should have failed", bad)
		}
	}
}

func TestRequestedDurationFor(t *testing.T) {
	mk := func(v string) *cobra.Command { return durationCmd(t, map[string]string{"for": v}) }
	if d, err := requestedDuration(mk("2h"), "2h"); err != nil || d != 2*time.Hour {
		t.Errorf("--for 2h -> (%v, %v)", d, err)
	}
	if d, err := requestedDuration(mk("1h30m"), "1h30m"); err != nil || d != 90*time.Minute {
		t.Errorf("--for 1h30m -> (%v, %v)", d, err)
	}
	if d, err := requestedDuration(mk("PT2H30M"), "PT2H30M"); err != nil || d != 150*time.Minute {
		t.Errorf("--for PT2H30M -> (%v, %v)", d, err)
	}
	for _, zero := range []string{"0m", "0s", "PT0S"} {
		_, err := requestedDuration(mk(zero), zero)
		if err == nil {
			t.Errorf("--for %s must be rejected, not read as 'use the maximum'", zero)
		} else if err.Error() != "--for must be greater than zero (omit it to use each role's policy maximum)" {
			t.Errorf("--for %s error should explain: %q", zero, err.Error())
		}
	}
	for _, bad := range []string{"banana", "two hours", ""} {
		_, err := requestedDuration(mk(bad), bad)
		if err == nil {
			t.Errorf("--for %q must be rejected", bad)
		} else if !strings.HasPrefix(err.Error(), "--for: ") {
			t.Errorf("--for %q error should name the flag: %q", bad, err.Error())
		}
	}
}
