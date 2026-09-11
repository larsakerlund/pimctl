# Project access

[Usage](usage.md) · [Selecting roles](roles.md) · [Scripting](scripting.md)

Commit a `.pimctl.yaml` so teammates can enable the project's roles with
`pimctl up`. Each person still needs their own eligible access.

## Create the file

From the directory where the file should live:

```sh
pimctl init              # choose scopes and roles
pimctl init -c work      # use a named cloudctx login
```

Browse or paste a scope, select roles with **Tab**, and confirm with **Enter**.
You can add more scopes before saving. Setup verifies the selection, activates
nothing, and refuses to overwrite an existing file.

Already know the selection? Use either:

```sh
pimctl init --from-preset daily
pimctl init --at /subscriptions/SUBSCRIPTION_UUID/resourceGroups/dev --role Reader
```

`--at` is repeatable during setup. To change an existing file, edit it or generate
a replacement with `init --file PATH` and review the result.

## Use it day to day

```sh
pimctl up --for 1h       # enable required roles for up to an hour
pimctl status --project  # check each requirement
pimctl down --project    # deactivate the file's exact roles
```

Repeated `up` keeps verified existing activations and their remaining windows;
it does not extend them. Before requesting new activations, pimctl checks the
file's tenant, every role's eligibility and required policies. Missing or
ambiguous eligibility stops that check before any role is activated. Azure can
still reject individual requests afterwards, so inspect every result.

**Bare `down` and `status` are unchanged.** Project deactivation uses the current
file's exact roles, including roles also used by other projects. It leaves
broader parent activations alone; it does not track ownership of a project session.

Project status reports every requirement, including inactive or uncertain ones.
Exit 0 means the lookup succeeded, not that every requirement is active.
Known broader activations are reported separately; this is not a complete
inventory of access. See [Scripting](scripting.md) for JSON and exit codes.

## What goes in the file?

Only the tenant and exact role targets. `init` fills in the real IDs and adds
readable role-name comments:

```yaml
tenant: 11111111-1111-4111-8111-111111111111
roles:
  - roleDefinitionId: acdd72a7-3385-48ef-bd42-f606fba81ae7 # Reader
    scope: /subscriptions/33333333-3333-4333-8333-333333333333/resourceGroups/dev
```

Names in comments are informational. Personal context names, credentials,
durations and justifications stay out of the file. Unknown fields, invalid IDs
and duplicate targets are rejected. A role may be eligible higher up and
activated at the narrower scope recorded here.

## Which file and login are used?

- `up` finds the nearest `.pimctl.yaml`, walking upwards to the Git/worktree root,
  home or filesystem root. Files are never merged; malformed files cause errors.
- `--project` requires that file. `--file PATH` selects another file explicitly.
  A missing file is an error, including for project `status` and `down`.
- `up --no-project` opens ordinary selection. An explicit preset or role selector
  also bypasses discovery. Explicit project selection cannot mix with selectors.
- Use your existing login or `-c CONTEXT`. The file cannot choose a personal
  context; a tenant mismatch stops the command.

cloudctx selects and isolates logins; pimctl resolves and activates roles.
pimctl does not switch your shell context or start the application. cloudctx is
optional; see [Setup and accounts](setup.md#choose-a-login).

For resolution rules and implementation details, see the
[project design notes](design.md#project-requirements-and-reduced-activation-scopes).
