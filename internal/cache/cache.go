// The eligible-role listing cache, plus the pieces every on-disk file pimctl
// writes shares: the cache directory, the context-name sanitiser, the shared
// directory mode and the atomic write. Policies are cached in policy.go; the
// token file is internal/azauth's, and the activation record internal/cli's.

package cache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/larsakerlund/pimctl/internal/armclient"
	"github.com/larsakerlund/pimctl/internal/store"
)

// TTL is how long a cached eligibility listing stays usable. Eligibility
// changes on the timescale of an access review, not a working session, so ten
// minutes of staleness costs nothing.
const TTL = 10 * time.Minute

// TTLHuman describes TTL in help text.
const TTLHuman = "10 minutes"

// version is the on-disk format of [envelope]. Bumping it is how a change to
// the cached shape is rolled out: [Read] treats a file written by any other
// version as absent, so the old one is simply re-fetched rather than
// mis-parsed.
const version = 2

// envelope is the on-disk shape of one context's listing: the roles, plus
// enough about the write for [Read] to decide whether to trust them.
type envelope struct {
	Version int `json:"version"` // the on-disk format; a mismatch means "absent".
	// Context is stored as well as being in the filename, so a file moved or
	// renamed by hand is rejected rather than served under the wrong context.
	Context string `json:"context"`
	// FetchedAt is when ARM answered, and is what TTL is measured from.
	FetchedAt time.Time               `json:"fetchedAt"`
	Roles     []armclient.Eligibility `json:"roles"` // the listing itself, exactly as ARM returned it.
}

// eligibilityPath is where one context's listing lives. The path is derived
// through FileName, never taken from the user.
func eligibilityPath(context string) (string, error) {
	dir, err := store.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "eligibilities-"+store.FileName(context)+".json"), nil
}

// Read returns a cached listing and its age. ok is false when there is no
// usable entry — a missing, unreadable, stale or wrong-version file all just
// mean "fetch it again", never an error the user has to deal with.
func Read(context string) (roles []armclient.Eligibility, age time.Duration, ok bool) {
	p, err := eligibilityPath(context)
	if err != nil {
		return nil, 0, false
	}
	// eligibilityPath derives p from the cache directory and the context name; the user
	// never supplies it.
	b, err := os.ReadFile(p) //nolint:gosec // see above
	if err != nil {
		return nil, 0, false
	}
	var env envelope
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, 0, false
	}
	if env.Version != version || env.Context != context {
		return nil, 0, false
	}
	age = time.Since(env.FetchedAt)
	if age < 0 || age > TTL {
		return nil, 0, false
	}
	return env.Roles, age, true
}

// Write stores a listing. Failures are silent by design: a cache that cannot be
// written must not break the command that produced the data.
func Write(context string, roles []armclient.Eligibility) {
	p, err := eligibilityPath(context)
	if err != nil {
		return
	}
	blob, err := json.Marshal(envelope{
		Version:   version,
		Context:   context,
		FetchedAt: time.Now(),
		Roles:     roles,
	})
	if err != nil {
		return
	}
	if err = os.MkdirAll(filepath.Dir(p), store.DirMode); err != nil {
		return
	}
	store.WriteAtomic(p, blob)
}

// Clear removes every cached eligibility listing, for every context, and
// reports how many files it deleted. It leaves policies, tokens and the
// activation record alone; `pimctl cache clear` calls each of those in turn.
func Clear() (int, error) {
	return store.RemoveFiles("eligibilities-")
}

// FormatAge renders a cache age for the "(cached Nm ago)" notice, in whole
// seconds below a minute and whole minutes above it. Both are truncated, not
// rounded: the age only has to tell a reader roughly how old the answer is, and
// [TTL] caps it at ten minutes anyway.
func FormatAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	default:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
}
