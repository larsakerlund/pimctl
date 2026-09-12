// The calls themselves and the HTTP layer under them: one client per token, the
// retry loop, nextLink paging, the listing, policy, submit and read-back
// endpoints, and the poll to a terminal status. The wire types are in types.go
// and the classification of a failed response in errors.go.

package armclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Client is an ARM PIM client bound to a single access token (and therefore a
// single tenant / cloudctx context).
type Client struct {
	Host string       // ARM endpoint, [DefaultHost] in production and a test server's URL in tests.
	HTTP *http.Client // carries the timeout and the connection pool; never nil.
	// MaxRetries is how many times a throttled or transiently failing call is
	// retried before the error is returned. [New] sets [DefaultMaxRetries];
	// zero means no retry at all, which is a legitimate thing to ask for.
	MaxRetries int
	// tokenMu guards token, which is replaced when a 401 forces a re-mint while
	// other goroutines are mid-fan-out.
	tokenMu sync.Mutex
	token   string // the bearer token, read through bearer() and replaced by SetToken.
	// policyMu guards policyCach, which the plan builder fills from several
	// goroutines at once.
	policyMu sync.Mutex
	// policyCache memoises policies per (scope, role GUID) for the life of the
	// client, so a batch activating several roles at one scope reads each
	// policy once.
	policyCache map[string]*RoleSettings
}

// DefaultHTTPTimeout bounds a single ARM call. ARM's slowest listing on this
// tenant takes about 20 s; 60 s leaves room without hanging a shell.
const DefaultHTTPTimeout = 60 * time.Second

// New builds the client for one ARM token: that token is the only credential
// the client will ever send, and hc is wrapped in the origin guard from
// origin.go so no request built here can leave the ARM host. A nil hc gets a
// client with [DefaultHTTPTimeout]; an empty host means [DefaultHost].
func New(host, token string, hc *http.Client) *Client {
	if host == "" {
		host = DefaultHost
	}
	if hc == nil {
		hc = &http.Client{Timeout: DefaultHTTPTimeout}
	}
	c := &Client{
		Host:        strings.TrimSuffix(host, "/"),
		HTTP:        hc,
		MaxRetries:  DefaultMaxRetries,
		token:       token,
		policyCache: map[string]*RoleSettings{},
	}
	c.HTTP = c.guardedHTTPClient(hc)
	return c
}

// DefaultMaxRetries is what [New] puts in [Client.MaxRetries]: four attempts
// after the first, which covers the throttling bursts a rapid `up` provokes
// without turning a genuine outage into a minute of waiting.
const DefaultMaxRetries = 4

// retryableStatus reports whether ARM is asking us to come back later rather
// than reporting a real problem with the request.
func retryableStatus(code int) bool {
	return code == http.StatusTooManyRequests ||
		code == http.StatusServiceUnavailable ||
		code == http.StatusGatewayTimeout
}

// MaxRetryDelay caps the wait before any single retry. A Retry-After header is
// whatever ARM chose to send, and a value of hours would park the CLI for that
// long on every attempt; a minute is longer than any throttling window seen on
// this tenant and short enough that an operator can still tell the tool is
// alive. The [APIError.RetryAfter] a caller inspects is left as ARM sent it —
// only the sleep is clamped.
const MaxRetryDelay = 60 * time.Second

// retryDelay is how long to wait before the next attempt: what ARM asked for
// when it said, and exponential backoff from one second when it did not, both
// clamped to [MaxRetryDelay].
//
// It reads the wait off the parsed error rather than the raw header, so the
// RetryAfter field a caller can inspect is the same value the retry loop
// slept for, up to the cap.
func retryDelay(err *APIError, attempt int) time.Duration {
	if err != nil && err.HasRetryAfter {
		return min(err.RetryAfter, MaxRetryDelay)
	}
	return min(time.Duration(1<<attempt)*time.Second, MaxRetryDelay)
}

// SetToken replaces the bearer token, used after a 401 forces a re-mint.
func (c *Client) SetToken(token string) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	c.token = token
}

// bearer returns the token to put in the Authorization header. It takes the
// mutex because [Client.SetToken] swaps the token when a 401 forces a re-mint
// while other goroutines are still mid-fan-out and about to read it.
func (c *Client) bearer() string {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	return c.token
}

