# Setup and accounts

[Usage](usage.md) · [Troubleshooting](troubleshooting.md)

## Install

Requires macOS or Linux and [Azure CLI](https://learn.microsoft.com/en-us/cli/azure/install-azure-cli).

```sh
curl -fsSL https://raw.githubusercontent.com/larsakerlund/pimctl/main/install.sh | sh
```

The installer verifies the release's SHA-256 checksum and installs to
`~/.local/bin`. Follow its PATH instructions if needed. Download the script and
run `sh install.sh -h` for options, or get an archive from
[Releases](https://github.com/larsakerlund/pimctl/releases).

To install from source:

```sh
go install github.com/larsakerlund/pimctl/cmd/pimctl@latest
```

## Choose a login

pimctl uses an existing Azure CLI login; it does not sign you in.

```sh
az login
pimctl ls               # use the shared login; prints tenant and user
pimctl ls -c work       # use a named cloudctx login
```

[cloudctx](https://github.com/eliknut/cloudctx) is optional and must be version
1.4.0 or newer when used. Context names are case-sensitive.

| Selection | Login used |
|---|---|
| `--bare-az` | Shared Azure CLI login |
| `-c CONTEXT` (repeatable) | Named cloudctx contexts |
| `--all-contexts` | All cloudctx contexts |
| A preset | The contexts saved in it |
| No explicit selection | `$CLOUDCTX_CONTEXT`, otherwise the shared login |

Inside a cloudctx shell, `--bare-az` is refused unless you explicitly use
`--bare-az=force`. For precedence and conflicting options, run `pimctl help auth`.
Project files specify a tenant, while each teammate chooses their own login.

## Install the agent skill

The [pimctl skill](../skills/pimctl/SKILL.md) helps coding agents select accounts,
change roles and interpret results:

```sh
npx skills add larsakerlund/pimctl --skill pimctl
```

This installs instructions; install the binary separately. From a local clone,
use `npx skills add ./ --skill pimctl`.

## Clear data or uninstall

`pimctl cache clear` removes cached tokens, listings and policies.
`pimctl cache clear --all` also forgets activations recorded on this machine;
it **does not deactivate roles** and can make the next status under-report them.
See [stored data](../SECURITY.md#what-pimctl-holds-and-where) for locations.

To uninstall, remove `~/.local/bin/pimctl` and any shell completions you installed.
Presets and remembered justification remain in `$XDG_CONFIG_HOME/pimctl`
(normally `~/.config/pimctl`); remove that directory only if you want to discard them.
