// The whole of the config package: the two on-disk shapes, where they live,
// and the read and atomic write they share.

package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// PresetEntry identifies one eligible role well enough to re-select it on a
// later run. The role definition GUID plus the scope is the stable identity;
// RoleName is kept only so a stale entry can be reported by name.
type PresetEntry struct {
	Context          string `json:"context"`             // cloudctx context; empty means the shared az login.
	Scope            string `json:"scope"`               // Full ARM scope id the role was eligible at.
	RoleDefinitionID string `json:"roleDefinitionId"`    // Full ARM id; only its GUID has to match.
	RoleName         string `json:"roleName"`            // for naming an entry that is no longer eligible.
	ScopeName        string `json:"scopeName,omitempty"` // Display name as it read when saved, for reporting only.
}

// Presets is the on-disk presets file: preset name to the roles it selects. The
// zero value has a nil map, which reads fine and cannot be written to; use
// [LoadPresets], or [Presets.Set], which allocates.
type Presets struct {
	Presets map[string][]PresetEntry `json:"presets"` // preset name to its entries; nil until something is saved.
}

// State is the on-disk remembered state. Its zero value is what a first run
// sees, and every field must stay meaningful when empty.
type State struct {
	// LastJustification is the text sent with the previous activation, offered
	// as the prompt's default and used silently when no prompt is shown.
	LastJustification string `json:"lastJustification"`
}

// Dir returns pimctl's config directory, honouring $XDG_CONFIG_HOME.
func Dir() (string, error) {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "pimctl"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("could not determine the home directory: %w", err)
	}
	return filepath.Join(home, ".config", "pimctl"), nil
}

// path joins one config file name onto [Dir]. It fails only when the config
// directory cannot be determined at all.
func path(name string) (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, name), nil
}

// PresetsPath is where the named selections live. It is also printed when
// there are none, so the user can see which file `preset list` looked in.
func PresetsPath() (string, error) { return path("presets.json") }

// StatePath is the state file location.
func StatePath() (string, error) { return path("state.json") }

// readJSON decodes the file at p into v, leaving v untouched when the file is
// absent or empty — a first run has no config, which is not a failure. It
// returns an error naming p when the file exists but cannot be read or parsed,
// because reporting a corrupt presets.json as "no presets" would look like the
// presets had been lost.
func readJSON(p string, v any) error {
	// p always comes from PresetsPath/StatePath, under pimctl's own config
	// directory; it is never taken from the command line.
	b, err := os.ReadFile(p) //nolint:gosec // see above
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("could not read %s: %w", p, err)
	}
	if len(b) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("could not parse %s: %w", p, err)
	}
	return nil
}

// pimctl's config holds role names and scope ids for every tenant this account
// can reach, so both the directory and the files it contains are owner-only.
const (
	configDirMode  = 0o700 // owner-only directory.
	configFileMode = 0o600 // owner-only files.
)

// writeJSON replaces a config file atomically, creating the config directory if
// it is missing and returning an error that names the path for whichever step
// failed.
//
// The temp file gets a unique name so two pimctl runs finishing at once cannot
// clobber each other's half-written file, and it is fsynced before the rename
// so a crash in between cannot leave a zero-length file that readJSON would
// then treat as "no presets" rather than as corruption.
func writeJSON(p string, v any) error {
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, configDirMode); err != nil {
		return fmt.Errorf("could not create %s: %w", dir, err)
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')

	f, err := os.CreateTemp(dir, filepath.Base(p)+".tmp-*")
	if err != nil {
		return fmt.Errorf("could not create a temporary file in %s: %w", dir, err)
	}
	tmp := f.Name()
	// cleanup runs only on a path that already has a real error to return, so a
	// failure to tidy the temporary file must not mask it.
	cleanup := func() {
		f.Close()      //nolint:errcheck // best-effort cleanup, see above
		os.Remove(tmp) //nolint:errcheck // best-effort cleanup, see above
	}
	if err := f.Chmod(configFileMode); err != nil {
		cleanup()
		return fmt.Errorf("could not set permissions on %s: %w", tmp, err)
	}
	if _, err := f.Write(b); err != nil {
		cleanup()
		return fmt.Errorf("could not write %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("could not flush %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp) //nolint:errcheck // best-effort cleanup, see above
		return fmt.Errorf("could not close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp) //nolint:errcheck // best-effort cleanup, see above
		return fmt.Errorf("could not replace %s: %w", p, err)
	}
	return nil
}

// LoadPresets reads the presets file; a missing file is an empty set.
func LoadPresets() (*Presets, error) {
	p, err := PresetsPath()
	if err != nil {
		return nil, err
	}
	ps := &Presets{Presets: map[string][]PresetEntry{}}
	if err := readJSON(p, ps); err != nil {
		return nil, err
	}
	if ps.Presets == nil {
		ps.Presets = map[string][]PresetEntry{}
	}
	return ps, nil
}

// SavePresets writes the presets file atomically, creating the config
// directory if needed. It returns an error naming the file on any failure; the
// previous contents survive a failed write.
func SavePresets(ps *Presets) error {
	p, err := PresetsPath()
	if err != nil {
		return err
	}
	return writeJSON(p, ps)
}

// Names returns the preset names in sorted order.
func (p *Presets) Names() []string {
	names := make([]string, 0, len(p.Presets))
	for n := range p.Presets {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Get returns one preset's entries and whether that name is saved at all. A
// saved-but-empty preset and an unknown one are different answers.
func (p *Presets) Get(name string) ([]PresetEntry, bool) {
	e, ok := p.Presets[name]
	return e, ok
}

// Set replaces one preset in memory, allocating the map if the receiver came
// from a zero [Presets]. Nothing reaches disk until [SavePresets].
func (p *Presets) Set(name string, entries []PresetEntry) {
	if p.Presets == nil {
		p.Presets = map[string][]PresetEntry{}
	}
	p.Presets[name] = entries
}

// Delete removes a preset, reporting whether it existed.
func (p *Presets) Delete(name string) bool {
	if _, ok := p.Presets[name]; !ok {
		return false
	}
	delete(p.Presets, name)
	return true
}

// LoadState reads the state file; a missing file is zero state.
func LoadState() (*State, error) {
	p, err := StatePath()
	if err != nil {
		return nil, err
	}
	s := &State{}
	if err := readJSON(p, s); err != nil {
		return nil, err
	}
	return s, nil
}

// SaveState writes the state file atomically, with the same guarantees as
// [SavePresets].
func SaveState(s *State) error {
	p, err := StatePath()
	if err != nil {
		return err
	}
	return writeJSON(p, s)
}
