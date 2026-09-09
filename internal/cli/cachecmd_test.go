// Tests for cachecmd.go, chiefly that `cache clear` leaves the activation
// record alone and says so, and that --all says what deleting it costs.

package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/larsakerlund/pimctl/internal/azauth"
	"github.com/larsakerlund/pimctl/internal/cache"
)

func TestCacheClearRemovesBothCachesAndSaysWhatItDidNotDo(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	azauth.WriteTokenCache(&azauth.Token{
		Context:     "contoso",
		AccessToken: "x",
		ExpiresOn:   time.Now().Add(time.Hour).Format("2006-01-02 15:04:05.000000"),
		TenantID:    "t",
	}, azauth.DefaultRunner)
	cache.Write("contoso", nil)

	out, _, err := runCmd(t, "cache", "clear")
	if err != nil {
		t.Fatalf("cache clear: %v", err)
	}
	if !strings.Contains(out, "Cleared 1 cached token(s)") {
		t.Errorf("output:\n%s", out)
	}
	if !strings.Contains(out, "az login is unchanged") {
		t.Errorf("cache clear must not imply it logged you out:\n%s", out)
	}
	if tok := readTokenCache(t, "contoso"); tok != nil {
		t.Error("the token cache survived")
	}
	if _, _, ok := cache.Read("contoso"); ok {
		t.Error("the listing cache survived")
	}
}

func TestLogoutIsAHiddenAliasThatExplainsItself(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	out, _, err := runCmd(t, "logout")
	if err != nil {
		t.Fatalf("logout: %v", err)
	}
	if !strings.Contains(out, "az login is unchanged") {
		t.Errorf("logout must say it did not end the az session:\n%s", out)
	}
	// Hidden: it must not appear in the command list.
	help, _, err := runCmd(t, "--help")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(help, "logout") {
		t.Errorf("logout should be hidden from the command list:\n%s", help)
	}
}

// TestCacheClearKeepsTheActivationRecord: the record is not a cache. Deleting
// it makes the next `status` under-report roles that are still held, because
// Azure's listing lags — so "just clear the cache" must not do it.
func TestCacheClearKeepsTheActivationRecord(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()}
	f.install()
	_, _, entry := lagFixtures(t)
	writeRecord("contoso", []recordEntry{entry})

	out, _, err := runCmd(t, "cache", "clear")
	if err != nil {
		t.Fatalf("cache clear: %v", err)
	}
	if got := heldEntries(); len(got) != 1 {
		t.Errorf("cache clear deleted the activation record: %+v", got)
	}
	if strings.Contains(out, "activation") {
		t.Errorf("cache clear must not claim to have touched the record:\n%s", out)
	}

	// --all is the way to ask for it, and it warns.
	out, errOut, err := runCmd(t, "cache", "clear", "--all")
	if err != nil {
		t.Fatalf("cache clear --all: %v", err)
	}
	if got := readRecord("contoso"); len(got) != 0 {
		t.Errorf("cache clear --all left %d record entries", len(got))
	}
	// Counted in activations and named by context: two files holding three
	// activations is not "2 activation records", and one of the contexts may be
	// one this command never mentioned.
	if !strings.Contains(out, "Forgot 1 activation(s) across contoso") {
		t.Errorf("cache clear --all must say what it forgot, and where:\n%s", out)
	}
	if !strings.Contains(errOut, "may not show roles you are still holding") {
		t.Errorf("cache clear --all must warn about the consequence: %q", errOut)
	}
}

// TestCacheClearAllNamesEveryContextItForgot: the count comes from the live
// entries, but the names must come from the files. A record whose activations
// have all expired still belongs to a context, and reporting only the contexts
// with live entries meant a command that deleted three files could name none of
// them — while still deleting the record for a context the command was never
// asked about.
func TestCacheClearAllNamesEveryContextItForgot(t *testing.T) {
	f := &fakeARM{t: t, eligibilities: twoLowImpactRoles()}
	// Both contexts are registered: each keeps its record inside its own
	// cloudctx store, so the sweep has to visit every context cloudctx knows
	// rather than one directory.
	f.installContexts([]string{"contoso", "globex"}, nil)

	// contoso holds one live activation; globex's has expired but its file is
	// still there.
	live := mkRecordEntry("Cost Management Contributor", "contoso-prod", time.Hour)
	writeRecord("contoso", []recordEntry{live})
	expired := mkRecordEntry("Owner", "contoso-test", time.Hour)
	expired.Context = "globex"
	expired.End = time.Now().Add(-time.Hour)
	expired.Key = recordKey(expired.Context, expired.Scope, expired.RoleDefinitionID)
	writeRecordFileForTest(t, "globex", []recordEntry{expired})

	out, errOut, err := runCmd(t, "cache", "clear", "--all")
	if err != nil {
		t.Fatalf("cache clear --all: %v", err)
	}
	for _, want := range []string{"contoso", "globex"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not name %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "Forgot 1 activation(s)") {
		t.Errorf("the count should be of live activations:\n%s", out)
	}
	if !strings.Contains(errOut, "may not show roles you are still holding") {
		t.Errorf("the warning is missing: %q", errOut)
	}
	if got := readRecord("globex"); len(got) != 0 {
		t.Errorf("globex's record survived: %+v", got)
	}
}
