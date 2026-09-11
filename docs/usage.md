# Usage

Sign in with `az login`, then:

```sh
pimctl ls           # list eligible roles
pimctl up --for 1h  # choose and activate roles for up to an hour
pimctl status       # see active roles and their remaining time
pimctl down         # deactivate all active roles, after confirmation
```

In a project with `.pimctl.yaml`, `up` activates the file's required roles.
Use `up --no-project` to open the picker instead.
**Bare `down` and `status` keep the same meaning in every directory.**
Use `down --project` or `status --project` to select only the file's roles.

## Find your task

| I want to… | Guide |
|---|---|
| Choose roles, limit their scope, or save a personal preset | [Selecting roles](roles.md) |
| Share required access with a project team | [Project access](projects.md) |
| Run unattended or read JSON and exit codes | [Scripting](scripting.md) |
| Install pimctl or choose an Azure/cloudctx login | [Setup and accounts](setup.md) |
| Resolve a failed request or uncertain status | [Troubleshooting](troubleshooting.md) |

For all flags, use `pimctl COMMAND --help`. `activate` and `list` are aliases
for `up` and `ls`; `deactivate` opens a picker instead of `down`'s all-active
selection.
