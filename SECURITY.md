# Security policy

## Supported versions

pimctl is pre-1.0. Only the latest release receives security fixes, shipped as
the next patch or minor release. Older releases are not patched: upgrade with
`install.sh` or `go install github.com/larsakerlund/pimctl/cmd/pimctl@latest`.

## Reporting a vulnerability

Report privately through GitHub:
<https://github.com/larsakerlund/pimctl/security/advisories/new>. Do not open a
public issue for anything that could let someone read a token, activate or keep
a role they should not have, or run code on the machine pimctl runs on.

If the private form is unavailable to you, open an issue that says only that
you have something to report privately, and a channel will be arranged there.

Include what a reproduction needs: the output of `pimctl version`, the exact
command, its exit code, and `--debug` output if it helps. `--debug` never prints
a token; do not paste one from anywhere else either.

## What to expect

- An acknowledgement within seven days.
- A fix in a release, with the advisory published and credit to the reporter
  unless they prefer otherwise.
- Coordinated disclosure: please allow the fix to ship before publishing
  details. There is no bounty programme.

## Scope

In scope: the `pimctl` binary, `install.sh`, the release workflow and the
files pimctl writes on disk.

Out of scope: Azure PIM, the Azure CLI and cloudctx themselves. Report those to
their owners. Where pimctl keeps its caches and the token, and how they are
protected, is described in `pimctl help auth` and in
[docs/design.md](docs/design.md#token-caching).
