// The directory pimctl's files live in, the context-name-to-filename rule, and
// the atomic write and bulk remove every one of them uses. Nothing here knows
// what is being stored: the shapes, the TTLs and the meaning belong to the
// packages that call this one.

package store

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// DirMode is the permission for pimctl's own directories: nothing kept here is
// a secret except the token, and that file is 0600 regardless, but a private
// directory keeps role names and scopes off a shared machine.
const DirMode = 0o700

// Dir returns pimctl's cache directory, $XDG_CACHE_HOME/pimctl when that
// variable is set and ~/.cache/pimctl otherwise. It only computes the path; it
// does not create it. The error is returned when neither is available, which
// means the home directory could not be determined.
func Dir() (string, error) {
	if x := os.Getenv("XDG_CACHE_HOME"); x != "" {
		return filepath.Join(x, "pimctl"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("could not determine the home directory: %w", err)
	}
	return filepath.Join(home, ".cache", "pimctl"), nil
}

// unsafeInName is everything a context name may not contribute to a filename.
var unsafeInName = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

// SanitizeContext makes a context name usable as part of a filename. The
// ambient az login has no name, so it yields the empty string; callers wanting
// a filename should use FileName, which supplies the stand-in.
func SanitizeContext(context string) string {
	return unsafeInName.ReplaceAllString(context, "_")
}

// AzLoginName is the filename stand-in for the shared Azure CLI login.
const AzLoginName = "_az_login"

// FileName renders a context as the name component of a per-context file:
// sanitised, with AzLoginName standing in for the unnamed shared az login.
//
// The result is always a single safe path element — never empty, never "." or
// "..", never containing a separator — so a context name can never steer a
// write out of the directory it belongs in. Callers prefix and suffix it
// ("token-", ".json"), which would defuse a dotted name anyway; not relying on
// that is cheaper than checking each call site.
func FileName(context string) string {
	name := SanitizeContext(context)
	if name == "" || strings.Trim(name, ".") == "" {
		return AzLoginName
	}
	return name
}

// RemoveFiles deletes every file in the cache directory with the given prefix.
// A file that cannot be removed is reported by the next run as a stale cache,
// not as a failure of `pimctl cache clear`.
func RemoveFiles(prefix string) (int, error) {
	dir, err := Dir()
	if err != nil {
		return 0, err
	}
	return RemoveFilesIn(dir, prefix)
}

// RemoveFilesIn is [RemoveFiles] against a named directory, for the per-context
// state pimctl keeps inside a cloudctx context's store rather than in its own
// cache directory. A directory that does not exist has nothing to remove and is
// not an error.
func RemoveFilesIn(dir, prefix string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("could not read %s: %w", dir, err)
	}
	removed := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		if err = os.Remove(filepath.Join(dir, e.Name())); err == nil {
			removed++
		}
	}
	return removed, nil
}

// MoveIfAbsent moves a file from its old location to its new one, if and only
// if the old one exists and the new one does not. It is how a file that used to
// live under pimctl's own directory reaches the cloudctx context store it now
// belongs in, without a migration step the user has to run.
//
// A rename is tried first; when the two paths are on different filesystems the
// contents are copied through [WriteSecret] — 0600, fsynced, atomically
// published — and the original removed. Every failure is silent and leaves the
// old file in place: the caller writes to the new path either way, and both
// families that move here (a cached token and the activation record) are
// re-derivable from Azure.
func MoveIfAbsent(oldPath, newPath string) {
	if oldPath == "" || newPath == "" || oldPath == newPath {
		return
	}
	if _, err := os.Stat(newPath); err == nil {
		return // the new location wins; the old file is stale by definition.
	}
	if _, err := os.Stat(oldPath); err != nil {
		return // nothing to migrate, which is the ordinary case.
	}
	if err := os.MkdirAll(filepath.Dir(newPath), DirMode); err != nil {
		return
	}
	if err := os.Rename(oldPath, newPath); err == nil {
		return
	}
	blob, err := os.ReadFile(oldPath) //nolint:gosec // both paths are pimctl's own, built from a sanitised context name
	if err != nil {
		return
	}
	WriteSecret(newPath, blob)
	if _, err := os.Stat(newPath); err != nil {
		return // the copy did not land; keep the original where it is.
	}
	RemoveQuietly(oldPath)
}

// WriteAtomic publishes blob at path through a uniquely-named temporary file,
// so a concurrent run cannot see a half-written file. Failures are silent by
// design; every caller is writing something it can re-derive.
func WriteAtomic(path string, blob []byte) {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return
	}
	tmp := f.Name()
	if _, err = f.Write(blob); err != nil {
		closeAndRemove(f, tmp)
		return
	}
	if err = f.Close(); err != nil {
		RemoveQuietly(tmp)
		return
	}
	if err = os.Rename(tmp, path); err != nil {
		RemoveQuietly(tmp)
	}
}

// SecretFileMode is the mode for a file whose contents are a credential: the
// token cache, and nothing else so far. Everything else pimctl writes takes
// whatever os.CreateTemp gives it inside a [DirMode] directory.
const SecretFileMode fs.FileMode = 0o600

// WriteSecret publishes blob at path the way [WriteAtomic] does, with two
// additions a credential needs and a cache does not.
//
// The mode is set explicitly to [SecretFileMode] before the first byte is
// written, rather than relying on os.CreateTemp's documented 0600: the token
// must never exist on disk world-readable, not even for the instant between
// creation and a later chmod. And the contents are fsynced before the rename,
// so a crash cannot leave a file that exists but is empty — a token cache that
// reads back as garbage costs a re-mint, but one that reads back as a truncated
// token costs a confusing 401.
//
// Failures are silent, as in [WriteAtomic]: a token that cannot be cached is
// re-minted on the next command.
func WriteSecret(path string, blob []byte) {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return
	}
	tmp := f.Name()
	fail := func() { closeAndRemove(f, tmp) }
	if err = f.Chmod(SecretFileMode); err != nil {
		fail()
		return
	}
	if _, err = f.Write(blob); err != nil {
		fail()
		return
	}
	if err = f.Sync(); err != nil {
		fail()
		return
	}
	if err = f.Close(); err != nil {
		RemoveQuietly(tmp)
		return
	}
	if err = os.Rename(tmp, path); err != nil {
		RemoveQuietly(tmp)
	}
}

// closeAndRemove abandons a partially written temp file.
func closeAndRemove(f *os.File, path string) {
	if err := f.Close(); err != nil {
		_ = err // Nothing to do: the file is being abandoned either way.
	}
	RemoveQuietly(path)
}

// RemoveQuietly deletes a file whose absence is the desired end state. A
// failure is not worth reporting: a stray temp file is harmless because every
// write picks a fresh unique name, and a cache entry that cannot be deleted
// will be rejected on its next read anyway.
func RemoveQuietly(path string) {
	if err := os.Remove(path); err != nil {
		_ = err // See the doc comment.
	}
}
