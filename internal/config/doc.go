// Package config stores pimctl's presets and its small amount of remembered
// state under $XDG_CONFIG_HOME/pimctl, falling back to ~/.config/pimctl.
//
// Two files, each read with [LoadPresets] or [LoadState] and replaced whole and
// atomically by [SavePresets] or [SaveState]:
//
//   - presets.json — named selections of roles. `--save-preset <name>` writes
//     the roles just activated; `--preset <name>` selects them again on a later
//     run.
//   - state.json — one field, the justification last sent, so the prompt can
//     offer it again and an unattended run has something better than the
//     default to say.
//
// A [PresetEntry] is a saved *selection*, not a grant. It holds the cloudctx
// context, the ARM scope and the role definition id — the identity that finds
// the same eligible role again — plus the role and scope names as they read
// when it was saved, kept only so an entry that no longer resolves can be
// reported in words rather than as a GUID.
//
// Nothing here has any authority over Azure. Eligibility, activation and expiry
// all live in the tenant; a preset naming a role the user is no longer eligible
// for simply comes back as missing, and deleting either file costs a saved
// shortcut and a remembered sentence and nothing more. Both are written 0600 in
// a 0700 directory all the same, because between them they name every scope and
// role this account can reach.
//
// A missing file is not an error: it reads as an empty preset set or zero
// state. Only an unreadable or unparseable file is reported, since silently
// treating a corrupt presets.json as "no presets" would look like the presets
// had been lost.
package config
