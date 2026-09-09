// Covers session.go: that one dead context does not abort the others, that no
// usable context is fatal, and that a 401 on a cached token is retried exactly
// once against a fresh one. It also exercises the context precedence from
// context.go end to end, through a fake ARM backend.

package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/larsakerlund/pimctl/internal/armclient"
	"github.com/larsakerlund/pimctl/internal/azauth"
)

// TestOneDeadContextDoesNotAbortTheOthers pins HIGH-3: with --all-contexts, a
// context whose login has expired must not stop the healthy ones from being
// listed. The run still exits 1 so a script cannot mistake a partial answer for
// a complete one.
func TestOneDeadContextDoesNotAbortTheOthers(t *testing.T) {
	dead := errors.New("cloudctx exec globex -- az account get-access-token failed: Please run 'az login'")
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()}
	f.installContexts([]string{"contoso"}, []error{dead})

	out, errOut, err := runCmd(t, "list", "--all-contexts")
	if !strings.Contains(out, "Cost Management Contributor") {
		t.Fatalf("the healthy context's rows were discarded:\n%s", out)
	}
	if !strings.Contains(errOut, "globex") || !strings.Contains(errOut, "az login") {
		t.Errorf("the failing context was not reported: %q", errOut)
	}
	if err == nil {
		t.Fatal("a partial run must exit non-zero")
	}
	if got := ExitCode(err); got != ExitFailed {
		t.Errorf("exit code = %d, want 1", got)
	}
	if !strings.Contains(err.Error(), "incomplete") {
		t.Errorf("the error should say the output is incomplete: %q", err.Error())
	}
}

// TestEveryContextDeadIsFatal: when nothing could be opened there is nothing to
// show, so the run fails outright rather than printing an empty table.
func TestEveryContextDeadIsFatal(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()}
	f.installContexts(nil, []error{errors.New("token failure")})
	if _, _, err := runCmd(t, "list", "--all-contexts"); err == nil {
		t.Fatal("expected a fatal error when no context could be opened")
	}
}

// TestOneContextListFailureKeepsTheOthersRows pins HIGH-4 at the gather layer:
// rows already produced must survive another context's error.
func TestOneContextListFailureKeepsTheOthersRows(t *testing.T) {
	good := &fakeARM{t: t, eligibilities: twoLowImpactRoles()}
	goodSrv := good.start()

	// A second server that always fails, standing in for a tenant returning 403.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":{"code":"AuthorizationFailed","message":"denied"}}`)
	}))
	t.Cleanup(bad.Close)

	mk := func(name, url string, c *http.Client) *session {
		tok := &azauth.Token{Context: name, AccessToken: "fake", PrincipalID: "oid", TenantID: "tid"}
		return &session{Context: name, Token: tok, Client: armclient.New(url, tok.AccessToken, c)}
	}
	sessions := []*session{
		mk("contoso", goodSrv.URL, goodSrv.Client()),
		mk("broken", bad.URL, bad.Client()),
	}

	// Through the production path: a helper that only tests call proves only
	// that the helper works.
	rc := &runContext{Sessions: sessions, Ctx: context.Background(), Timings: newTimings(false)}
	rows, errs, _ := readEligibilities(context.Background(), newRootCmdWithDiscard(t), rc)
	if len(rows) != 2 {
		t.Fatalf("got %d rows from the healthy context, want 2 — a failing context discarded them", len(rows))
	}
	if len(errs) != 1 {
		t.Fatalf("got %d errors, want 1", len(errs))
	}
	if !strings.Contains(errs[0].Error(), "broken") {
		t.Errorf("the error does not name the failing context: %v", errs[0])
	}

	active, errs := listTenantWide(context.Background(), sessions)
	if len(errs) != 1 {
		t.Fatalf("listTenantWide: got %d errors, want 1", len(errs))
	}
	_ = active
}

// TestPresetOpensItsOwnContexts is the fix for presets being unusable without
// -c: the contexts come from the preset itself.
func TestPresetOpensItsOwnContexts(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()}
	f.install()
	if _, _, err := runCmd(t, "up", "-c", "contoso", "--all", "-j", "x", "--save-preset", "daily", "-y"); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Now run with NO -c and no $CLOUDCTX_CONTEXT: the preset must supply it.
	t.Setenv(envContext, "")
	var opened resolution
	inner := testDeps().openSessions
	installSessionOpener(t, func(res resolution, tm *timings, refresh bool) ([]*session, []error, error) {
		opened = res
		return inner(res, tm, refresh)
	})

	f.resetPuts()
	if _, _, err := runCmd(t, "up", "daily", "-j", "x", "-y", "--refresh"); err != nil {
		t.Fatalf("up daily without -c: %v", err)
	}
	if opened.Source != SourcePreset {
		t.Errorf("contexts came from %s, want the preset", opened.Source)
	}
	if len(opened.Names) != 1 || opened.Names[0] != "contoso" {
		t.Errorf("opened contexts = %v, want [contoso]", opened.Names)
	}
	if len(f.putBodies()) != 2 {
		t.Errorf("preset run sent %d activations, want 2", len(f.putBodies()))
	}
}

