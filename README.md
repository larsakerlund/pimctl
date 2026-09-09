# pimctl

[![CI](https://github.com/larsakerlund/pimctl/actions/workflows/ci.yml/badge.svg)](https://github.com/larsakerlund/pimctl/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/larsakerlund/pimctl?sort=semver)](https://github.com/larsakerlund/pimctl/releases)
[![Go Reference](https://pkg.go.dev/badge/github.com/larsakerlund/pimctl.svg)](https://pkg.go.dev/github.com/larsakerlund/pimctl)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Activate the **Azure resource roles** you are eligible for through Microsoft
Entra Privileged Identity Management, from the command line, in batch.

```console
$ pimctl up --role "Cost Management Contributor" --for 1h -j "landing zone work" -y
#  ROLE                         SCOPE                           TYPE             UNTIL             RESULT
1  Cost Management Contributor  Contoso landin… (contoso-prod)  ManagementGroup  2026-09-04 13:41  ✓ ACTIVATED
```

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/larsakerlund/pimctl/main/install.sh | sh
```

It downloads the release built for your machine, checks it against the
release's sha256 list, and installs `pimctl` into `~/.local/bin`. Run
`sh install.sh -h` for the few things you can change. Or
`go install github.com/larsakerlund/pimctl/cmd/pimctl@latest`, or an archive
from [Releases](https://github.com/larsakerlund/pimctl/releases).

## Quick start

You need the [Azure CLI](https://learn.microsoft.com/cli/azure/) and an
`az login`. That is the whole setup.

```sh
pimctl ls                # what am I eligible for?
pimctl up                # pick roles interactively (just type to filter)
pimctl status            # what do I hold, and for how long?
pimctl down              # give it all back
```

`pimctl --help` lists every command, and `pimctl up --help` the flags for
selecting roles without the picker. [docs/usage.md](docs/usage.md) is the
longer reference.

## Presets

```sh
pimctl up --role Owner --scope contoso-prod --save-preset daily
pimctl up daily          # replays it
pimctl down daily
```

## Scripting

Every command takes `-o json` and speaks one vocabulary: `key` selects a role,
`until` ends an activation, `scopeLabel` is the scope as the tables print it.

| Code | Meaning |
|---|---|
| 0 | every selected role reached a good terminal state, or was already there |
| 1 | at least one role failed or never finished in time — and every usage error |
| 2 | waiting for an approver: the change has **not** taken effect |
| 130 | interrupted; requests already sent may still be in flight |

Exit 2 exists so a script cannot read "queued for approval" as success.

## Working across tenants

[cloudctx](https://github.com/eliknut/cloudctx) keeps a separate Azure CLI login
per tenant. It is optional: without it, everything above works against your
`az login`. With it, pimctl follows the context your shell is in, takes
`-c <name>` per command, and reads every context at once with `--all-contexts`.
It needs cloudctx 1.4.0 or newer. Whichever login a run uses, pimctl prints the
tenant and user it resolved to.

## Not covered

Entra ID directory roles and PIM for Groups: they need Microsoft Graph
permissions the Azure CLI's app registration does not have. Eligibility you
inherit through a group is supported, and is the normal case.

## Troubleshooting

| Symptom | What it means |
|---|---|
| `FAILED … MfaRule` | the token lacks an MFA claim — log in again with `az login` |
| `at least 5 minutes` | PIM will not deactivate a role activated less than five minutes ago |
| `status` shows nothing you believe you hold | Azure's listing lags its own writes; deactivate **by name**, which asks ARM regardless of the listing |

The rest are in [docs/usage.md](docs/usage.md#troubleshooting), with the
measurements behind them in [docs/design.md](docs/design.md).

## Development

```sh
make check      # everything that has to be green before a change is done
```

[CONTRIBUTING.md](CONTRIBUTING.md) says what that gate expects — including the
documentation checker, which fails a change whose new declarations have no
comments. [docs/design.md](docs/design.md) is why the tool is shaped the way it
is. `make install` builds from a clone and adds the zsh completion.

## Licence

MIT — see [LICENSE](LICENSE).
