// Test the authenticated HTTP origin boundary with synthetic credentials.
// The transport probes do not contact Azure or any external host.

package armclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type originTransport func(*http.Request) (*http.Response, error)

func (f originTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAuthenticatedDestinationBoundary(t *testing.T) {
	c := New("https://management.azure.com", "synthetic", nil)
	for _, tc := range []struct {
		name    string
		url     string
		allowed bool
	}{
		{"same origin", "https://management.azure.com/page2?opaque=1", true},
		{"host case", "https://MANAGEMENT.azure.com/page2", true},
		{"default port", "https://management.azure.com:443/page2", true},
		{"other host", "https://untrusted.invalid/page2", false},
		{"subdomain", "https://untrusted.management.azure.com/page2", false},
		{"lookalike", "https://management.azure.com.untrusted.invalid/page2", false},
		{"downgrade", "http://management.azure.com/page2", false},
		{"other port", "https://management.azure.com:8443/page2", false},
		{"userinfo", "https://user:password@management.azure.com/page2", false},
		{"fragment", "https://management.azure.com/page2#secret", false},
		{"relative", "/page2", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.url)
			if err != nil {
				t.Fatal(err)
			}
			if err = c.validateDestination(u); (err == nil) != tc.allowed {
				t.Fatalf("destination allowed=%v, want %v: %v", err == nil, tc.allowed, err)
			}
		})
	}
}

func TestPaginationAndRedirectsKeepCredentialsAtOrigin(t *testing.T) {
	for _, redirect := range []bool{false, true} {
		for _, destination := range []string{
			"https://untrusted.invalid/next", "http://management.azure.com/next",
			"https://management.azure.com:8443/next", "https://management.azure.com/next",
		} {
			t.Run(destination+map[bool]string{true: "/redirect", false: "/pagination"}[redirect], func(t *testing.T) {
				allowed := destination == "https://management.azure.com/next"
				calls := 0
				hc := &http.Client{Transport: originTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					if calls > 1 && !allowed {
						t.Error("untrusted destination reached transport")
					}
					if r.Header.Get("Authorization") != "Bearer synthetic" {
						t.Error("trusted request lost authentication")
					}
					body := `{"value":[]}`
					status := http.StatusOK
					header := make(http.Header)
					if calls == 1 {
						if redirect {
							status = http.StatusFound
							header.Set("Location", destination)
						} else {
							body = `{"value":[],"nextLink":"` + destination + `"}`
						}
					}
					return &http.Response{
						StatusCode: status,
						Header:     header,
						Body:       io.NopCloser(strings.NewReader(body)),
						Request:    r,
					}, nil
				})}
				c := New("https://management.azure.com", "synthetic", hc)
				_, err := c.ListEligibilities(context.Background())
				if (err == nil) != allowed {
					t.Fatalf("allowed=%v, error=%v", allowed, err)
				}
				wantCalls := 1
				if allowed {
					wantCalls = 2
				}
				if calls != wantCalls {
					t.Fatalf("transport called %d times, want %d", calls, wantCalls)
				}
				if hc.CheckRedirect != nil {
					t.Error("New mutated the caller's HTTP client")
				}
			})
		}
	}
}

func TestTrustedRedirectRetainsCallerPolicy(t *testing.T) {
	want := errors.New("caller refused redirect")
	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return want }}
	c := New("https://management.azure.com", "synthetic", hc)
	req, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		"https://management.azure.com/next",
		http.NoBody,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.HTTP.CheckRedirect(req, nil); !errors.Is(err, want) {
		t.Fatalf("caller policy ignored: %v", err)
	}
}
