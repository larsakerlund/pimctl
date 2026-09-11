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
// tenant takes about 20 seconds; 60 leaves room without hanging a shell.
const DefaultHTTPTimeout = 60 * time.Second

// New builds a client for one ARM token.
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

// retryDelay is how long to wait before the next attempt: what ARM asked for
// when it said, and exponential backoff from one second when it did not.
//
// It reads the wait off the parsed error rather than the raw header, so the
// RetryAfter field a caller can inspect is the same value the retry loop
// actually slept for. They were computed separately before, and the field was
// never assigned at all.
func retryDelay(err *APIError, attempt int) time.Duration {
	if err != nil && err.HasRetryAfter {
		return err.RetryAfter
	}
	return time.Duration(1<<attempt) * time.Second
}

// SetToken replaces the bearer token, used after a 401 forces a re-mint.
func (c *Client) SetToken(token string) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	c.token = token
}

// bearer returns the current token for the Authorization header.
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
		// attempt is only in scope here, so the backoff is computed here —
		// otherwise every retry without a Retry-After header waits a flat 1s.
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
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.Header, fmt.Errorf("%s %s: could not read the response: %w", method, redactURL(rawURL), err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, resp.Header, ParseAPIError(method, redactURL(rawURL), resp.StatusCode, raw, resp.Header)
	}
	return raw, resp.Header, nil
}

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
// The tenant-wide form of this call is both slow (11-21s here) and lossy: three
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
			PolicyID         string `json:"policyId"`         // the policy document to fetch next.
			RoleDefinitionID string `json:"roleDefinitionId"` // the role the policy applies to.
			Scope            string `json:"scope"`            // the scope it applies at.
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

	praw, err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s%s?api-version=%s", c.Host, policyID, APIVersion), nil)
	if err != nil {
		return nil, err
	}
	settings, err := ParsePolicy(praw)
	if err != nil {
		return nil, err
	}
	settings.PolicyID = policyID

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
	s := &RoleSettings{PolicyID: doc.ID}
	for _, rr := range doc.Properties.Rules {
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
// first poll dominates the wall time of an activation. A flat three seconds
// spent that on every role; starting at half a second and backing off finds the
// same answer sooner without polling harder for long.
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
// can report "still <status>". Own-budget exhaustion returns no error; cancellation
// of ctx returns its error, preserving the distinction from user interruption.
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
	for attempt := 0; ; attempt++ {
		select {
		case <-pollCtx.Done():
			return last, ctx.Err()
		case <-time.After(pollWait(attempt)):
		}
		next, err := c.GetRequest(pollCtx, last.ID)
		if pollCtx.Err() != nil {
			// Exhausting our own budget is an unfinished request, not an
			// interruption. Only the caller's cancellation returns an error.
			return last, ctx.Err()
		}
		if err != nil {
			return last, err
		}
		last = next
		if IsTerminalStatus(last.Properties.Status) {
			return last, nil
		}
	}
}
