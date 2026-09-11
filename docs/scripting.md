# Scripting

[Usage](usage.md) · [Project access](projects.md) · [Troubleshooting](troubleshooting.md)

## Run without a terminal

Supply an explicit selection and `-y`; activation also requires a nonempty
justification with `-j`. The justification goes into the PIM audit log.

```sh
pimctl up --project --for 1h -j "Develop the project" -y -o json
pimctl status --project -o json
pimctl down --project -y -o json
```

Use `--project` in project scripts so a missing file fails. For individual roles,
use `--key KEY` from `pimctl ls -o json`. Add `-c CONTEXT` to select a cloudctx
login. If policy requires a ticket, pass `--ticket-number` and `--ticket-system`.

## Read JSON results

Use `-o json` on `ls`, `up`, `down` and `status`. Keep stderr and pimctl's exit
code alongside the JSON; a pipeline can otherwise hide a failure.

Common fields include `key`, `role`, the full ARM `scope`, `scopeName`,
`scopeLabel` (the table label), `scopeType`, activation timestamps `since` and
`until`, and `eligibleUntil` for eligibility expiry.

`ls -o json` returns an array. It prints local activation state first and reports
Azure corrections on stderr. Use `ls --with-active -o json` to wait for the
reconciled result. `active: false` with `confirmed: false` does not prove inactivity.

`status -o json` returns an object:

```json
{"roles": [], "unconfirmedScopes": []}
```

Always read both fields. **Empty `roles` with nonempty `unconfirmedScopes`
means unknown, not no access.** JSON status normally waits for Azure; avoid
`--fast` when checking current access. `--wait` allows longer reads but cannot
guarantee a complete answer.

| Role `state` | Meaning |
|---|---|
| `confirmed` | Azure's listing reported the activation |
| `confirming` | A recent local activation is awaiting listing confirmation (`~` in tables) |
| `unconfirmed` | Local state lacks current listing confirmation (`?` in tables) |

pimctl checks confirming rows against their requests where possible, but the
marker alone is not proof of access. See [the propagation window](design.md#the-propagation-window).

Project status adds `requirements`, one per target, with `active`, `not active`,
`confirming`, `unconfirmed` or `unknown` state. Known broader access is disclosed
on stderr, including in JSON mode. The diagnostic `--all-scopes` option is not
supported for project operations.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Command succeeded; selected changes completed or were already satisfied |
| 1 | Failure, incomplete operation or lookup, or usage error |
| 2 | Waiting for an approver; the requested change has not taken effect |
| 130 | Interrupted; requests already sent may still be in flight |

`status` is observational: inactive project requirements alone do not cause a
failure exit. Check their states before treating the project as ready.
With `--no-wait`, exit 0 can mean requests were submitted; check their outcomes
before assuming access was granted or removed.