// do issues one ARM call, retrying throttling and transient failures.
func (c *Client) do(ctx context.Context, method, rawURL string, body any) ([]byte, error) {
	var lastErr error
	for attempt := 0; ; attempt++ {
		raw, _, err := c.doOnce(ctx, method, rawURL, body)
		if err == nil {
			return raw, nil
		}
		lastErr = err
		var ae *APIError
		if !errors.As(err, &ae) || !retryableStatus(ae.StatusCode) || attempt >= c.MaxRetries {
			return nil, lastErr
		}
		// The backoff is computed from attempt here, inside the loop, because
		// that is the only place the attempt number exists: computed anywhere
		// else, every retry without a Retry-After header would wait the same
		// first-attempt second.
		select {
		case <-ctx.Done():
			return nil, lastErr
		case <-time.After(retryDelay(ae, attempt)):
		}
	}
}

// doOnce issues a single call and returns the response headers alongside the
// body so the caller can honour Retry-After.
func (c *Client) doOnce(ctx context.Context, method, rawURL string, body any) ([]byte, http.Header, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, nil, fmt.Errorf("could not encode the request body: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return nil, nil, err
	}
	if destinationErr := c.validateDestination(req.URL); destinationErr != nil {
		return nil, nil, destinationErr
	}
	req.Header.Set("Authorization", "Bearer "+c.bearer())
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("%s %s: %w", method, redactURL(rawURL), err)
	}
	// The body is fully drained a line below; a close error after that tells us
	// nothing the read did not already say.
	defer resp.Body.Close() //nolint:errcheck // see above
	// One byte past the cap is read on purpose: it is how an over-long body is
	// told apart from one that is exactly the cap.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return nil, resp.Header, fmt.Errorf("%s %s: could not read the response: %w", method, redactURL(rawURL), err)
	}
	if len(raw) > MaxResponseBytes {
		return nil, resp.Header, fmt.Errorf(
			"%s %s: the response is larger than the %d MiB pimctl will read",
			method, redactURL(rawURL), MaxResponseBytes/mebibyte,
		)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, resp.Header, ParseAPIError(method, redactURL(rawURL), resp.StatusCode, raw, resp.Header)
	}
	return raw, resp.Header, nil
}

// MaxResponseBytes is the most of one ARM response body [Client.doOnce] will
// read into memory. The largest document pimctl asks for — a full page of
// schedule instances — is well under a megabyte, so 32 MiB is far above
// anything ARM sends and only exists so a misbehaving or misdirected response
// cannot grow the process without bound.
const MaxResponseBytes = 32 * mebibyte

// mebibyte is 2^20 bytes, the unit [MaxResponseBytes] is stated and reported in.
const mebibyte = 1 << 20

// redactURL keeps URLs safe to print. ARM PIM URLs carry no secrets, but the
// query string is dropped defensively so nothing token-shaped can ever leak.
func redactURL(u string) string {
	if before, _, ok := strings.Cut(u, "?"); ok {
		return before + "?…"
	}
	return u
}

// listPage is one page of any ARM listing: the rows, and the absolute URL of
// the page after this one. NextLink is empty on the last page, which is how
// [listAll] knows to stop.
type listPage[T any] struct {
	Value    []T    `json:"value"`    // this page's items.
	NextLink string `json:"nextLink"` // the next page's URL, empty on the last one.
}

// maxPages bounds nextLink following so a server-side paging loop cannot hang
// the CLI forever. Hitting it is an error, never a silently short list.
const maxPages = 100

// listAll issues a GET for firstURL and every nextLink after it, returning the
// concatenated rows in the order ARM sent them.
//
// It returns an error rather than a short list on any failure, including
// running past [maxPages]: a listing that stops early but reads as complete is
// exactly the failure the per-scope fan-out exists to avoid, and it would be no
// better arriving from here.
func listAll[T any](ctx context.Context, c *Client, firstURL string) ([]T, error) {
	var out []T
	next := firstURL
	pages := 0
	for ; next != "" && pages < maxPages; pages++ {
		raw, err := c.do(ctx, http.MethodGet, next, nil)
		if err != nil {
			return nil, err
		}
		var page listPage[T]
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, fmt.Errorf("could not parse the response from %s: %w", redactURL(next), err)
		}
		out = append(out, page.Value...)
		next = page.NextLink
	}
	if next != "" {
		return nil, fmt.Errorf(
			"%s returned more than %d pages; refusing to report a partial list",
			redactURL(firstURL),
			maxPages,
		)
	}
	return out, nil
}

