# Usage

The README is the two-minute version. This is everything else: the commands,
how roles are selected, the JSON vocabulary, how pimctl decides which login to
use, and the full list of messages you might have to act on.

## Commands

| Command | What it does |
|---|---|
| `pimctl ls` | every eligible role, with a stable selection key and whether it is active |
| `pimctl up` | activate project requirements when present; otherwise interactively, by flags, or from a preset |
| `pimctl init` | choose project scopes and roles; create `.pimctl.yaml` without activating |
| `pimctl status` | what is activated right now, and for how long |
| `pimctl down` | give roles up: everything, or a named selection |
| `pimctl preset list\|show\|delete` | saved selections |
| `pimctl cache clear` | drop cached tokens, listings and policies (`--all` also forgets what this machine activated) |
| `pimctl version` | the release tag, or the commit for a local build |

`up` and `ls` are the everyday spellings of `activate` and `list`. With no
selection, `down` selects listed and locally recorded active roles and asks for confirmation;
`deactivate` opens a picker instead.

## Selecting roles

Without a project file or selection flags, `up` opens a type-to-filter picker;
`deactivate` also uses the picker by default: type to narrow,
`tab` toggles, `ctrl+a` toggles everything matching, `enter` confirms, `esc`
clears the filter and quits when it is already empty.

| Flag | Meaning |
|---|---|
| `--key abc12345` | the stable key from `pimctl ls -o json`; unambiguous, and preferred in scripts |
| `--role NAME` | exact role name when one matches, otherwise substring (repeatable) |
| `--scope ID` | scope id **or** display-name substring (repeatable) |
| `--at ID` | activate selected roles at this exact scope, provided Azure confirms eligibility there |
| `--all` | everything you are eligible for |
| `--preset NAME` | a saved selection, which also supplies the contexts |
| `--for 2h` | how long: `2h`, `90m`, `1h30m` or `PT2H30M` |
| `-j`, `--justification` | goes into the PIM audit log; required when there is no terminal |
| `-y`, `--yes` | skip the confirmation |

`--role` and `--scope` are AND-ed with each other. The default duration is each
role's own policy maximum, which differs per role *and* per scope; `--for` is a
cap, not a target, and pimctl reports the clamp. An unattended run refuses more
than ten roles without `--force`.

Context names are case-sensitive. Selection keys printed by older versions
remain accepted when they identify only one role; an ambiguous old key is
refused. Narrow the context with `-c` or use the current key from `ls`.

## Project access

Run this once in the directory where the file should live:

```sh
pimctl init                  # browse scopes, select roles, write .pimctl.yaml
pimctl init -c work          # the same, using a particular cloudctx login
```

The scope picker offers subscriptions, resource groups and resources, eligible
grant scopes (including management groups), or a pasted full ARM ID. Type to
filter; Enter chooses the highlighted scope. The role picker uses the usual Tab
toggles. You can add another scope before writing. Setup verifies the selection,
activates nothing, and refuses to overwrite an existing file.

You can also create it from a personal preset, or without a picker:

```sh
pimctl init --from-preset daily -c work
pimctl init --at /subscriptions/SUBSCRIPTION_UUID/resourceGroups/dev --role Reader
```

`--at` is repeatable for setup. `--from-preset` verifies the preset's targets
using the selected login; the preset's personal context names are not copied.
`init --file PATH` writes a different new file. Edit the YAML to change an
existing selection, or generate a replacement at another path and review it.

The committed file needs only a tenant and exact role targets:

```yaml
tenant: 11111111-1111-4111-8111-111111111111
roles:
  - roleDefinitionId: acdd72a7-3385-48ef-bd42-f606fba81ae7 # Reader
    scope: /subscriptions/33333333-3333-4333-8333-333333333333/resourceGroups/dev
```

Use your real tenant, subscription and role identifiers. `init` supplies these
and readable role-name comments. Names in comments are informational; role UUIDs
and complete scope IDs select access. No user IDs, eligibility schedule IDs,
cloudctx names, credentials, durations or justifications belong in the file.
Unknown fields, invalid IDs, duplicate targets and multiple YAML documents are
errors. The file describes requirements; it does not grant eligibility.

```sh
pimctl up                    # enable this project's required roles
pimctl up --for 1h           # cap new activation windows
pimctl status --project      # inspect every exact requirement
pimctl down --project        # give up the file's exact roles
pimctl up --no-project       # use the ordinary picker
```

