# Usage

The README is the two-minute version. This is everything else: the commands,
how roles are selected, the JSON vocabulary, how pimctl decides which login to
use, and the full list of messages you might have to act on.

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

`up` and `ls` are the everyday spellings of `activate` and `list`. With no
selection, `down` selects all listed active roles and asks for confirmation;
`deactivate` opens a picker instead.

## Selecting roles

Without selection flags, `up` and `deactivate` open a type-to-filter picker: type to narrow,
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

## Presets

```sh
pimctl up --role Owner --scope contoso-prod --save-preset daily
pimctl up daily          # replays it, in exactly the contexts it names
pimctl down daily
```

A role in a preset that is no longer eligible is reported and skipped, so a
preset survives an access review.

## JSON output

`ls`, `up`, `down`, and `status` use the following vocabulary under `-o json`: `key`, `role`, `scope`,
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
`confirming` (a recent local activation that Azure's listing has not confirmed)
or `unconfirmed` (local state without current listing confirmation). pimctl
checks confirming rows against their schedule requests where possible, but
retains them when that check is unavailable too; the marker alone is not proof
of current access.
**An empty `roles` with a non-empty `unconfirmedScopes` means *unknown*, not
*nothing*.** See [the propagation window](design.md#the-propagation-window).

## Exit codes

| Code | Meaning |
|---|---|
| 0 | selected operations succeeded or were already satisfied |
| 1 | a failure, incomplete operation, or usage error |
| 2 | waiting for an approver; the change has not taken effect |
| 130 | interrupted; requests already sent may still be in flight |

With `--no-wait`, exit 0 can also mean requests were submitted without waiting
for completion. Check their state before assuming access was granted or removed.

## Other installation options

Install from Go source:

```sh
go install github.com/larsakerlund/pimctl/cmd/pimctl@latest
```

For installer options, download the script and read its help:

```sh
curl -fsSL https://raw.githubusercontent.com/larsakerlund/pimctl/main/install.sh -o install.sh
sh install.sh -h
```

To remove the default binary and cached data, run `pimctl cache clear --all`,
then `rm ~/.local/bin/pimctl`. Presets and remembered justification remain in
`$XDG_CONFIG_HOME/pimctl` (normally `~/.config/pimctl`); remove that directory
only if you also want to discard them. Remove any shell completion you installed
separately.

## Agent skill

The [pimctl skill](../skills/pimctl/SKILL.md) guides coding agents through account
selection, role changes, and interpreting incomplete or pending results.
Install it with the [Skills CLI](https://github.com/vercel-labs/skills):

```sh
npx skills add larsakerlund/pimctl --skill pimctl
```

This installs agent instructions; install the pimctl binary separately.
From a local clone, use `npx skills add ./ --skill pimctl`.

## Which login a run uses

pimctl never logs in. It asks the Azure CLI for an ARM token and talks to
`management.azure.com` directly.

In precedence order: `--bare-az`, then `-c`, `--all-contexts`, a preset's
contexts, `$CLOUDCTX_CONTEXT`, and finally the shared `az login`. Whichever it
lands on, the shared-login path prints its tenant and user; named contexts
are identified by their context names.
`pimctl help auth` explains the same thing in the terminal.

With [cloudctx](https://github.com/eliknut/cloudctx), each context's ARM token
and activation record live inside that context's own store,
`$CLOUDCTX_STORE/pimctl/`, so `cloudctx delete <name>` sweeps them with the rest
of it; account-owned files in the fallback directory are moved there on first use.
Older role caches and records without account ownership are ignored. Role
listings, policies and anything belonging to the shared `az login` stay under
`$XDG_CACHE_HOME/pimctl` and `$XDG_STATE_HOME/pimctl`. [SECURITY.md](../SECURITY.md)
lists every file and its mode.

cloudctx must be 1.4.0 or newer: that is the release that declared the
[companion contract](https://github.com/eliknut/cloudctx/blob/main/docs/companions.md)
pimctl drives — `cloudctx exec` to run az inside a context,
`cloudctx list --names` to enumerate them, `cloudctx show` for the tenant a
context is pinned to and where its store is. An older one is refused with a
single message naming the version needed. The shared `az login` path never
spawns cloudctx and is unaffected.

## Troubleshooting

| Symptom | What it means |
|---|---|
| `FAILED … MfaRule` | the token lacks an MFA claim — `cloudctx login <ctx>`, or `az login` |
| a claims challenge | pimctl prints the exact `az login --claims-challenge` command to run |
| `requires ticket information` | re-run with `--ticket-number` and `--ticket-system` |
| `at least 5 minutes` | PIM will not deactivate a role activated less than five minutes ago |
| `not finished propagating` | the role is **still active**; wait a minute and retry the deactivation |
| `N scope(s) unconfirmed (slow ARM)` | the listing is incomplete, not empty — `pimctl status --wait` allows longer reads but can still return incomplete results |
| `status` shows nothing you believe you hold | deactivate it **by name**; a named `down` asks ARM regardless of the listing |

Longer explanations, and the measurements behind these behaviours, are in
[design.md](design.md#troubleshooting-in-depth).