// ListEligibilities returns every Azure resource role the signed-in user is
// eligible for, tenant-wide, following nextLink when ARM pages the result.
func (c *Client) ListEligibilities(ctx context.Context) ([]Eligibility, error) {
	u := fmt.Sprintf("%s/providers/Microsoft.Authorization/roleEligibilityScheduleInstances?api-version=%s&$filter=%s",
		c.Host, APIVersion, url.QueryEscape("asTarget()"))
	return listAll[Eligibility](ctx, c, u)
}

// ListAssignments returns every role assignment schedule instance for the user,
// both permanent ("Assigned") and activated ("Activated").
func (c *Client) ListAssignments(ctx context.Context) ([]Assignment, error) {
	u := fmt.Sprintf("%s/providers/Microsoft.Authorization/roleAssignmentScheduleInstances?api-version=%s&$filter=%s",
		c.Host, APIVersion, url.QueryEscape("asTarget()"))
	return listAll[Assignment](ctx, c, u)
}

// ListAssignmentsAtScope lists role assignment schedule instances for the
// signed-in user at one scope.
//
// The tenant-wide form of this call is both slow (11-21 s here) and lossy: three
// runs against an unchanged tenant returned 126, 131 and 132 rows with no
// nextLink. Asking scope by scope returns a complete answer and the slowest
// single call is a few seconds.
func (c *Client) ListAssignmentsAtScope(ctx context.Context, scope string) ([]Assignment, error) {
	u := fmt.Sprintf("%s%s/providers/Microsoft.Authorization/roleAssignmentScheduleInstances?api-version=%s&$filter=%s",
		c.Host, strings.TrimSuffix(scope, "/"), APIVersion, url.QueryEscape("asTarget()"))
	return listAll[Assignment](ctx, c, u)
}

// ListActivated returns only the assignments that are live PIM activations.
func (c *Client) ListActivated(ctx context.Context) ([]Assignment, error) {
	all, err := c.ListAssignments(ctx)
	if err != nil {
		return nil, err
	}
	var out []Assignment
	for _, a := range all {
		if a.IsActivated() {
			out = append(out, a)
		}
	}
	return out, nil
}

// RoleSettings is the activation-relevant subset of a PIM role management
// policy, resolved for one (scope, role) pair.
type RoleSettings struct {
	// PolicyID is the policy this was read from, kept so a cached entry can be
	// traced back to the ARM document it came from.
	PolicyID string
	// MaximumDuration is Expiration_EndUser_Assignment.maximumDuration.
	MaximumDuration time.Duration
	// MaximumDurationISO is the same value as ARM sent it, e.g. "PT8H", for
	// messages that quote the policy rather than a parsed duration.
	MaximumDurationISO string
	// EnabledRules is Enablement_EndUser_Assignment.enabledRules — some of
	// MultiFactorAuthentication, Justification, Ticketing.
	EnabledRules []string
	// ApprovalRequired is Approval_EndUser_Assignment.setting.isApprovalRequired.
	ApprovalRequired bool
	// AuthContextEnabled is AuthenticationContext_EndUser_Assignment.isEnabled.
	AuthContextEnabled bool
	// AuthContextClaimValue is the acrs claim the policy demands, which is what
	// a Conditional Access challenge asks the user to re-authenticate for.
	AuthContextClaimValue string
}

// RequiresJustification reports whether the policy demands a justification.
func (r *RoleSettings) RequiresJustification() bool { return r.hasRule("Justification") }

// RequiresTicket reports whether the policy demands ticket info.
func (r *RoleSettings) RequiresTicket() bool { return r.hasRule("Ticketing") }

// RequiresMFA reports whether the policy demands an MFA-backed token.
func (r *RoleSettings) RequiresMFA() bool { return r.hasRule("MultiFactorAuthentication") }

// hasRule reports whether name appears in [RoleSettings.EnabledRules]. The
// comparison ignores case so that a difference in how the policy document
// spells a rule cannot quietly turn a requirement the user must satisfy into an
// optional one.
func (r *RoleSettings) hasRule(name string) bool {
	for _, e := range r.EnabledRules {
		if strings.EqualFold(e, name) {
			return true
		}
	}
	return false
}