// TestNoContextFallsBackToAzLoginAndSaysSo: with no context anywhere pimctl
// uses the shared az login, and announces the tenant and user it resolved to so
// acting on the wrong tenant cannot happen silently.
func TestNoContextFallsBackToAzLoginAndSaysSo(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()}
	f.installContexts([]string{""}, nil)
	t.Setenv(envContext, "")

	out, errOut, err := runCmd(t, "list")
	if err != nil {
		t.Fatalf("list with no context: %v", err)
	}
	if !strings.Contains(out, "2 eligible role(s).") {
		t.Errorf("list output:\n%s", out)
	}
	if !strings.Contains(errOut, "using az login: tenant") {
		t.Errorf("the shared login must be announced: %q", errOut)
	}
	if strings.Count(errOut, "using az login") != 1 {
		t.Errorf("exactly one announcement line expected, got: %q", errOut)
	}
}

func TestEnvContextIsUsedAndAnnounced(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()}
	f.install()
	t.Setenv(envContext, "contoso")
	out, errOut, err := runCmd(t, "list")
	if err != nil {
		t.Fatalf("list with $%s set: %v", envContext, err)
	}
	if !strings.Contains(out, "2 eligible role(s).") {
		t.Errorf("list output:\n%s", out)
	}
	if !strings.Contains(errOut, "using context contoso") || !strings.Contains(errOut, envContext) {
		t.Errorf("the resolved context should be announced on stderr: %q", errOut)
	}
}

func TestBareAzIsExplicitOnly(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()}
	f.installContexts([]string{""}, nil)
	t.Setenv(envContext, "")

	out, errOut, err := runCmd(t, "list", "--bare-az")
	if err != nil {
		t.Fatalf("--bare-az: %v", err)
	}
	if !strings.Contains(out, "eligible role(s).") {
		t.Errorf("list output:\n%s", out)
	}
	if !strings.Contains(errOut, "using az login: tenant") {
		t.Errorf("using the shared store must be stated plainly: %q", errOut)
	}
}

// TestBareAzInsideAContextWindowIsRefused: inside a `cloudctx use` window the
// flag and the shell contradict each other, and guessing wrong means acting on
// another tenant. It is also the case where --bare-az did not
// actually give you the shared login: az inherits AZURE_CONFIG_DIR from the
// window, so the "bare" token came from the context's own store and was cached
// under the shared-login slot for later unscoped runs to reuse.
func TestBareAzInsideAContextWindowIsRefused(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()}
	f.installContexts([]string{""}, nil)
	t.Setenv(envContext, "contoso")

	_, _, err := runCmd(t, "list", "--bare-az")
	if err == nil {
		t.Fatal("--bare-az inside a context window must be refused, not silently obeyed")
	}
	for _, want := range []string{"contoso", "--bare-az=force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should mention %q: %v", want, err)
		}
	}
}

// TestBareAzForceIgnoresTheEnvironmentContext: --bare-az=force is how you say
// it anyway, and it is the only spelling that ignores a scoped shell.
func TestBareAzForceIgnoresTheEnvironmentContext(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()}
	var opened resolution
	f.installContexts([]string{""}, nil)
	inner := testDeps().openSessions
	installSessionOpener(t, func(res resolution, tm *timings, refresh bool) ([]*session, []error, error) {
		opened = res
		return inner(res, tm, refresh)
	})
	t.Setenv(envContext, "contoso")

	if _, _, err := runCmd(t, "list", "--bare-az=force"); err != nil {
		t.Fatalf("--bare-az=force with $%s set: %v", envContext, err)
	}
	if !opened.Bare || len(opened.Names) != 0 {
		t.Errorf("--bare-az=force should ignore $%s, got %+v", envContext, opened)
	}

	// Outside a context window the plain flag is unambiguous and still works.
	t.Setenv(envContext, "")
	if _, _, err := runCmd(t, "list", "--bare-az"); err != nil {
		t.Fatalf("--bare-az outside a context window: %v", err)
	}
	if !opened.Bare {
		t.Errorf("--bare-az should still select the shared login, got %+v", opened)
	}

	// A value pimctl does not understand is refused rather than read as unset.
	if _, _, err := runCmd(t, "list", "--bare-az=yes"); err == nil {
		t.Error("--bare-az=yes should be rejected, not treated as unset")
	}
}

