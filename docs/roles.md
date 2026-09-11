# Selecting roles

[Usage](usage.md) · [Project access](projects.md) · [Scripting](scripting.md)

## Choose interactively

Without a project file or selection flags, `pimctl up` opens a picker.
Use `pimctl deactivate` to pick roles to give up.

Type to filter, **Tab** to toggle a role, **Ctrl+A** to toggle all matches,
and **Enter** to confirm. **Esc** clears the filter, then quits.

## Select by key or name

```sh
pimctl ls
pimctl up --key KEY --for 1h
pimctl up --role Reader --scope contoso-dev --for 1h
pimctl down --key KEY
```

Replace `KEY` with the stable key from `ls`.

| Selector | Matches |
|---|---|
| `--key KEY` | One exact role selection; preferred in scripts |
| `--role NAME` | Exact role name when available, otherwise a substring |
| `--scope ID_OR_NAME` | Scope ID or display-name substring |
| `--all` | All eligible roles for activation |
| `--preset NAME` | A saved selection and its contexts |

Keys, roles and scopes are repeatable. Role and scope filters combine with AND.
Name-filter selections above ten roles require `--force`; check the selection
before allowing that breadth. Explicit selectors bypass project-file discovery.

`--for` accepts durations such as `1h`, `90m` and `1h30m`. Each role is capped
at its policy maximum; without `--for`, pimctl requests that maximum.
Interactive activation asks for a justification only when policy requires it.
For unattended flags, see [Scripting](scripting.md).

## Activate at a narrower scope

`--scope` filters eligible roles. `--at` chooses where to activate them:

```sh
pimctl up --key KEY --at /subscriptions/SUBSCRIPTION_UUID/resourceGroups/dev
```

Replace the target with its full ARM resource ID. Azure must confirm that the
eligibility applies there, including when inherited from a management group.
A failed check never falls back to activating at the broader scope.

## Save a personal preset

```sh
pimctl up --role Reader --scope contoso-dev --save-preset daily
pimctl up daily
pimctl down daily
```

A preset remembers the selected roles, contexts and any narrower `--at` target.
Inspect or remove it with `pimctl preset list`, `pimctl preset show daily`,
and `pimctl preset delete daily`.

Ordinary preset entries that are no longer eligible are reported and skipped.
Narrowed targets must still pass eligibility verification before activation.
To share requirements in a repository, use a [project file](projects.md).