// policyAssignmentList is a roleManagementPolicyAssignments listing, narrowed
// to the policyId each entry points at. The query that produces it names one
// role definition at one scope, so [Client.GetRoleSettings] reads only the
// first entry.
type policyAssignmentList struct {
	// Value holds the assignments; the query narrows it to one role at one
	// scope, so only the first entry is read.
	Value []struct {
		Properties struct {
			PolicyID         string `json:"policyId"`         // the policy document, fetched only when EffectiveRules is empty.
			RoleDefinitionID string `json:"roleDefinitionId"` // the role the policy applies to.
			Scope            string `json:"scope"`            // the scope it applies at.
			// EffectiveRules is the rule set ARM computed for this assignment,
			// the same heterogeneous array the policy document carries under
			// properties.rules. When it is present the policy document adds
			// nothing: on the tenant this was measured against, every end-user
			// rule pimctl reads was byte-identical in both, so reading it here
			// saves the second GET of every policy lookup.
			EffectiveRules []json.RawMessage `json:"effectiveRules"`
		} `json:"properties"` // the only part of an assignment pimctl reads.
	} `json:"value"`
}

// policyDoc is a role management policy document. Its rules are a
// heterogeneous array — each rule has its own body — so they are held as raw
// JSON here and decoded a second time by [ParsePolicy], once [ruleHeader] has
// said which rule each one is.
type policyDoc struct {
	ID         string `json:"id"` // the policy's own ARM id, carried into [RoleSettings.PolicyID].
	Properties struct {
		Scope string `json:"scope"` // the scope the policy governs.
		// Rules are heterogeneous, so they stay raw until [ruleHeader] says
		// which shape each one is.
		Rules []json.RawMessage `json:"rules"`
	} `json:"properties"` // everything but the id.
}

// ruleHeader is the part every policy rule has in common, decoded first so the
// rest of the rule can be decoded into the shape its ID calls for. A rule whose
// ID is not one pimctl acts on is skipped without ever being read further.
//
// Only the id is kept. ARM also sends a ruleType, but it is the coarser of the
// two — one type covers both the end-user and the administrator form of a rule,
// and pimctl acts on the end-user form only — so switching on it would confuse
// the two. The unread field is dropped rather than left decoded, so nobody
// reaches for the wrong discriminator later.
type ruleHeader struct {
	ID string `json:"id"` // e.g. "Expiration_EndUser_Assignment"; the discriminator.
}

// GetRoleSettings resolves the PIM policy for one (scope, role) pair. Results
// are cached per (scope, role definition GUID) for the life of the client, so a
// batch of activations at the same scope costs one policy lookup.
func (c *Client) GetRoleSettings(ctx context.Context, scope, roleDefinitionID string) (*RoleSettings, error) {
	key := strings.ToLower(scope) + "|" + strings.ToLower(RoleDefinitionGUID(roleDefinitionID))
	c.policyMu.Lock()
	if s, ok := c.policyCache[key]; ok {
		c.policyMu.Unlock()
		return s, nil
	}
	c.policyMu.Unlock()

	filter := fmt.Sprintf("roleDefinitionId eq '%s'", roleDefinitionID)
	u := fmt.Sprintf("%s%s/providers/Microsoft.Authorization/roleManagementPolicyAssignments?api-version=%s&$filter=%s",
		c.Host, strings.TrimSuffix(scope, "/"), APIVersion, url.QueryEscape(filter))
	raw, err := c.do(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	var pal policyAssignmentList
	if err = json.Unmarshal(raw, &pal); err != nil {
		return nil, fmt.Errorf("could not parse the policy assignment list: %w", err)
	}
	if len(pal.Value) == 0 {
		return nil, fmt.Errorf(
			"no PIM policy is assigned to role %s at scope %s",
			RoleDefinitionGUID(roleDefinitionID),
			scope,
		)
	}
	policyID := pal.Value[0].Properties.PolicyID
	if policyID == "" {
		return nil, fmt.Errorf(
			"the policy assignment for role %s at scope %s has no policyId",
			RoleDefinitionGUID(roleDefinitionID),
			scope,
		)
	}

	// The listing's effectiveRules answer the question; the document is the
	// fallback for a listing that omits them or carries a set the parser
	// cannot use, where the document is authoritative and a second GET is
	// cheaper than a wrong answer.
	settings, err := parseRules(policyID, pal.Value[0].Properties.EffectiveRules)
	if err != nil {
		praw, getErr := c.do(ctx, http.MethodGet, fmt.Sprintf("%s%s?api-version=%s", c.Host, policyID, APIVersion), nil)
		if getErr != nil {
			return nil, getErr
		}
		if settings, err = ParsePolicy(praw); err != nil {
			return nil, err
		}
		settings.PolicyID = policyID
	}

	c.policyMu.Lock()
	c.policyCache[key] = settings
	c.policyMu.Unlock()
	return settings, nil
}

// ParsePolicy extracts the end-user activation rules from a role management
// policy document.
func ParsePolicy(raw []byte) (*RoleSettings, error) {
	var doc policyDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("could not parse the role management policy: %w", err)
	}
	return parseRules(doc.ID, doc.Properties.Rules)
}

