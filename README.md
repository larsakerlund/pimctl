# pimctl

[![CI](https://github.com/larsakerlund/pimctl/actions/workflows/ci.yml/badge.svg)](https://github.com/larsakerlund/pimctl/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/larsakerlund/pimctl?sort=semver)](https://github.com/larsakerlund/pimctl/releases)
[![Go Reference](https://pkg.go.dev/badge/github.com/larsakerlund/pimctl.svg)](https://pkg.go.dev/github.com/larsakerlund/pimctl)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Batch-activate the **Azure resource roles** you are eligible for through
Microsoft Entra Privileged Identity Management, from the command line, across
several tenants — instead of clicking through the portal one role at a
time.

```console
$ pimctl up --role "Cost Management Contributor" --for 1h -j "landing zone work" -y
#  ROLE                         SCOPE                           TYPE             UNTIL             RESULT
1  Cost Management Contributor  Contoso landin… (contoso-prod)  ManagementGroup  2026-09-04 13:41  ✓ ACTIVATED
```

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/larsakerlund/pimctl/main/install.sh | sh
```

The script picks the release built for your OS and architecture, checks it
against the release's own sha256 list, and installs `pimctl` into
`~/.local/bin`. `PIMCTL_INSTALL_DIR` puts the binary somewhere else and
`PIMCTL_VERSION=v0.1.1` pins a release; `sh install.sh -h` lists the lot.
Setting `GITHUB_TOKEN` raises GitHub's API rate limit; installing does not need
one.

With Go instead:

```sh
go install github.com/larsakerlund/pimctl/cmd/pimctl@latest
```

Or download an archive from [Releases](https://github.com/larsakerlund/pimctl/releases).
For the zsh completion and a version string carrying the commit, clone and run
`make install`, which writes `~/.local/bin/pimctl` and
`~/.local/share/zsh/site-functions/_pimctl`.

You need the [Azure CLI](https://learn.microsoft.com/cli/azure/). That is all:
pimctl works with a plain `az login`.
[cloudctx](https://github.com/eliknut/cloudctx) — a separate tool that gives
each context its own Azure CLI login store — is optional, and worth having if
you work across tenants: pimctl follows the context a shell is scoped to and
adds the `-c` and `--all-contexts` flags when it is installed.

## Quick start

With one tenant, an `az login` is the whole setup:

```sh
az login                 # once

pimctl ls                # what am I eligible for?
pimctl up                # pick roles interactively (just type to filter)
pimctl status            # what do I hold, and for how long?
pimctl down              # give it all back
```

Across tenants, [cloudctx](#authentication) keeps their logins apart, and
pimctl follows whichever one the shell is scoped to:

```sh
cloudctx use acme        # pick the context once, for this shell
pimctl ls                # …and every command follows it

pimctl ls -c globex      # or name the context per command
```

That is the whole daily loop. `up`, `down` and `ls` are the everyday spellings;
`activate`, `deactivate` and `list` are the same commands under their original
names.

**Which tenant, and how pimctl knows.** With cloudctx: `cloudctx use <name>`
once per shell, or `-c <name>` per command. Without it, or outside a context
window, pimctl uses the shared `az login` and prints the tenant and user that
resolved to, so every run says which login it used.

**In scope:** Azure resource roles — management group, subscription, resource
group and resource RBAC. **Not in scope:** Entra ID directory roles and PIM for
Groups. They need Microsoft Graph scopes the Azure CLI's app registration does
not have, and a token minted by `az` gets a 403 no matter what it asks for.
Group-derived eligibility *is* supported, and is the normal case.

## Commands

| Command | What it does |
|---|---|
| `pimctl ls` | every eligible role, with a stable selection key and whether it is active |
| `pimctl up` | activate: interactively, by flags, or from a preset |
| `pimctl status` | what is activated right now, and for how long |
| `pimctl down` | give roles up: everything, or a named selection |
| `pimctl preset list\|show\|delete` | saved selections |
| `pimctl cache clear` | drop cached tokens, listings and policies (`--all` also forgets what this machine activated) |
| `pimctl version` | the release tag, or the commit for a local build |

### Selecting roles

Without flags, `up` and `down` open a type-to-filter picker: type to narrow,
`tab` toggles, `ctrl+a` toggles everything matching, `enter` confirms, `esc`
clears the filter and quits when it is already empty.

| Flag | Meaning |
|---|---|
| `--key abc12345` | the stable key from `pimctl ls -o json`; unambiguous, and preferred in scripts |
| `--role NAME` | exact role name when one matches, otherwise substring (repeatable) |
| `--scope ID` | scope id **or** display-name substring (repeatable) |
| `--all` | everything you are eligible for |
| `--preset NAME` | a saved selection, which also supplies the contexts |
| `--for 2h` | how long: `2h`, `90m`, `1h30m` or `PT2H30M` |
| `-j`, `--justification` | goes into the PIM audit log; required when there is no terminal |
| `-y`, `--yes` | skip the confirmation |

`--role` and `--scope` are AND-ed with each other. The default duration is each
role's own policy maximum, which differs per role *and* per scope; `--for` is a
cap, not a target, and pimctl reports the clamp. An unattended run refuses more
than ten roles without `--force`.

### Presets

```sh
pimctl up --role Owner --scope contoso-prod --save-preset daily
pimctl up daily          # replays it, in exactly the contexts it names
pimctl down daily
```

A role in a preset that is no longer eligible is reported and skipped, so a
preset survives an access review.

### JSON

Every command speaks one vocabulary under `-o json`: `key`, `role`, `scope`,
`scopeName`, `scopeLabel` (the scope as the tables print it), `scopeType`,
`since`/`until` for an activation window, and `eligibleUntil` for when the
*eligibility* lapses.

```console
$ pimctl status -o json | jq '{unconfirmed: .unconfirmedScopes, held: [.roles[] | {key, role, until, state}]}'
{
  "unconfirmed": [],
  "held": [
    { "key": "a1b2c3d4", "role": "Cost Management Contributor", "until": "2026-09-07T22:14:27Z", "state": "confirmed" }
  ]
}
```

`status -o json` is an object, not an array. `state` is `confirmed`,
`confirming` (activated here, Azure's listing has not caught up — the role *is*
held) or `unconfirmed` (Azure did not answer for that scope).
**An empty `roles` with a non-empty `unconfirmedScopes` means *unknown*, not
*nothing*.** See [the propagation window](docs/design.md#the-propagation-window).

## Exit codes

| Code | Meaning |
|---|---|
| 0 | every selected role reached a good terminal state, or was already there |
| 1 | at least one role failed or never reached a final status in time — and every usage or setup error |
| 2 | nothing failed, but something is **waiting for an approver**: the change has not taken effect. After `up` the role is not active; after `down` it is still active |
| 130 | interrupted; requests already sent may still be in flight |

Exit 2 exists so a script cannot read "queued for approval" as success.

## Authentication

pimctl never logs in. It asks the Azure CLI for an ARM token and talks to
`management.azure.com` directly.
[cloudctx](https://github.com/eliknut/cloudctx) — a separate tool that keeps one
Azure CLI login store per context — is optional: without it, everything works against your `az login` except `-c`
and `--all-contexts`, which are cloudctx's own features and say so if you use
them without it.

When cloudctx *is* used it must be **1.4.0 or newer**, the release that
declared its [companion contract](https://github.com/eliknut/cloudctx/blob/main/docs/companions.md).
pimctl drives that contract — `cloudctx exec` to run az inside a context,
`cloudctx list --names` to enumerate them, `cloudctx show` for the tenant a
context is pinned to and where its store is — and refuses an older one with a
single message naming the version needed, rather than guessing at surfaces
nobody declared stable. An older cloudctx does not affect the shared `az login`
path, which never goes through cloudctx at all.

Each context's ARM token and activation record live inside that context's own
store, `$CLOUDCTX_STORE/pimctl/`, so `cloudctx delete <name>` sweeps them along
with the rest of that context's store; files written by an earlier pimctl are moved
there on first use. Role listings, policies and anything belonging to the shared
`az login` stay under `$XDG_CACHE_HOME/pimctl` and `$XDG_STATE_HOME/pimctl`.

In precedence order: `--bare-az`, then `-c`,
`--all-contexts`, a preset's contexts, `$CLOUDCTX_CONTEXT`, and finally the
shared `az login` — which prints the tenant and user it resolved to, so every
run says which login it used. `pimctl help auth` explains it in
the terminal, and [SECURITY.md](SECURITY.md) says what is cached where.

## Troubleshooting

| Symptom | What it means |
|---|---|
| `FAILED … MfaRule` | the token lacks an MFA claim — `cloudctx login <ctx>`, or `az login` |
| a claims challenge | pimctl prints the exact `az login --claims-challenge` command to run |
| `requires ticket information` | re-run with `--ticket-number` and `--ticket-system` |
| `at least 5 minutes` | PIM will not deactivate a role activated less than five minutes ago |
| `not finished propagating` | the role is **still active**; wait a minute and retry the deactivation |
| `N scope(s) unconfirmed (slow ARM)` | the listing is incomplete, not empty — `pimctl status --wait` reads it properly |
| `status` shows nothing you believe you hold | deactivate it **by name**; a named `down` asks ARM regardless of the listing |

Longer explanations, and the measurements behind these behaviours, are in
[docs/design.md](docs/design.md#troubleshooting-in-depth).

## Development

```sh
make check      # everything that has to be green before a change is done
```

`make check` runs the formatters in diff mode, the linters — golangci-lint over
the Go, shellcheck over `install.sh` — `go vet`, the documentation checker,
govulncheck, the race-enabled unit tests — twice,
the second time with cloudctx stripped from `PATH`, because pimctl has to work
without it — and the expect(1) picker tests. `make test-install` is separate
because it downloads a release: it installs one into a temporary directory,
checks the binary reports that release, and checks a tampered archive is
refused.
`.golangci.yml` enables every linter golangci-lint ships
and disables only what carries a one-line reason in the file; documentation is
enforced by `internal/tools/doccheck`, which will fail a change whose new
declarations have no comments.

[CONTRIBUTING.md](CONTRIBUTING.md) is one page and says what that gate expects.
[CLAUDE.md](CLAUDE.md) is the long version of the conventions, and
[docs/design.md](docs/design.md) is why the tool is shaped the way it is.

## Licence

MIT — see [LICENSE](LICENSE).
