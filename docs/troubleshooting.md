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
| `request exists (…): an earlier request for this role is still open` | ARM refuses a second request while an earlier one is undecided, typically one still waiting on an approver. The role is not held and the run exits 1; check `pimctl status`, and wait for the open request to be decided or withdraw it in the portal before retrying |

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

## A role you were just granted is missing from `ls`

The eligible-role listing is cached for ten minutes and each role's PIM policy
for a day. `--refresh` re-reads them from Azure for one command:

```sh
pimctl ls --refresh
```

`pimctl cache clear` deletes every cache — tokens, listings, policies and the
probed cloudctx version — so the next command starts cold; it leaves the record
of this machine's own activations alone. `pimctl cache path` prints the cache
directory, for when you want to look at the files yourself.

## Seeing what a command spent its time on

`--debug` prints a timing breakdown of each phase to stderr — the token, the
eligibility listing, each per-scope activation read — and whether the token
came from the cache. It never prints the token itself, only `cache hit` or
`cache miss`, so its output is safe to paste into a bug report alongside
`pimctl version`.