// parseRules reads the four end-user rules pimctl acts on out of a rule array,
// whether it came from a policy document or from an assignment's
// effectiveRules; the array has the same shape in both. policyID is recorded
// on the result so a cached entry can be traced to its ARM document. It
// returns an error when the array has no Expiration_EndUser_Assignment rule
// with a maximumDuration, or when a rule it acts on does not decode, because a
// policy without a maximum is not one an activation can be planned against.
func parseRules(policyID string, rules []json.RawMessage) (*RoleSettings, error) {
	s := &RoleSettings{PolicyID: policyID}
	for _, rr := range rules {
		var h ruleHeader
		if err := json.Unmarshal(rr, &h); err != nil {
			continue
		}
		switch h.ID {
		case "Expiration_EndUser_Assignment":
			var r struct {
				MaximumDuration string `json:"maximumDuration"`
			}
			if err := json.Unmarshal(rr, &r); err != nil {
				return nil, fmt.Errorf("could not parse Expiration_EndUser_Assignment: %w", err)
			}
			s.MaximumDurationISO = r.MaximumDuration
			if r.MaximumDuration != "" {
				d, err := ParseISODuration(r.MaximumDuration)
				if err != nil {
					return nil, fmt.Errorf("Expiration_EndUser_Assignment.maximumDuration: %w", err)
				}
				s.MaximumDuration = d
			}
		case "Enablement_EndUser_Assignment":
			var r struct {
				EnabledRules []string `json:"enabledRules"`
			}
			if err := json.Unmarshal(rr, &r); err != nil {
				return nil, fmt.Errorf("could not parse Enablement_EndUser_Assignment: %w", err)
			}
			s.EnabledRules = r.EnabledRules
		case "Approval_EndUser_Assignment":
			var r struct {
				Setting struct {
					IsApprovalRequired bool `json:"isApprovalRequired"`
				} `json:"setting"`
			}
			if err := json.Unmarshal(rr, &r); err != nil {
				return nil, fmt.Errorf("could not parse Approval_EndUser_Assignment: %w", err)
			}
			s.ApprovalRequired = r.Setting.IsApprovalRequired
		case "AuthenticationContext_EndUser_Assignment":
			var r struct {
				IsEnabled  bool   `json:"isEnabled"`
				ClaimValue string `json:"claimValue"`
			}
			if err := json.Unmarshal(rr, &r); err != nil {
				return nil, fmt.Errorf("could not parse AuthenticationContext_EndUser_Assignment: %w", err)
			}
			s.AuthContextEnabled = r.IsEnabled
			s.AuthContextClaimValue = r.ClaimValue
		}
	}
	if s.MaximumDurationISO == "" {
		return nil, errors.New("the policy has no Expiration_EndUser_Assignment.maximumDuration rule")
	}
	return s, nil
}

// SubmitRequest PUTs a role assignment schedule request. requestName must be a
// client-generated GUID; reusing it on a retry updates the same request rather
// than creating a duplicate.
func (c *Client) SubmitRequest(
	ctx context.Context,
	scope, requestName string,
	body RequestBody,
) (*ScheduleRequest, error) {
	u := fmt.Sprintf("%s%s/providers/Microsoft.Authorization/roleAssignmentScheduleRequests/%s?api-version=%s",
		c.Host, strings.TrimSuffix(scope, "/"), requestName, APIVersion)
	raw, err := c.do(ctx, http.MethodPut, u, body)
	if err != nil {
		return nil, err
	}
	var sr ScheduleRequest
	if err := json.Unmarshal(raw, &sr); err != nil {
		return nil, fmt.Errorf("could not parse the schedule request response: %w", err)
	}
	return &sr, nil
}

