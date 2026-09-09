// What a non-2xx ARM response means: parsing the error envelope, sorting the
// result into an [ErrorKind] the CLI can act on, and digging the Conditional
// Access claims challenge out of whichever of the header, the message or the
// raw body ARM put it in. Which kinds are worth retrying is client.go's
// decision, not this file's.

package armclient

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ErrorKind classifies an ARM failure so the CLI can react appropriately.
type ErrorKind int

const (
	// KindOther is any error pimctl has no special handling for. Its code and
	// message are always shown verbatim.
	KindOther ErrorKind = iota
	// KindAlreadyActive means the role is already activated. Not a failure.
	KindAlreadyActive
	// KindPolicyValidation means the PIM policy rejected the request; the
	// failing rule names are extracted from the message.
	KindPolicyValidation
	// KindClaimsChallenge means Conditional Access wants a stepped-up token.
	KindClaimsChallenge
	// KindNotActive means the role assignment being deactivated is not there.
	// PIM also reports this for a few seconds after an activation, before the
	// assignment has finished propagating.
	KindNotActive
	// KindMinimumDuration means PIM refuses the operation because the role has
	// not been active for its minimum five minutes yet.
	KindMinimumDuration
)

// APIError is a non-2xx ARM response.
type APIError struct {
	StatusCode int    // the HTTP status; 429 and 5xx are what the retry loop acts on.
	Code       string // ARM's error code, e.g. "RoleAssignmentExists"; empty when ARM did not use its envelope.
	Message    string // ARM's own message, shown to the user verbatim.
	// Body is the raw response body, kept for errors ARM does not wrap in the
	// standard {error:{code,message}} envelope.
	Body string
	// WWWAuthenticate is the response header, if any.
	WWWAuthenticate string
	// Claims is the base64 claims challenge, if one was found. Already
	// base64-encoded and ready to pass to `az login --claims-challenge`.
	Claims string
	// FailedRules holds the policy rule names from a policy validation failure.
	FailedRules []string
	// Kind is the classification the CLI switches on, so no caller has to match
	// on ARM's code strings itself.
	Kind ErrorKind
	// Method and URL identify the failed call.
	Method string // the HTTP method, for the message.
	URL    string // the URL with its query redacted, safe to print.
	// RetryAfter is how long ARM asked us to wait, from the Retry-After header
	// of a throttled response. Meaningful only when HasRetryAfter is set: the
	// header may legitimately say zero, which is not the same as absent.
	RetryAfter time.Duration
	// HasRetryAfter reports whether ARM sent a Retry-After header at all.
	HasRetryAfter bool
}

// parseRetryAfter reads the Retry-After header in either form RFC 9110 allows:
// a whole number of seconds, or an HTTP date. ok is false when the header is
// absent or unparseable — distinct from a header that says zero, which means
// "retry immediately" and which ARM does send.
//
// A date already in the past yields zero rather than a negative wait.
func parseRetryAfter(header http.Header) (d time.Duration, ok bool) {
	v := strings.TrimSpace(header.Get("Retry-After"))
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(time.Until(t), 0), true
	}
	return 0, false
}

// Error renders the failure the way it should read on a terminal: ARM's own
// code and message when there is one, the raw body when ARM did not use its
// standard envelope, and for a 429 that survived every retry a sentence saying
// so, because "HTTP 429" alone does not tell an operator to wait.
func (e *APIError) Error() string {
	switch {
	case e.Code != "" && e.Message != "":
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	case e.Message != "":
		return e.Message
	case e.StatusCode == http.StatusTooManyRequests:
		return "ARM is throttling this account (HTTP 429); pimctl retried and still could not get through — wait a minute and try again"
	case e.Body != "":
		return fmt.Sprintf("HTTP %d: %s", e.StatusCode, strings.TrimSpace(e.Body))
	default:
		return fmt.Sprintf("HTTP %d", e.StatusCode)
	}
}

// RecoveryCommand returns the `cloudctx exec … az login --claims-challenge …`
// command that re-mints a token satisfying the Conditional Access challenge.
// It returns "" when this error is not a claims challenge.
func (e *APIError) RecoveryCommand(context, tenantID string) string {
	if e.Kind != KindClaimsChallenge {
		return ""
	}
	login := fmt.Sprintf(`az login --tenant %s --scope "https://management.core.windows.net//.default"`, tenantID)
	if e.Claims != "" {
		login += fmt.Sprintf(" --claims-challenge %q", e.Claims)
	}
	if context == "" {
		return login
	}
	return fmt.Sprintf("cloudctx exec %s -- %s", context, login)
}

// armErrorEnvelope is ARM's standard {"error":{"code","message"}} wrapper. Not
// every failure arrives in it, which is why [APIError.Body] keeps the raw text
// as well and [APIError.Error] falls back to it.
type armErrorEnvelope struct {
	// Error is the standard envelope. A response that does not use it decodes
	// to the zero value rather than failing.
	Error struct {
		Code    string `json:"code"`    // the machine-readable code.
		Message string `json:"message"` // the human-readable message.
	} `json:"error"`
}

// alreadyActiveCodes are the ARM error codes meaning "you already hold this".
var alreadyActiveCodes = map[string]bool{
	"RoleAssignmentExists":                true,
	"RoleAssignmentRequestExists":         true,
	"RoleAssignmentScheduleRequestExists": true,
	"RoleAssignmentScheduleExists":        true,
}

// alreadyActivePhrases are the message wordings that mean the same thing as an
// [alreadyActiveCodes] entry. They are matched as well as the codes because
// some tenants report the condition under a plain BadRequest, where the
// phrasing is the only thing left to go on.
var alreadyActivePhrases = []string{
	"role assignment already exists",
	"already active",
	"is already assigned",
	"already has an active",
}

