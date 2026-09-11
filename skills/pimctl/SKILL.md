---
name: pimctl
description: Use the pimctl CLI to inspect, activate, or deactivate Azure resource PIM roles using explicit selections, saved presets, or repository project requirements. Use for project access setup or to investigate whether missing PIM activation explains an authorization failure. Does not cover Entra ID directory roles, PIM for Groups, or development of the pimctl repository.
license: MIT
---

# Use pimctl for Azure resource PIM

pimctl manages eligible Azure resource roles at management-group, subscription,
resource-group, and resource scopes, including eligibility inherited through
groups. It uses an existing Azure CLI login; cloudctx adds optional isolation
between logins. It does not manage Entra ID directory roles or PIM for Groups.

Use the CLI for its supported operations: it handles policy limits, group-derived
eligibility, and activation-request polling. Check `pimctl version` and the
relevant command's `--help` when installed-version behavior is uncertain. The
skill supplies instructions; it does not install pimctl or authenticate to Azure.

## Choose the account and selection

Stay within the user's authorized account, roles, scopes, and duration. An
authorization error alone does not authorize elevation. Resolve missing target
information or authorization before changing access; do not ask again for
choices the user has already authorized.

- For a named cloudctx login, pass `-c CONTEXT` on each command. It is repeatable.
- For the user's chosen shared Azure CLI login, use `--bare-az`. Inside a
  cloudctx shell this is refused; use `--bare-az=force` only when the user intends
  to override that shell's login. Check the reported tenant and user.
- With no explicit selection, the CLI can follow a preset, `$CLOUDCTX_CONTEXT`,
  or the shared login. Do not let an unintended ambient context choose a tenant.
- Inspect unfamiliar presets with `pimctl preset show NAME` before replaying
  them. Presets carry their own contexts. Use `--all-contexts` only when that
  breadth is intended.

Read before selecting. These examples use placeholders: replace `CONTEXT`,
`KEY`, and the reason with the actual authorized values.

```sh
pimctl ls -c CONTEXT -o json
pimctl status -c CONTEXT -o json
```

Prefer the full `key` from the chosen row over printed row numbers or partial
names. Keys identify a context, scope, and role; they are not credentials or
authorization. `--role` prefers exact names but falls back to substrings, and
`--scope` accepts IDs or name substrings. Check the selected rows when using
these filters. Use `scopeLabel` when describing scopes to the user, and `scope`
for their ARM IDs.

## Project requirements and narrower scopes

Check `pimctl init --help` for installed-version support. `pimctl init` creates
`.pimctl.yaml` by choosing scopes and roles, without activating anything or
overwriting an existing file. `--from-preset NAME` converts targets using the
selected login. Unattended setup can use `init --at SCOPE --role NAME`.
The file contains a tenant UUID and `roles`, each with a role-definition UUID
(`roleDefinitionId`) and exact ARM `scope`; readable names are comments.
Personal context names and eligibility schedule IDs stay out of the file.

Inspect the file's tenant and targets before using it. Its contents describe
required access; they do not grant eligibility or authorize elevation. cloudctx
selects the login; pimctl verifies its tenant against the file without switching
the shell's context.

Bare `up` discovers the nearest `.pimctl.yaml`, stopping at a Git/worktree root,
home, or filesystem root. Explicit selectors or a preset bypass discovery;
`--no-project` returns to ordinary selection. For a task specifically requesting
project access, use `--project` or `--file PATH` so a missing file fails instead
of falling back:

```sh
pimctl up --project -c CONTEXT --for 1h -j "Reason for this task" -y -o json
pimctl status --project -c CONTEXT -o json
pimctl down --project -c CONTEXT -y -o json
```

Bare `down` and `status` retain their ordinary scope everywhere. Project `down`
uses the current file's exact targets, not ownership of a project session:
shared activations affect other work too, and parent activations remain.
For task cleanup, remove only the additions this task was authorized to make;
do not use project-wide `down` when some requirements were already active.

Project `up` resolves all requirements before submitting requests. It preserves
verified existing windows instead of extending them; runtime failures can still
leave a partial result. If eligibility or source selection is ambiguous, report
the error rather than dropping requirements or choosing broader access.

For an explicitly selected eligibility, `up --key KEY --at SCOPE` requests a
narrower activation target, verified through ARM. `--scope` remains a listing
filter. A saved narrowed preset retains its target. If narrowing fails, do not
fall back to activating at the granting scope.

