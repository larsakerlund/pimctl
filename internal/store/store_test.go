// Tests for the file primitives every on-disk thing pimctl writes shares: the
// context-name rule, the atomic publish and the bulk remove. What is stored in
// those files, and for how long, is tested in the packages that own it.

package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFileNameIsOneStandInForTheNamelessContext: four files are named after a
// context — the token, the listing, the policies and the record. The listing
// used `_bare_az` where the other three used `_az_login`; nothing broke because
// the prefixes differ, but the fifth file would have been a coin toss.
func TestFileNameIsOneStandInForTheNamelessContext(t *testing.T) {
	if got := FileName(""); got != AzLoginName {
		t.Errorf("FileName(\"\") = %q, want %q", got, AzLoginName)
	}
	if got := FileName("contoso"); got != "contoso" {
		t.Errorf("a named context should be itself, got %q", got)
	}
	// Whatever the name, the result is one safe path element: a context can
	// never steer a write out of the cache directory.
	for _, name := range []string{"", "///", "a/b:c", "..", "../../etc/passwd"} {
		got := FileName(name)
		if got == "" || strings.ContainsAny(got, `/\:`) || got == ".." {
			t.Errorf("FileName(%q) = %q, which is not a safe single path element", name, got)
		}
	}
}

// TestDirFollowsXDG: the directory is computed, never created, and honours
// XDG_CACHE_HOME so a test — or a user with an unusual home — can redirect
// everything pimctl writes with one variable.
func TestDirFollowsXDG(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", tmp)
	got, err := Dir()
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	if want := filepath.Join(tmp, "pimctl"); got != want {
		t.Errorf("Dir = %q, want %q", got, want)
	}
	if _, err := os.Stat(got); !os.IsNotExist(err) {
		t.Errorf("Dir must not create the directory (stat err %v)", err)
	}
}

// TestWriteAtomicPublishesOrLeavesTheOldFile: a reader must never see half a
// file, and a failed write must leave the previous contents in place.
func TestWriteAtomicPublishesOrLeavesTheOldFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "thing.json")

	WriteAtomic(path, []byte("first"))
	got, err := os.ReadFile(path) //nolint:gosec // a path this test just built
	if err != nil || string(got) != "first" {
		t.Fatalf("read back %q (%v), want \"first\"", got, err)
	}

	WriteAtomic(path, []byte("second"))
	got, err = os.ReadFile(path) //nolint:gosec // as above
	if err != nil || string(got) != "second" {
		t.Errorf("second write left %q (%v)", got, err)
	}

	// No temporary files survive a successful publish.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("a temporary file was left behind: %s", e.Name())
		}
	}

	// A path in a directory that does not exist is a silent no-op, not a panic
	// or a partial file: every caller is writing something re-derivable.
	WriteAtomic(filepath.Join(dir, "missing", "x.json"), []byte("x"))
}

// TestRemoveFilesTakesOnlyItsOwnPrefix: `cache clear` removes families of files
// by prefix, and taking one file too many would delete another family's state.
func TestRemoveFilesTakesOnlyItsOwnPrefix(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", tmp)
	dir := filepath.Join(tmp, "pimctl")
	if err := os.MkdirAll(dir, DirMode); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"token-a.json", "token-b.json", "eligibilities-a.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	n, err := RemoveFiles("token-")
	if err != nil {
		t.Fatalf("RemoveFiles: %v", err)
	}
	if n != 2 {
		t.Errorf("removed %d files, want 2", n)
	}
	if _, err := os.Stat(filepath.Join(dir, "eligibilities-a.json")); err != nil {
		t.Errorf("another family's file was removed: %v", err)
	}

	// A missing directory is nothing to do, not an error: there is no cache to
	// clear before the first run.
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmp, "nowhere"))
	if n, err := RemoveFiles("token-"); err != nil || n != 0 {
		t.Errorf("RemoveFiles on a missing directory = %d, %v; want 0, nil", n, err)
	}
}

// TestWriteSecretIsNeverWorldReadable: the token cache is the one file here
// whose contents are a credential, and the mode is set before the first byte
// rather than after the write, so it never exists readable by anyone else even
// for an instant.
func TestWriteSecretIsNeverWorldReadable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token-contoso.json")

	WriteSecret(path, []byte(`{"accessToken":"secret"}`))
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("WriteSecret wrote nothing: %v", err)
	}
	if perm := info.Mode().Perm(); perm != SecretFileMode {
		t.Errorf("mode = %#o, want %#o", perm, SecretFileMode)
	}
	got, err := os.ReadFile(path) //nolint:gosec // a path this test just built
	if err != nil || string(got) != `{"accessToken":"secret"}` {
		t.Fatalf("read back %q (%v)", got, err)
	}

	// Replacing keeps the mode: a second write must not inherit a umask.
	WriteSecret(path, []byte(`{"accessToken":"second"}`))
	if info, err = os.Stat(path); err != nil || info.Mode().Perm() != SecretFileMode {
		t.Errorf("mode after rewrite = %v (%v)", info.Mode().Perm(), err)
	}

	// No temporary file survives, and a missing directory is a silent no-op
	// rather than a panic: a token that cannot be cached is re-minted.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("directory holds %d files, want just the token", len(entries))
	}
	WriteSecret(filepath.Join(dir, "missing", "token.json"), []byte("x"))
}

// TestRemoveQuietlyIgnoresWhatIsAlreadyGone: every caller wants the file gone,
// and a file that was never there satisfies that.
func TestRemoveQuietlyIgnoresWhatIsAlreadyGone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gone.json")
	if err := os.WriteFile(path, []byte("x"), SecretFileMode); err != nil {
		t.Fatal(err)
	}
	RemoveQuietly(path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the file survived: %v", err)
	}
	RemoveQuietly(path)                             // already gone.
	RemoveQuietly(filepath.Join(dir, "never.json")) // never there.
}

// TestDirFallsBackToTheHomeDirectory: XDG_CACHE_HOME is the override, not the
// requirement. Without it the path is the conventional ~/.cache/pimctl.
func TestDirFallsBackToTheHomeDirectory(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "")
	got, err := Dir()
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory to fall back to")
	}
	if want := filepath.Join(home, ".cache", "pimctl"); got != want {
		t.Errorf("Dir = %q, want %q", got, want)
	}
}
