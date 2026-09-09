// Covers errors.go: the classification of each ARM code and message wording
// pimctl reacts to, the claims challenge wherever ARM hides it, and the
// recovery command. What the retry loop then does with those kinds belongs to
// client_test.go.

package armclient

import (
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestParseAPIErrorAlreadyActive(t *testing.T) {
	body := []byte(`{"error":{"code":"RoleAssignmentExists","message":"The Role assignment already exists."}}`)
	e := ParseAPIError("PUT", "/x", 400, body, http.Header{})
	if e.Kind != KindAlreadyActive {
		t.Fatalf("kind = %v, want KindAlreadyActive", e.Kind)
	}

	// Some tenants report the same condition with a different code but the
	// same phrasing; the message must be enough on its own.
	body = []byte(
		`{"error":{"code":"BadRequest","message":"The Role assignment already exists at scope /subscriptions/x."}}`,
	)
	if e := ParseAPIError("PUT", "/x", 400, body, http.Header{}); e.Kind != KindAlreadyActive {
		t.Fatalf("message-based detection failed: kind = %v", e.Kind)
	}
}

func TestParseAPIErrorPolicyValidation(t *testing.T) {
	body := []byte(
		`{"error":{"code":"RoleAssignmentRequestPolicyValidationFailed","message":"The following policy rules failed: [\"ExpirationRule\",\"JustificationRule\"]"}}`,
	)
	e := ParseAPIError("PUT", "/x", 400, body, http.Header{})
	if e.Kind != KindPolicyValidation {
		t.Fatalf("kind = %v", e.Kind)
	}
	if len(e.FailedRules) != 2 || e.FailedRules[0] != "ExpirationRule" || e.FailedRules[1] != "JustificationRule" {
		t.Fatalf("FailedRules = %v", e.FailedRules)
	}
}

func TestParseAPIErrorClaimsChallengeFromBody(t *testing.T) {
	claimsJSON := `{"access_token":{"acrs":{"essential":true,"value":"c1"}}}`
	body := []byte(`{"error":{"code":"RoleAssignmentRequestAcrsValidationFailed","message":"claims=` +
		strings.ReplaceAll(claimsJSON, `"`, `\"`) + `"}}`)
	e := ParseAPIError("PUT", "/x", 403, body, http.Header{})
	if e.Kind != KindClaimsChallenge {
		t.Fatalf("kind = %v, want KindClaimsChallenge", e.Kind)
	}
	if e.Claims == "" {
		t.Fatal("no claims extracted")
	}
	dec, err := base64.StdEncoding.DecodeString(e.Claims)
	if err != nil {
		t.Fatalf("claims are not base64: %v", err)
	}
	if string(dec) != claimsJSON {
		t.Fatalf("decoded claims =\n %s\nwant %s", dec, claimsJSON)
	}
	cmd := e.RecoveryCommand("contoso", "11111111-2222-3333-4444-555555555555")
	for _, want := range []string{
		"cloudctx exec contoso -- az login",
		"--tenant 11111111-2222-3333-4444-555555555555",
		`--scope "https://management.core.windows.net//.default"`,
		`--claims-challenge "` + e.Claims + `"`,
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("recovery command missing %q:\n%s", want, cmd)
		}
	}
}

func TestParseAPIErrorClaimsChallengeFromHeader(t *testing.T) {
	claimsJSON := `{"access_token":{"acrs":{"essential":true,"value":"c1"}}}`
	header := http.Header{}
	header.Set(
		"WWW-Authenticate",
		`Bearer realm="", authorization_uri="https://login.windows.net/common/oauth2/authorize", error="insufficient_claims", claims="`+
			base64.StdEncoding.EncodeToString(
				[]byte(claimsJSON),
			)+`"`,
	)
	e := ParseAPIError("PUT", "/x", 401, []byte(`{"error":{"code":"Unauthorized","message":"denied"}}`), header)
	if e.Kind != KindClaimsChallenge {
		t.Fatalf("kind = %v", e.Kind)
	}
	// An already-base64 challenge must be passed through untouched.
	if e.Claims != base64.StdEncoding.EncodeToString([]byte(claimsJSON)) {
		t.Fatalf("base64 claims were re-encoded: %q", e.Claims)
	}
}

func TestRecoveryCommandOnlyForClaimsChallenges(t *testing.T) {
	e := ParseAPIError("PUT", "/x", 400, []byte(`{"error":{"code":"SomethingElse","message":"nope"}}`), http.Header{})
	if got := e.RecoveryCommand("contoso", "tid"); got != "" {
		t.Fatalf("unrelated error produced a recovery command: %q", got)
	}
}

