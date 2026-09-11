// Constrain authenticated requests and redirects to the configured ARM origin.
// This file does not acquire tokens or interpret ARM responses.

package armclient

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// maxRedirects preserves net/http's default bound when no caller supplies a
// redirect policy. Origin checks apply before any caller policy runs.
const maxRedirects = 10

// validateDestination rejects URLs outside the configured HTTP origin, including
// user information, fragments and opaque URLs. Production uses an HTTPS ARM
// host; an explicitly configured HTTP host allows local test servers. No
// rejected URL is included in the error because its contents are untrusted.
func (c *Client) validateDestination(destination *url.URL) error {
	origin, err := url.Parse(c.Host)
	if err != nil || destination == nil || origin.Hostname() == "" {
		return errors.New("invalid ARM endpoint")
	}
	if (origin.Scheme != "https" && origin.Scheme != "http") || origin.User != nil ||
		destination.User != nil || destination.Fragment != "" || destination.Opaque != "" ||
		destination.Scheme != origin.Scheme || !strings.EqualFold(destination.Hostname(), origin.Hostname()) ||
		effectivePort(destination) != effectivePort(origin) {
		return errors.New("refusing to send an ARM credential outside the configured origin")
	}
	return nil
}

// effectivePort returns a URL's explicit port or its HTTP scheme's default, so
// HTTPS with no port and HTTPS on port 443 have the same origin.
func effectivePort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}

// guardedHTTPClient copies the caller's client without changing its transport,
// timeout or cookie jar, and checks every redirect before it can transmit a
// credential. The caller's redirect policy remains in force for trusted URLs.
func (c *Client) guardedHTTPClient(source *http.Client) *http.Client {
	guarded := *source
	guarded.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if err := c.validateDestination(req.URL); err != nil {
			return err
		}
		if source.CheckRedirect != nil {
			return source.CheckRedirect(req, via)
		}
		if len(via) >= maxRedirects {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return &guarded
}