`up` discovers the nearest `.pimctl.yaml` from the current directory upwards,
stopping at a Git repository/worktree root, home or filesystem root. It uses one
file, with no parent merging. A malformed discovered file is an error. An
explicit preset, `--role`, `--scope`, `--key`, `--all` or `--at` bypasses discovery.
`--project` requires the default file; `--file PATH` explicitly selects another
file. Explicit project selection cannot be combined with role selectors.

**Bare `down` and bare `status` have the same meaning in every directory.**
`down --project` and `status --project` require the file; if a branch removes it,
they error instead of falling back. Project deactivation acts on current file
contents, not a remembered project session. Roles used by several projects are
shared: deactivating an exact role affects all work using that activation.
Broader parent activations are left alone.

Before activation, pimctl prints the selected file and login, checks the tenant,
and resolves every requirement and required policy. Missing eligibility,
ambiguous source policies, failed lookups or missing ticket information stop the
whole preflight before any activation request. Each teammate may qualify via a
different parent scope or group. Equivalent grants at one scope use direct
membership first; distinct source scopes or conditions are reported as ambiguous. Ordinary
`up --key KEY --at SCOPE` can activate the preferred source shown by `ls`;
choosing a different same-scope conditional grant is not supported. Azure remains the
final authority, so runtime failures can still leave a partially activated set;
results name each outcome and preserve the ordinary exit codes.

Repeated project `up` preserves an exact activation confirmed by Azure and
reports its remaining window. It does not renew that window or ask for a
justification for it. Slow or failed activation reads are named; provisional
local records never become proof. New activations retain the usual policy
maximum, `--for` cap, approval flow and justification rules. Unattended use still
requires `-j` and `-y`.

Project status lists every requirement as active, not active, confirming,
unconfirmed or unknown. Broader activations encountered in the listing are
reported separately, using resource paths or cached ARM ancestry evidence. This
is disclosure of known broader access, not a complete ancestry inventory; JSON
mode prints these disclosures to stderr. This describes PIM activation state, not whether an
application is ready or an existing credential has refreshed. Status is
observational: an inactive requirement alone does not change its exit code;
a failed or incomplete lookup does. JSON adds `requirements` beside `roles`
and `unconfirmedScopes`. The lossy diagnostic `--all-scopes` mode is refused
for project operations.

cloudctx owns login selection and credential isolation. pimctl owns requirement
resolution, policy checks, activation and deactivation. Use the ambient login or
`-c YOUR_CONTEXT`; a tenant mismatch stops with the required tenant named.
pimctl does not switch your shell context, choose a personal context from the
file, install tools or start the application. cloudctx stays optional. Other
tools can invoke these ordinary CLI commands and consume the existing exit codes.

### Activating below the eligible scope

`--scope` still filters the eligible listing. `--at` chooses the exact activation
target for selected roles:

```sh
pimctl up --key ABC12345 \
  --at /subscriptions/SUBSCRIPTION_UUID/resourceGroups/dev
```

Azure must confirm the source applies at that target. Management-group ancestry
is established through ARM, not guessed from names. A failed check never falls
back to activating at the broader grant. `--save-preset NAME` remembers the
narrower target for later `up NAME` and `down NAME`.

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

`ls` prints local activation state immediately, then reconciles against Azure
and reports corrections on stderr. Use `ls --with-active` to wait for the merged
answer before printing. Its JSON rows include `confirmed` and `state`, matching
the confidence fields in `status`. An `active: false` row with `confirmed: false`
does not prove the role is inactive. The table uses `?` for unconfirmed state and
`~` for a local activation awaiting listing confirmation.

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

## Installation

The installer linked from the README downloads the release for your platform,
verifies its SHA-256 checksum, and installs it in `~/.local/bin`. Follow its PATH
instructions if needed. Archives are also available from
[Releases](https://github.com/larsakerlund/pimctl/releases).

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
| `N scope(s) unconfirmed (ARM did not answer)` | the listing is incomplete, not empty — failed reads preserve local records; `pimctl status --wait` allows longer reads but can still return incomplete results |
| `status` shows nothing you believe you hold | deactivate it **by name**; a named `down` asks ARM regardless of the listing |

Longer explanations, and the measurements behind these behaviours, are in
[design.md](design.md#troubleshooting-in-depth).
