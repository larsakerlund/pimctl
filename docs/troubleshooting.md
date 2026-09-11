# Troubleshooting

[Usage](usage.md) · [Setup and accounts](setup.md) · [Scripting](scripting.md)

## Activation failed

| Message | What to do |
|---|---|
| `MfaRule` | Sign in again with `cloudctx login CONTEXT`, or `az login` for the shared login |
| Claims challenge | Follow the exact login command pimctl prints |
| `requires ticket information` | Supply `--ticket-number` and `--ticket-system` |
| Project tenant mismatch | Choose a login for the required tenant |
| Missing or ambiguous project eligibility | Check the file's exact targets and your eligible roles with `ls`; pimctl will not guess a grant |

An approval-pending result (exit 2) means access has not changed yet. For a
partial failure, inspect the per-role outcomes before retrying.

## Deactivation failed

| Message | What to do |
|---|---|
| `at least 5 minutes` | Wait until the activation is five minutes old, then retry |
| `not finished propagating` | The role is still active; wait a minute and retry |

If you intend to deactivate a role omitted from status, select it explicitly
with a key, role/scope filters or a preset. Named deactivation asks ARM about the
target even when the listing misses it.

## Status is incomplete or unexpected

`N scope(s) unconfirmed (ARM did not answer)` means unknown state for those
scopes. Local records remain visible, but are not proof of current access.
Try `pimctl status --wait` to allow longer reads; it can still return incomplete
results. Keep warnings and `unconfirmedScopes` when reading JSON.

A `~` marker means a recent activation is awaiting listing confirmation; `?`
means unconfirmed local state. Missing rows do not prove access is gone.
Project status describes PIM activation, not whether an application or an
existing credential has picked up the change.

For why these states occur, see
[Troubleshooting in depth](design.md#troubleshooting-in-depth).
