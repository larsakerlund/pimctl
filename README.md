# pimctl

List, activate, and deactivate your eligible Azure resource roles through
Microsoft Entra Privileged Identity Management (PIM).

Supports batch activation and eligibility inherited through groups. Entra ID
directory roles and PIM for Groups are outside its scope.

## Install

Requires macOS or Linux and the [Azure CLI](https://learn.microsoft.com/cli/azure/).

```sh
curl -fsSL https://raw.githubusercontent.com/larsakerlund/pimctl/main/install.sh | sh
```

The installer downloads the release for your platform, verifies its SHA-256
checksum, and installs it in `~/.local/bin`. Follow its PATH instructions if needed.
You can also download an archive from
[Releases](https://github.com/larsakerlund/pimctl/releases).

## Use

Sign in with `az login`, then:

```sh
pimctl ls                # list eligible roles
pimctl up --for 1h       # pick roles and request up to one hour
pimctl status            # show active roles and expiry times
pimctl down              # deactivate listed active roles, after confirmation
```

Select roles directly for scripts:

```sh
pimctl up --role "Cost Management Contributor" --for 1h -j "Monthly reporting" -y
pimctl status -o json
```

`pimctl up --help` lists selection, duration, and preset options.
The [usage guide](docs/usage.md) covers JSON output, status uncertainty,
authentication, and troubleshooting. Exit code `2` means approval is still pending.

## Multiple tenants

Optional [cloudctx](https://github.com/eliknut/cloudctx) integration requires
version 1.4.0 or newer. pimctl follows `$CLOUDCTX_CONTEXT`; use `-c NAME` to
select a context or `--all-contexts` to work across them.

## Development

Run `make check`. See [CONTRIBUTING.md](CONTRIBUTING.md) for prerequisites,
[docs/design.md](docs/design.md) for architecture, and [SECURITY.md](SECURITY.md)
for credential storage and reporting issues.

MIT licensed. See [LICENSE](LICENSE).