// GetRequest reads back a schedule request by its full ARM id.
func (c *Client) GetRequest(ctx context.Context, id string) (*ScheduleRequest, error) {
	raw, err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s%s?api-version=%s", c.Host, id, APIVersion), nil)
	if err != nil {
		return nil, err
	}
	var sr ScheduleRequest
	if err := json.Unmarshal(raw, &sr); err != nil {
		return nil, fmt.Errorf("could not parse the schedule request response: %w", err)
	}
	return &sr, nil
}

// pollSchedule is the wait before each status re-read.
//
// ARM does not return a terminal status on the PUT itself — the request lands
// as Accepted or PendingProvisioning and settles a second or two later — so the
// first poll dominates the wall time of an activation. Starting at half a
// second catches that settle early; backing off to three seconds keeps a slow
// request from being polled harder the longer it takes.
var pollSchedule = []time.Duration{
	500 * time.Millisecond,
	time.Second,
	2 * time.Second,
	3 * time.Second,
}

// pollWait returns the delay before attempt n, holding at the last value.
func pollWait(attempt int) time.Duration {
	if attempt < len(pollSchedule) {
		return pollSchedule[attempt]
	}
	return pollSchedule[len(pollSchedule)-1]
}

// Poll re-reads a schedule request until it reaches a terminal status or the
// timeout expires, bounding poll sleeps, HTTP requests and retry delays together.
// The last observed request is always returned, even on timeout, so the caller
// can report "still <status>".
//
// A failed read inside the budget is not the end of the poll: the request was
// accepted by the PUT, and a read that fails — a connection reset, a 404 while
// the request is still propagating, a 5xx that outlived the retries — says
// nothing about the request's fate, so the next [pollWait] is spent and the
// read is tried again. The exception is ARM rejecting the credential, which
// [credentialRejected] identifies and which returns at once: every later read
// in the budget would be rejected the same way, and only the caller can mint a
// replacement. What comes back when the budget runs out is the last
// request seen together with the error from the last read that completed, nil
// if that read succeeded; a read the budget itself cut short is not an
// observation and reports nothing. Cancellation of ctx returns ctx.Err() at
// once, preserving the distinction from own-budget exhaustion so the caller
// can tell an interruption from a request that is merely slow.
func (c *Client) Poll(ctx context.Context, sr *ScheduleRequest, timeout time.Duration) (*ScheduleRequest, error) {
	if sr == nil {
		return nil, errors.New("nothing to poll")
	}
	last := sr
	if IsTerminalStatus(last.Properties.Status) {
		return last, nil
	}
	pollCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastErr error
	for attempt := 0; ; attempt++ {
		select {
		case <-pollCtx.Done():
			return last, pollOutcome(ctx, lastErr)
		case <-time.After(pollWait(attempt)):
		}
		next, err := c.GetRequest(pollCtx, last.ID)
		if pollCtx.Err() != nil {
			return last, pollOutcome(ctx, lastErr)
		}
		if err != nil {
			if credentialRejected(err) {
				return last, err
			}
			lastErr = err
			continue
		}
		lastErr = nil
		last = next
		if IsTerminalStatus(last.Properties.Status) {
			return last, nil
		}
	}
}

// credentialRejected reports whether err is ARM refusing the token rather than
// failing to answer about the request: a 401, which a cached token that expired
// mid-poll produces, or a 403. Retrying such a read cannot change the answer,
// so [Client.Poll] hands it straight back for the caller to re-mint against —
// spending the whole poll budget on reads ARM rejects identically would turn a
// one-second refresh into two full budgets of doomed requests per role.
func credentialRejected(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	return ae.StatusCode == http.StatusUnauthorized || ae.StatusCode == http.StatusForbidden
}

// pollOutcome is the error [Client.Poll] returns when its budget is gone:
// the caller's own cancellation when ctx is done, because an interruption must
// never read as a timeout, and otherwise lastErr, the error from the last
// completed read — nil when that read succeeded and the request is simply not
// terminal yet.
func pollOutcome(ctx context.Context, lastErr error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return lastErr
}
