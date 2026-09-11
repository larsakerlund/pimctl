// Covers eligibility and local-record isolation when the selected account
// changes. The backend is a local fake and all persisted data is temporary.

package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/larsakerlund/pimctl/internal/armclient"
	"github.com/larsakerlund/pimctl/internal/azauth"
	"github.com/larsakerlund/pimctl/internal/cache"
	"github.com/larsakerlund/pimctl/internal/store"
)

func TestAccountSwitchDoesNotReuseRolesOrActivationRecords(t *testing.T) {
	for _, owner := range []store.Owner{
		{TenantID: "new-tenant", PrincipalID: "oid-1"},
		{TenantID: "tid-1", PrincipalID: "new-user"},
	} {
		t.Run(owner.TenantID+"/"+owner.PrincipalID, func(t *testing.T) {
			t.Setenv("XDG_CACHE_HOME", t.TempDir())
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			old := testOwner("")
			cache.Write(old, twoLowImpactRoles())
			entry := mkRecordEntry("Owner", "old-scope", time.Hour)
			entry.Context = "(default)"
			writeRecord(old, []recordEntry{entry})
			var reads atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/providers/Microsoft.Authorization/roleEligibilityScheduleInstances" {
					reads.Add(1)
				}
				fmt.Fprint(w, `{"value":[]}`)
			}))
			t.Cleanup(srv.Close)
			tok := &azauth.Token{
				TenantID:    owner.TenantID,
				PrincipalID: owner.PrincipalID,
				AccessToken: "fake-new-account",
			}
			sess := &session{Token: tok, Client: armclient.New(srv.URL, tok.AccessToken, srv.Client())}
			rc := &runContext{
				Ctx:      context.Background(),
				Sessions: []*session{sess},
				Timeouts: defaultTimeouts(),
				Timings:  newTimings(false),
			}
			rows, errs, future := readEligibilities(rc.Ctx, newRootCmdWithDiscard(t), rc)
			future.Wait(-1)
			if len(errs) != 0 || len(rows) != 0 || reads.Load() != 1 {
				t.Fatalf("new identity reused old roles: rows=%d reads=%d errs=%v", len(rows), reads.Load(), errs)
			}
			if got := readLocalRecord(rc); len(got.rows) != 0 || len(got.revoked) != 0 {
				t.Fatal("new identity reused old activation state")
			}
			if len(readRecord(old)) != 1 {
				t.Fatal("switching accounts destroyed the original account's record")
			}
		})
	}
}
