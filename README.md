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
pimctl ls           # list eligible roles
pimctl up --for 1h  # activate roles
pimctl status       # show active roles
pimctl down         # deactivate roles, after confirmation
```

See the [usage guide](docs/usage.md) for more options and examples.

## Agent skill

Install [instructions for your coding agent](skills/pimctl/SKILL.md):

```sh
npx skills add larsakerlund/pimctl --skill pimctl
```

[Contributing](CONTRIBUTING.md) · [Security](SECURITY.md) · [MIT license](LICENSE)
