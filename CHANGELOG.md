# Changelog

Notable changes to pimctl. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[semantic versioning](https://semver.org/spec/v2.0.0.html).

While the major version is 0, a minor bump may change output shapes and flags.
The exit-code contract will not change without a major version.

## [Unreleased]

Nothing yet.

## [0.2.0] - 2026-09-09

First public release. Earlier versions were released from a private repository; their notes are kept below for reference.

### Changed

- **Requires cloudctx >= 1.4.0 when cloudctx is used; still works without
  cloudctx.**
  1.4.0 is the release that declared cloudctx's companion contract
  (`docs/companions.md`), and pimctl drives that contract rather than the
  undeclared surfaces it used before: context names come from
  `cloudctx list --names` instead of the internal `_names` and the human
  listing, and `CLOUDCTX_STORE` joins the managed variables a bare `az` is
  stripped of. An older cloudctx is refused with one message naming the version
  needed and `cloudctx self-update`; it is not silently worked around. pimctl
  still works with no cloudctx at all, and the shared `az login` path is
  unaffected either way. The version is read from `cloudctx --version` once per
  binary per day, because a cloudctx spawn costs ~0.65 s.
- **A context's token and activation record now live inside that context.**
  Both move to `$CLOUDCTX_STORE/pimctl/`, so `cloudctx delete <name>` sweeps
  the context's cached credential and the record of what this machine activated
  there, and `--keep-store` keeps them. Files written by an earlier pimctl are
  moved there the first time the store answers; `pimctl cache clear` sweeps both
  locations. Role listings, policies, and everything belonging to the shared
  `az login`, stay under `$XDG_CACHE_HOME/pimctl` and `$XDG_STATE_HOME/pimctl`,
  as does all of it on a machine without cloudctx.

### Added

- **A one-line installer.** `install.sh` picks the release archive for the
  machine's operating system and architecture, checks it against the release's
  own sha256 list, and installs the binary into `~/.local/bin`.
  `PIMCTL_INSTALL_DIR` changes where it goes and `PIMCTL_VERSION` which release
  it is. It needs no credential; `GITHUB_TOKEN`, when the environment has one,
  is sent to raise GitHub's API rate limit and never reaches the command line or
  the output. The README's Install section leads with it.
- `make test-install` runs the installer end to end against the latest release:
  it has to install a binary that reports that release, and it has to refuse an
  archive whose checksum does not match. CI runs it on ubuntu and macOS, and
  `make lint` now runs shellcheck over `install.sh` and the shell tests.

## 0.1.1 - 2026-09-08

cloudctx becomes optional, and pimctl is stricter about which login a cached
token belongs to.

### Changed

- **A bare `az` is spawned with cloudctx's variables stripped.** `CLOUDCTX_CONTEXT`,
  `CLOUDCTX_AZURE_LABEL`, `AZURE_CONFIG_DIR` and the three AWS variables are
  removed from the child environment, so the shared login is the shared login
  wherever pimctl is run from. `--bare-az` inside a context window is refused
  and names the context; `--bare-az=force` runs anyway.
- **Cached tokens are verified against the tenant they are meant for, on every
  read.** For a named context the expected tenant comes from `cloudctx show`, a
  registry read with no `az` spawn; for the shared login it comes from az's own
  `azureProfile.json`. A cached token whose tenant does not match is discarded
  and a new one minted.
- **cloudctx is optional, and pimctl behaves like it.** A machine with only the
  Azure CLI runs the whole daily loop against its `az login`; `-c` and
  `--all-contexts` are cloudctx's own features and say so when it is absent,
  rather than reporting "executable file not found". Context completion offers
  nothing instead of failing, and advice that names cloudctx is printed only on
  machines that have it.
- The comment standard says comments describe the code as it is; a decision that
  was reversed belongs in `git log` or the "Decisions that were reversed"
  section of `docs/design.md`.
- `make check` runs the test suite twice, the second time with cloudctx stripped
  from `PATH`, so "works without cloudctx" is enforced rather than remembered.

### Fixed

- **`--all-contexts` on an empty registry acted on a context called "no".**
  `cloudctx list` says "no contexts. Create one with: …", which the line parser
  read as a name. Context names now come from `cloudctx _names`, with the human
  listing as a fallback that recognises the empty case.
- **An interactive run no longer replays the last justification unasked.** When
  no selected role's policy requires one, the neutral default is sent instead:
  the remembered text is a prefill for the prompt, not something to send on your
  behalf.
- `--all-contexts` with cloudctx installed and its registry empty printed
  `no context could be used:` followed by `%!w(<nil>)`. It now says that
  cloudctx has no contexts yet and names both ways forward: create one, or run
  without `-c` against the `az login`.
- Documentation referred to `cloudctx logout <ctx>`, which does not exist; the
  command is `cloudctx exec <ctx> -- az logout`.

## 0.1.0 - 2026-09-07

First tagged release. pimctl batch-activates the Azure resource roles you are
eligible for through Entra PIM, across several tenants, from one
command.

### Added

**The daily loop.** `ls`, `up`, `status` and `down`, with `list`, `activate` and
`deactivate` as their original names. A type-to-filter picker when no selection
flags are given — type to narrow, `tab` toggles, `ctrl+a` toggles everything
matching — that draws inline and erases itself, leaving a one-line summary in
scrollback rather than a dead frame.

**Selection that cannot be misread.** A stable `key` per role, emitted by every
command under `-o json` and taken by `--key`; `--role` prefers an exact name
before falling back to substring; `--scope` matches an id or a display name;
`--all`, `--preset`, and a refusal to touch more than ten roles unattended
without `--force`.

**Presets.** Saved selections that carry their own contexts, so `pimctl up
daily` opens exactly the tenants it names. A role that is no longer eligible is
reported and skipped rather than failing the run.

**Multi-tenant by construction.** Credentials are isolated per context through
cloudctx: `-c` (repeatable), `--all-contexts`, `$CLOUDCTX_CONTEXT`, or the
shared `az login` — which announces the tenant and user it resolved to, because
guessing a tenant without saying so is the one thing this tool must not do. One
unreachable context warns and is skipped; the run still exits non-zero, so a
partial answer cannot be mistaken for a complete one.

**An honest picture of what is held.** `status` renders from a local record of
what this machine activated, reconciled against Azure on every run and never a
replacement for it. Rows Azure has not confirmed are marked, `unconfirmedScopes`
is always present so an empty answer cannot be read as "nothing held", and a
role that disappears is always named on stderr.

**Speed that came from measurement.** Tokens, eligibility listings and PIM
policies cached on disk (5 min before expiry, 10 min, 24 h); the activation
listing read as a per-scope fan-out rather than ARM's tenant-wide call, which
takes 11–21 s and silently drops rows; a 3-second soft deadline per scope, with
any scope that misses it *named* rather than dropped. A warm command makes no
network call before it prints.

**Exit codes as a contract.** 0 held, 1 failed or unfinished — including every
usage error, 2 waiting on an approver (the change has *not* taken effect), 130
interrupted.

**The token cache.** It is `0600` in a `0700` directory, refused on read if the
mode has been widened, versioned, tenant-checked, and written with its mode set
before the first byte. Tokens are never logged; `--debug` reports only `cache
hit` or `miss`, and every URL in an error has its query stripped.

**Four ways a PIM tool can lie about access, each pinned by a test.** ARM's
per-scope listing runs minutes behind its own writes — 4.5 observed — so an
activation is confirmed against its own schedule request, which reports
`Provisioned` immediately. `RoleAssignmentDoesNotExist` on a deactivation, when
the listing has just shown the role active, means propagation rather than
absence and is reported as a failure. A poll that never reaches a terminal
status is a failure (1), not "pending approval" (2), because no approver is
coming. And a scope ARM did not answer for is *unknown*, not empty: it keeps
its last known state and is named.

### Known limitations

- Entra ID directory roles and PIM for Groups are out of scope: they need
  Microsoft Graph scopes the Azure CLI's app registration does not have.
- macOS and Linux only. Paths, file modes and the pty tests assume a POSIX
  machine.

[Unreleased]: https://github.com/larsakerlund/pimctl/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/larsakerlund/pimctl/releases/tag/v0.2.0