// TestUnauthorizedDropsTheCachedTokenAndRetriesOnce covers a cached token that
// ARM has stopped accepting — revoked, or invalidated by a policy change,
// before its stated expiry. The first 401 should be invisible and self-healing.
func TestUnauthorizedDropsTheCachedTokenAndRetriesOnce(t *testing.T) {
	cacheHome := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheHome)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(envContext, "")

	// A cached token that the server will reject.
	stale := &azauth.Token{
		Context:     "contoso",
		AccessToken: "stale-token",
		ExpiresOn:   time.Now().Add(time.Hour).Format("2006-01-02 15:04:05.000000"),
		Tenant:      "tid-1",
		PrincipalID: "oid-1",
		TenantID:    "tid-1",
	}
	azauth.WriteTokenCache(stale, azauth.DefaultRunner)

	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		seen = append(seen, auth)
		mu.Unlock()
		if auth == "stale-token" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":{"code":"ExpiredAuthenticationToken","message":"rejected"}}`)
			return
		}
		writeJSON(t, w, map[string]any{"value": []any{}})
	}))
	t.Cleanup(srv.Close)

	// The re-mint path goes through the real AcquireCached, so give it a runner.
	freshJWT := fakeTestJWT(`{"oid":"oid-1","tid":"tid-1"}`)
	prevRunner := azauth.DefaultRunner
	minted := 0
	azauth.DefaultRunner = func(name string, args ...string) ([]byte, []byte, error) {
		// The registry reads behind the tenant check and the store path are
		// not mints, and counting them as such would hide a second one.
		if name == "cloudctx" && len(args) > 1 && args[0] == "show" {
			return []byte("[" + args[1] + "]\nazure_tenant = tid-1\n\nstore:           " +
				fakeContextStore(args[1]) + "\n"), nil, nil
		}
		minted++
		expiry := time.Now().Add(time.Hour).Format("2006-01-02 15:04:05.000000")
		return []byte(`{"accessToken":"` + freshJWT + `","expiresOn":"` + expiry + `","tenant":"tid-1"}`), nil, nil
	}
	t.Cleanup(func() { azauth.DefaultRunner = prevRunner })

	installSessionOpener(t, func(resolution, *timings, bool) ([]*session, []error, error) {
		tok, _, err := azauth.ReadTokenCache("contoso", "", azauth.TokenCacheMargin, azauth.DefaultRunner)
		if err != nil || tok == nil {
			t.Fatalf("the stale token should have been readable: %v", err)
		}
		return []*session{{
			Context: "contoso",
			Token:   tok,
			Client:  armclient.New(srv.URL, tok.AccessToken, srv.Client()),
		}}, nil, nil
	})

	if _, _, err := runCmd(t, "list", "-c", "contoso"); err != nil {
		t.Fatalf("a 401 on a cached token should self-heal, got: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 {
		t.Fatalf("expected a retry after the 401, saw %d requests", len(seen))
	}
	if seen[0] != "stale-token" {
		t.Errorf("the first attempt should have used the cached token, got %q", seen[0])
	}
	if seen[1] == "stale-token" {
		t.Error("the retry reused the rejected token")
	}
	if minted != 1 {
		t.Errorf("expected exactly one re-mint, got %d", minted)
	}
	// The rejected entry must be gone or replaced, never left to fail again.
	if tok := readTokenCache(t, "contoso"); tok != nil && tok.AccessToken == "stale-token" {
		t.Error("the rejected token is still cached")
	}
}