func TestParseAPIErrorPassesUnknownErrorsThroughVerbatim(t *testing.T) {
	body := []byte(`{"error":{"code":"InvalidResourceType","message":"The resource type could not be found."}}`)
	e := ParseAPIError("GET", "/x", 404, body, http.Header{})
	if e.Kind != KindOther {
		t.Fatalf("kind = %v, want KindOther", e.Kind)
	}
	if e.Error() != "InvalidResourceType: The resource type could not be found." {
		t.Fatalf("error text = %q", e.Error())
	}
}

func TestParseAPIErrorNonJSONBody(t *testing.T) {
	e := ParseAPIError("GET", "/x", 502, []byte("<html>bad gateway</html>"), http.Header{})
	if !strings.Contains(e.Error(), "502") || !strings.Contains(e.Error(), "bad gateway") {
		t.Fatalf("a non-JSON body must still be reported: %q", e.Error())
	}
}

func TestEncodeClaims(t *testing.T) {
	raw := `{"access_token":{"acrs":{"essential":true,"value":"c1"}}}`
	enc := EncodeClaims(raw)
	if enc == raw {
		t.Fatal("raw JSON should have been base64-encoded")
	}
	if EncodeClaims(enc) != enc {
		t.Fatal("already-base64 input should be passed through unchanged")
	}
	if EncodeClaims("") != "" {
		t.Fatal("empty in, empty out")
	}
}

func TestParseAPIErrorDeactivationCodes(t *testing.T) {
	// Observed live: deactivating within seconds of activating races the
	// assignment's propagation, and PIM enforces a five-minute minimum.
	e := ParseAPIError(
		"PUT",
		"/x",
		400,
		[]byte(
			`{"error":{"code":"RoleAssignmentDoesNotExist","message":"The Role assignment does not exist."}}`,
		),
		http.Header{},
	)
	if e.Kind != KindNotActive {
		t.Errorf("RoleAssignmentDoesNotExist kind = %v, want KindNotActive", e.Kind)
	}

	e = ParseAPIError(
		"PUT",
		"/x",
		400,
		[]byte(
			`{"error":{"code":"ActiveDurationTooShort","message":"The Active duration is too short. Miniumum Required is 5 minutes."}}`,
		),
		http.Header{},
	)
	if e.Kind != KindMinimumDuration {
		t.Errorf("ActiveDurationTooShort kind = %v, want KindMinimumDuration", e.Kind)
	}
	if e.Kind == KindAlreadyActive {
		t.Error("ActiveDurationTooShort must not be mistaken for already-active")
	}
}

func TestParseAPIErrorRecordsRetryAfter(t *testing.T) {
	// Seconds, the form ARM actually sends.
	h := http.Header{}
	h.Set("Retry-After", "7")
	e := ParseAPIError("GET", "/x", 429, []byte(`{}`), h)
	if !e.HasRetryAfter || e.RetryAfter != 7*time.Second {
		t.Errorf("RetryAfter = %v (set %v), want 7s", e.RetryAfter, e.HasRetryAfter)
	}

	// Zero is a real answer — retry immediately — and must not read as absent.
	h.Set("Retry-After", "0")
	zero := ParseAPIError("GET", "/x", 429, []byte(`{}`), h)
	if !zero.HasRetryAfter || zero.RetryAfter != 0 {
		t.Errorf("Retry-After: 0 must be recorded as set and zero: %v/%v", zero.RetryAfter, zero.HasRetryAfter)
	}

	// The HTTP-date form RFC 9110 also allows.
	h.Set("Retry-After", time.Now().Add(30*time.Second).UTC().Format(http.TimeFormat))
	e = ParseAPIError("GET", "/x", 429, []byte(`{}`), h)
	if !e.HasRetryAfter || e.RetryAfter < 25*time.Second || e.RetryAfter > 30*time.Second {
		t.Errorf("an HTTP-date Retry-After gave %v, want about 30s", e.RetryAfter)
	}

	// A date already past means "now", never a negative wait.
	h.Set("Retry-After", time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat))
	past := ParseAPIError("GET", "/x", 429, []byte(`{}`), h)
	if !past.HasRetryAfter || past.RetryAfter != 0 {
		t.Errorf("a past Retry-After gave %v, want 0", past.RetryAfter)
	}

	// No header at all: nothing to honour, and the field says so.
	absent := ParseAPIError("GET", "/x", 429, []byte(`{}`), http.Header{})
	if absent.HasRetryAfter {
		t.Errorf("no header must leave HasRetryAfter false, got %v", absent.RetryAfter)
	}
}
