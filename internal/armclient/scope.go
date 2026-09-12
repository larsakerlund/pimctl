// String work on the identifiers and durations ARM deals in: role definition
// ids and the scope they are qualified against, scope types, the ISO-8601
// activation window, and the middle truncation the tables share. Nothing here
// calls ARM or holds a client; the calls are in client.go.

package armclient

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// roleDefSegment is what sits between a scope and the role definition GUID it
// qualifies. It is spelled exactly as ARM spells it, capitals included, because
// [QualifyRoleDefinitionID] builds an id ARM has to accept, not one it only has
// to recognise.
const roleDefSegment = "/providers/Microsoft.Authorization/roleDefinitions/"

// RoleDefinitionGUID returns the trailing GUID of a role definition id. Role
// definition ids are scope-qualified ("/subscriptions/x/providers/.../<guid>"
// at subscription scope, bare "/providers/.../<guid>" at management-group
// scope) but the GUID is the same role everywhere, so it is what pimctl keys
// caches and presets on.
func RoleDefinitionGUID(id string) string {
	i := strings.LastIndex(id, "/")
	if i < 0 {
		return id
	}
	return id[i+1:]
}

// QualifyRoleDefinitionID re-qualifies a role definition id to the scope an
// activation is being requested at.
//
// When the activation scope equals the eligibility's own scope the eligibility's
// id is returned verbatim, including the management-group case where ARM stores
// the tenant-level "/providers/Microsoft.Authorization/roleDefinitions/…" form
// rather than an MG-prefixed one. This is the path v1 always takes, and the live
// E2E proved ARM accepts it (five management-group activations succeeded).
//
// The re-qualifying branch below is what 16 of the tenant's 17 historical
// SelfActivate requests used — they activated at a narrower scope than the
// eligibility. Nothing in v1 reaches it, because the activation scope is always
// the eligibility's own; it exists, with test coverage, for when activation-scope
// selection is added.
func QualifyRoleDefinitionID(activationScope, eligibilityScope, roleDefinitionID string) string {
	if strings.EqualFold(activationScope, eligibilityScope) {
		return roleDefinitionID
	}
	return strings.TrimSuffix(activationScope, "/") + roleDefSegment + RoleDefinitionGUID(roleDefinitionID)
}

// NormalizeScopeType turns ARM's lowercase scope type into the display form
// used in tables. When ARM omits the type it is inferred from the scope id.
func NormalizeScopeType(armType, scopeID string) string {
	switch strings.ToLower(armType) {
	case "subscription":
		return "Subscription"
	case "managementgroup":
		return "ManagementGroup"
	case "resourcegroup":
		return "ResourceGroup"
	case "resource":
		return "Resource"
	case "":
		// Fall through and infer the type from the scope id instead.
	default:
		return armType
	}
	switch {
	case scopeID == "":
		return ""
	case strings.Contains(scopeID, "/providers/Microsoft.Management/managementGroups/"):
		return "ManagementGroup"
	case strings.Contains(strings.ToLower(scopeID), "/resourcegroups/"):
		if strings.Count(scopeID, "/providers/") > 0 {
			return "Resource"
		}
		return "ResourceGroup"
	case strings.HasPrefix(scopeID, "/subscriptions/"):
		return "Subscription"
	}
	return "Resource"
}

// isoDurationRE matches the ISO-8601 durations a PIM policy's maximumDuration
// actually uses: days, hours, minutes and seconds. Years and months are left
// out of the pattern on purpose — an activation window never uses them, and
// neither has a fixed length to turn into a [time.Duration].
var isoDurationRE = regexp.MustCompile(`^P(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+)S)?)?$`)

// MaxISODuration bounds what [ParseISODuration] accepts, in total and per
// component. A PIM activation window is at most 24 hours and a permanent
// eligibility is expressed without a duration at all, so a thousand days is
// beyond any value a policy or a user can mean; the bound exists because a
// component large enough to wrap a [time.Duration] when multiplied by its unit
// would otherwise parse into a nonsense window instead of an error.
const MaxISODuration = 1000 * 24 * time.Hour

// ParseISODuration parses the ISO-8601 durations PIM policies use for
// maximumDuration — PT1H, PT4H, PT1H30M, PT30M, P1D. Years and months are not
// accepted because activation windows never use them and their length is
// ambiguous. It returns an error for an empty or malformed string, and for
// any component or total beyond [MaxISODuration].
func ParseISODuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0, errors.New("empty duration")
	}
	m := isoDurationRE.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf(
			"%q is not a supported ISO-8601 duration (expected forms like PT1H, PT4H, PT1H30M, PT30M, P1D)",
			s,
		)
	}
	if m[1] == "" && m[2] == "" && m[3] == "" && m[4] == "" {
		return 0, fmt.Errorf("%q has no duration components", s)
	}
	var d time.Duration
	for i, unit := range []time.Duration{24 * time.Hour, time.Hour, time.Minute, time.Second} {
		if m[i+1] == "" {
			continue
		}
		n, err := strconv.ParseInt(m[i+1], 10, 64)
		// The bound is checked on the count, before the multiply, so a value
		// that would wrap the product is caught by the same comparison as one
		// that merely exceeds the limit.
		if err != nil || n > int64(MaxISODuration/unit) {
			return 0, errTooLong(s)
		}
		d += time.Duration(n) * unit
	}
	if d > MaxISODuration {
		return 0, errTooLong(s)
	}
	return d, nil
}

// errTooLong is the error for a duration beyond [MaxISODuration], quoting the
// input and the bound in days, which is the unit the bound is stated in.
func errTooLong(s string) error {
	return fmt.Errorf("%q is longer than the %d days pimctl accepts", s, MaxISODuration/(24*time.Hour))
}

// FormatISODuration renders a duration in the PT#H#M#S form ARM expects.
func FormatISODuration(d time.Duration) string {
	if d <= 0 {
		return "PT0S"
	}
	d = d.Round(time.Second)
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	s := int((d % time.Minute) / time.Second)
	out := "PT"
	if h > 0 {
		out += strconv.Itoa(h) + "H"
	}
	if m > 0 {
		out += strconv.Itoa(m) + "M"
	}
	if s > 0 || out == "PT" {
		out += strconv.Itoa(s) + "S"
	}
	return out
}

// TruncateMiddle shortens a string to width runes for table display, keeping
// both ends around a single "…". It counts and cuts in runes, not bytes: a
// role or scope name with an "å" or an emoji in it cut on a byte boundary
// would leave half a code point in the table.
func TruncateMiddle(s string, width int) string {
	r := []rune(s)
	if width <= 3 || len(r) <= width {
		return s
	}
	keep := width - 1
	head := keep / 2
	tail := keep - head
	return string(r[:head]) + "…" + string(r[len(r)-tail:])
}