// TestPersistentUnauthorizedIsReportedNotRetriedForever: a second 401 is a real
// authorization failure.
func TestPersistentUnauthorizedIsReported(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(envContext, "")

	azauth.WriteTokenCache(&azauth.Token{
		Context:     "contoso",
		AccessToken: "stale-token",
		ExpiresOn:   time.Now().Add(time.Hour).Format("2006-01-02 15:04:05.000000"),
		TenantID:    "tid-1",
		PrincipalID: "oid-1",
	}, azauth.DefaultRunner)

	var mu sync.Mutex
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"code":"AuthenticationFailed","message":"no"}}`)
	}))
	t.Cleanup(srv.Close)

	prevRunner := azauth.DefaultRunner
	jwt := fakeTestJWT(`{"oid":"oid-1","tid":"tid-1"}`)
	azauth.DefaultRunner = func(string, ...string) ([]byte, []byte, error) {
		expiry := time.Now().Add(time.Hour).Format("2006-01-02 15:04:05.000000")
		return []byte(`{"accessToken":"` + jwt + `","expiresOn":"` + expiry + `","tenant":"tid-1"}`), nil, nil
	}
	t.Cleanup(func() { azauth.DefaultRunner = prevRunner })

	installSessionOpener(t, func(resolution, *timings, bool) ([]*session, []error, error) {
		tok := readTokenCache(t, "contoso")
		return []*session{
			{Context: "contoso", Token: tok, Client: armclient.New(srv.URL, tok.AccessToken, srv.Client())},
		}, nil, nil
	})

	if _, _, err := runCmd(t, "list", "-c", "contoso"); err == nil {
		t.Fatal("a persistent 401 must be reported, not retried away")
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts > 4 {
		t.Errorf("expected at most one retry per call, saw %d attempts", attempts)
	}
}

// TestOpenSessionsWithKeepsTheContextsThatWork exercises the real function
// rather than the fake that usually replaces it. The promise it keeps is the
// one the README makes: one context whose login has expired is a warning, not
// the end of the run.
func TestOpenSessionsWithKeepsTheContextsThatWork(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	prev := azauth.DefaultRunner
	azauth.DefaultRunner = func(name string, args ...string) ([]byte, []byte, error) {
		joined := name + " " + strings.Join(args, " ")
		if strings.Contains(joined, "broken") {
			return nil, []byte("AADSTS700082: refresh token has expired"), errors.New("exit status 1")
		}
		expiry := time.Now().Add(time.Hour).Format("2006-01-02 15:04:05.000000")
		jwt := fakeTestJWT(`{"oid":"oid-1","tid":"tid-1","upn":"test@example.com"}`)
		return []byte(`{"accessToken":"` + jwt + `","expiresOn":"` + expiry + `","tenant":"tid-1"}`), nil, nil
	}
	t.Cleanup(func() { azauth.DefaultRunner = prev })

	res := resolution{Names: []string{"contoso", "broken", "globex"}, Source: SourceFlag}
	sessions, failures, err := openSessionsWith(res, newTimings(false), false)
	if err != nil {
		t.Fatalf("two good contexts must still open: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("opened %d sessions, want the two that work", len(sessions))
	}
	if len(failures) != 1 {
		t.Fatalf("got %d failures, want the one dead context", len(failures))
	}
	if !strings.Contains(failures[0].Error(), "broken") {
		t.Errorf("the failure does not name its context: %v", failures[0])
	}
	// az's own stderr is what tells the user to log in again, so it has to
	// survive into the message.
	if !strings.Contains(failures[0].Error(), "AADSTS700082") {
		t.Errorf("az's stderr was dropped: %v", failures[0])
	}
	for _, s := range sessions {
		if s.Token == nil || s.Client == nil || s.Token.PrincipalID != "oid-1" {
			t.Errorf("session %q is not usable: %+v", s.Context, s)
		}
	}

	// Every context dead is the one case that is fatal: there is nothing to
	// show, so printing an empty table would be a lie.
	res = resolution{Names: []string{"broken"}, Source: SourceFlag}
	if _, _, err := openSessionsWith(res, newTimings(false), false); err == nil {
		t.Error("no usable context must be an error, not an empty listing")
	}
}

// TestOpenSessionsWithRecordsCacheHitsWithoutTheToken: --debug has to say
// whether a token was minted or reused, and must never say more than that.
func TestOpenSessionsWithRecordsCacheHitsWithoutTheToken(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	installFakeRunner(t, []string{"contoso"})

	res := resolution{Names: []string{"contoso"}, Source: SourceFlag}
	timings := newTimings(true)
	sessions, _, err := openSessionsWith(res, timings, false)
	if err != nil {
		t.Fatal(err)
	}
	token := sessions[0].Token.AccessToken

	// The second open reads the cache the first one wrote.
	timings = newTimings(true)
	if _, _, err = openSessionsWith(res, timings, false); err != nil {
		t.Fatal(err)
	}
	var report strings.Builder
	timings.Report(&report, time.Second)
	if !strings.Contains(report.String(), "cache hit") {
		t.Errorf("a cached token should report as a hit:\n%s", report.String())
	}
	if strings.Contains(report.String(), token) {
		t.Fatal("the token leaked into the debug report")
	}

	// --refresh mints a new one and says so.
	timings = newTimings(true)
	if _, _, err = openSessionsWith(res, timings, true); err != nil {
		t.Fatal(err)
	}
	report.Reset()
	timings.Report(&report, time.Second)
	if !strings.Contains(report.String(), "miss") {
		t.Errorf("--refresh should report a miss:\n%s", report.String())
	}
}