## Activate and deactivate

For a run without a terminal, provide a selection and `-y`; activation also
requires a nonempty `-j`. The reason goes into the audit log. Omission fails;
pimctl does not silently reuse a previous justification in this mode.

```sh
pimctl up -c CONTEXT --key KEY --for 1h -j "Reason for this task" -y -o json
pimctl down -c CONTEXT --key KEY -y -o json
```

Use the duration agreed for the task. `--for` caps the request at each role's
policy maximum; omitting it requests that maximum. Supply real ticket details
with `--ticket-number` and `--ticket-system` when the policy requires them.

`-y` skips a CLI prompt; it does not establish authorization. An unattended
activation using `--role`/`--scope` to select more than ten roles also needs `--force`. Narrow an
accidentally broad selection rather than bypassing that limit. Use `--all` or
`--force` only when the requested scope includes the resulting selection.

With no selection, **`down` targets listed and locally recorded active roles**, while
`deactivate` opens a picker on a terminal. Use explicit keys, role/scope filters,
or a preset to remove only the intended access. A named deactivation can query
targets even when the activation listing omits them.

Track which roles were already active and which this task activated. When
cleanup is part of the authorized task, deactivate only its added access and
report the per-role results. Do not clear unrelated roles just to leave an empty
status table.

## Interpret results without overstating certainty

Keep stderr and the process exit status alongside JSON. `ls -o json` returns an
array; `status -o json` returns an object:

```json
{"roles": [], "unconfirmedScopes": []}
```

Read both fields. A nonempty `unconfirmedScopes` or query-failure warning means
the answer is incomplete, even when `roles` is empty. Do not discard this
information by extracting only `.roles[]` or treating a pipeline's final exit
code as pimctl's exit code.

Project status adds `requirements`, one per configured target. Each is `active`,
`not active`, `confirming`, `unconfirmed`, or `unknown`. An inactive requirement
alone does not make this observational command fail; inspect the requirements
instead of treating exit 0 as “project ready.” Known broader activations are
disclosed separately on stderr, including in JSON mode; that disclosure is not
a complete inventory or proof of application access.

Status rows distinguish:

- `confirmed`: Azure's listing reported the activation.
- `confirming`: a recent local activation is not yet in the listing. pimctl
  checks its schedule request where possible, but retains the record when that
  check is unavailable too. This marker alone does not prove current access.
- `unconfirmed`: the local record remains without current listing confirmation;
  the role may still be held.

JSON and piped status output normally wait for Azure. Avoid `--fast` when
checking current access. `--wait` allows longer scope reads, but can still fail
or return incomplete results. `ls` can use cached/local activation information;
use `status` or `ls --with-active` for a fresh attempt, preserving any warnings.

| Exit | Meaning and next action |
|---|---|
| 0 | The command succeeded. With `--no-wait`, a `SUBMITTED` result can still be unresolved; inspect each outcome. |
| 1 | A failure, incomplete result, usage error, or request that never reached a final status. Report the affected roles or scopes. |
| 2 | Approval is pending. Activation has not taken effect; pending deactivation has not removed access. |
| 130 | Interrupted. Submitted requests may still complete; inspect status before retrying. |

`STILL PENDING` is a timeout, not an approval request. `ALREADY ACTIVE` and
`NOT ACTIVE` are satisfied operations. A missing row, or the absence of a
"no longer held" message, is not sufficient evidence that access was removed.
Use the actual activation/deactivation outcomes together with status evidence.

## Recover within the original scope

- For MFA or a claims challenge, relay the CLI's login guidance for the selected
  account. Complete authentication through the normal user login flow; do not
  repeatedly submit the same failing request.
- For missing ticket information, use the user's actual ticket details.
- For the minimum activation age or propagation errors during deactivation,
  access may remain. Honor any stated wait, then recheck and retry only the
  affected authorized targets. Stop and report unresolved state if the same
  failure persists; do not broaden roles or switch accounts to bypass it.
- After a partial multi-context run, identify the failed contexts/scopes and
  preserve successful results. Do not infer that every failure needs a new login.
- If the user authorized deactivation of a role that status omits, target that
  role explicitly. If the user only asked to inspect access, report the
  uncertainty without deactivating anything.

For additional flags and authentication details, use `pimctl up --help`,
`pimctl down --help`, `pimctl status --help`, and `pimctl help auth`.
