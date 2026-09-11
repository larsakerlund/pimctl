# pimctl

Manage your eligible Azure resource roles through Microsoft Entra PIM.

## Install

Requires macOS or Linux and the [Azure CLI](https://learn.microsoft.com/cli/azure/).

```sh
curl -fsSL https://raw.githubusercontent.com/larsakerlund/pimctl/main/install.sh | sh
```

## Use

Sign in with `az login`, then:

```sh
pimctl ls            # list eligible roles
pimctl up --for 1h   # pick roles to activate
pimctl status        # show active roles
pimctl down          # deactivate listed roles, after confirmation
```

For a project, run `pimctl init` to choose its scopes and roles, then commit the
generated `.pimctl.yaml`. Teammates can run `pimctl up` with their own login.
Use `pimctl status --project` or `pimctl down --project` for that file’s exact
roles; bare `status` and `down` keep their usual meaning.

See the [usage guide](docs/usage.md) for project access, presets, scripting, multiple tenants,
and troubleshooting.

## Agent skill

Install [instructions for your coding agent](skills/pimctl/SKILL.md):

```sh
npx skills add larsakerlund/pimctl --skill pimctl
```

[Contributing](CONTRIBUTING.md) · [Security](SECURITY.md) · [MIT license](LICENSE)
