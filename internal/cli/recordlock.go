// The advisory lock one pimctl process holds while it reads, merges and writes
// the activation record, so a second process cannot lose the first one's entry.
// It is flock(2) and nothing else: pimctl ships for darwin and linux only, both
// of which have it, so the file carries no build tag. What the lock protects is
// updateRecord in record.go; nothing here knows what a record contains.

package cli

import (
	"os"
	"syscall"

	"github.com/larsakerlund/pimctl/internal/store"
)

// recordLockSuffix names the sidecar the lock is taken on, next to the record
// it guards. The record itself is published by rename, which replaces the
// inode a lock would be held on; a sidecar that is never renamed keeps every
// process locking the same file.
const recordLockSuffix = ".lock"

// lockRecord takes an exclusive advisory lock on the sidecar next to the record
// at path, blocking until any other pimctl process holding it lets go, and
// returns the function that releases it. Both are best-effort: when the sidecar
// cannot be created or the filesystem does not honour flock(2), the returned
// function is a no-op and the caller proceeds unlocked, silently. The record is
// additive to ARM and reconciled against it on every command, so an entry lost
// to a race costs one fan-out and nothing else — whereas refusing to write, or
// printing a warning on every command from a network home directory, would
// cost more than the race does.
func lockRecord(path string) (unlock func()) {
	lockFile := path + recordLockSuffix
	flags := os.O_RDONLY | os.O_CREATE
	f, err := os.OpenFile(lockFile, flags, store.SecretFileMode) //nolint:gosec // pimctl's own path, sanitised
	if err != nil {
		return func() {}
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		closeQuietly(f)
		return func() {}
	}
	return func() {
		// Closing the descriptor releases the lock; an explicit unlock first
		// keeps the release independent of how the close goes.
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
			_ = err // The close below releases it regardless.
		}
		closeQuietly(f)
	}
}

// closeQuietly closes the lock sidecar. A failed close of a file nothing was
// written to changes nothing the caller can act on.
func closeQuietly(f *os.File) {
	if err := f.Close(); err != nil {
		_ = err // See the doc comment.
	}
}