// failedRulesRE captures the bracketed list a policy validation message ends
// with, so [extractFailedRules] can name the rules the request fell foul of
// instead of echoing the whole sentence at the user.
var failedRulesRE = regexp.MustCompile(`\[([^\]]*)\]`)

// claimsMarkerRE locates a `claims=` marker in an ARM body or a
// WWW-Authenticate header. What follows it is scanned by hand because the value
// is a JSON object that may be bare, quoted, or backslash-escaped inside an
// enclosing JSON string.
var claimsMarkerRE = regexp.MustCompile(`(?i)claims\s*=\s*`)

// ParseAPIError turns an ARM response into a classified APIError.
func ParseAPIError(method, url string, status int, body []byte, header http.Header) *APIError {
	e := &APIError{
		StatusCode:      status,
		Body:            string(body),
		WWWAuthenticate: header.Get("WWW-Authenticate"),
		Method:          method,
		URL:             url,
	}
	e.RetryAfter, e.HasRetryAfter = parseRetryAfter(header)
	var env armErrorEnvelope
	if err := json.Unmarshal(body, &env); err == nil {
		e.Code = env.Error.Code
		e.Message = env.Error.Message
	}
	lowerMsg := strings.ToLower(e.Message + " " + e.Body)

	switch {
	case alreadyActiveCodes[e.Code]:
		e.Kind = KindAlreadyActive
	case containsAny(lowerMsg, alreadyActivePhrases):
		e.Kind = KindAlreadyActive
	case e.Code == "RoleAssignmentDoesNotExist":
		e.Kind = KindNotActive
	case e.Code == "ActiveDurationTooShort":
		e.Kind = KindMinimumDuration
	case e.Code == "RoleAssignmentRequestAcrsValidationFailed":
		e.Kind = KindClaimsChallenge
	case e.Code == "RoleAssignmentRequestPolicyValidationFailed":
		e.Kind = KindPolicyValidation
		e.FailedRules = extractFailedRules(e.Message)
	}

	if c := extractClaims(e.WWWAuthenticate, e.Message, string(body)); c != "" {
		e.Claims = c
		if status == 401 || status == 403 || e.Kind == KindClaimsChallenge {
			e.Kind = KindClaimsChallenge
		}
	} else if (status == 401 || status == 403) && strings.Contains(lowerMsg, "claims") {
		e.Kind = KindClaimsChallenge
	}
	return e
}

// containsAny reports whether s contains any of needles. It folds no case of
// its own: the only caller lower-cases s first, and the needles are written in
// lower case to match.
func containsAny(s string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// extractFailedRules pulls rule names out of messages like
// `The following policy rules failed: ["MfaRule","ExpirationRule"]`.
func extractFailedRules(msg string) []string {
	m := failedRulesRE.FindStringSubmatch(msg)
	if m == nil {
		return nil
	}
	var rules []string
	for part := range strings.SplitSeq(m[1], ",") {
		r := strings.Trim(strings.TrimSpace(part), `"'`)
		if r != "" {
			rules = append(rules, r)
		}
	}
	return rules
}

// extractClaims finds a claims challenge in any of the given sources — the
// WWW-Authenticate header, the parsed error message, or the raw body — and
// returns it base64-encoded, ready for `az login --claims-challenge`. A
// challenge that is already base64 is passed through unchanged.
func extractClaims(sources ...string) string {
	for _, src := range sources {
		if src == "" {
			continue
		}
		if raw := findClaimsValue(src); raw != "" {
			return EncodeClaims(unescapeJSONString(raw))
		}
	}
	return ""
}

// findClaimsValue returns the raw text following the first `claims=` marker.
func findClaimsValue(s string) string {
	loc := claimsMarkerRE.FindStringIndex(s)
	if loc == nil {
		return ""
	}
	rest := s[loc[1]:]
	// Step over an opening quote, and the backslash of an escaped one.
	for rest != "" && (rest[0] == '"' || rest[0] == '\'' || rest[0] == '\\') {
		rest = rest[1:]
	}
	if rest == "" {
		return ""
	}
	if rest[0] == '{' {
		return scanBalancedObject(rest)
	}
	end := strings.IndexAny(rest, `,"' `)
	if end < 0 {
		return rest
	}
	return rest[:end]
}

// scanBalancedObject returns the leading JSON object of s, tolerating the
// backslash escaping ARM applies when the object is embedded in a JSON string.
func scanBalancedObject(s string) string {
	depth, inString, escaped := 0, false, false
	for i := range len(s) {
		c := s[i]
		if escaped {
			escaped = false
			continue
		}
		switch c {
		case '\\':
			escaped = true
		case '"':
			inString = !inString
		case '{':
			if !inString {
				depth++
			}
		case '}':
			if !inString {
				depth--
				if depth == 0 {
					return s[:i+1]
				}
			}
		}
	}
	return ""
}

// unescapeJSONString undoes one level of JSON string escaping.
func unescapeJSONString(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	r := strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\/`, `/`)
	return r.Replace(s)
}

// EncodeClaims base64-encodes a raw claims JSON blob. If the value already
// decodes to JSON it is assumed to be base64 already and returned unchanged.
func EncodeClaims(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "{") {
		return base64.StdEncoding.EncodeToString([]byte(raw))
	}
	// Looks like it might already be base64: verify it decodes to JSON.
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if dec, err := enc.DecodeString(raw); err == nil && json.Valid(dec) {
			return raw
		}
	}
	return base64.StdEncoding.EncodeToString([]byte(raw))
}
