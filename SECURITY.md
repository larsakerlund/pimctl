# Security

## What pimctl holds, and where

pimctl does not log in. It asks the Azure CLI for an ARM access token and talks
to `management.azure.com` directly, so the credential it handles is a bearer
token minted by `az` and valid for about an hour.

| Path | Contents | Mode |
|---|---|---|
| `$CLOUDCTX_STORE/pimctl/token-<context>.json` | one ARM access token per context | `0600` in a `0700` directory |
| `$CLOUDCTX_STORE/pimctl/active-<context>-<account>.json` | what this machine activated | `0600` |
| `$XDG_CACHE_HOME/pimctl/eligibilities-<context>-<account>.json` | role names and scope ids | `0600` |
| `$XDG_CACHE_HOME/pimctl/policies-<context>-<account>.json` | PIM policy per role and scope | `0600` |
| `$XDG_CONFIG_HOME/pimctl/{presets,state}.json` | saved selections, last justification | `0600` |

The first two belong to one cloudctx context, so they live inside that context's
own store and `cloudctx delete <name>` sweeps them. Without cloudctx, and for
the shared `az login`, which belongs to no context, they fall back to
`$XDG_CACHE_HOME/pimctl` and `$XDG_STATE_HOME/pimctl`.

`<account>` is a digest of the context, tenant id, and principal id. Each role
cache and activation record also stores that ownership and checks it on read.
Older files without account ownership are ignored; `cache clear --all` can
remove them. Switching accounts preserves each account's separate state.

Token reuse checks the selected Azure CLI profile's tenant and user against the
cached token. An absent or unrecognisable account causes a fresh token mint.
A refresh during a command must preserve its tenant and principal.

Only the first is a credential. The token cache is **refused on read** if its
mode has been widened, is versioned and carries the context it was minted for,
and is written atomically with the mode set before the first byte — so it never
exists on disk world-readable, not even for an instant.

`az` already stores access and refresh tokens unencrypted in
`~/.azure/msal_token_cache.json` at `0600`. pimctl's cache is a second copy of a
credential the machine already holds, not a new class of exposure. It exists
because minting one costs an `az` process launch (1.23 s measured) on every
command.

Tokens are never logged. `--debug` reports only `cache hit` or `cache miss`, and
every URL in an error has its query string stripped before it is printed.

Authenticated requests, pagination links and redirects are constrained to the
configured ARM origin: scheme, hostname and effective port must match. A link
to another host or an HTTP downgrade is rejected before sending the token.

## Reducing what is kept

- `pimctl cache clear` deletes the tokens, listings and policies. It leaves the
  activation record alone, because deleting that makes the next `status`
  under-report roles you still hold; `pimctl cache clear --all` deletes it too.
- Deactivate when you are done: `pimctl down`. A time-boxed role you are not
  using is still a role someone could use.
- On a shared or multi-user machine, set `XDG_CACHE_HOME` somewhere only you can
  read. The `0700` directory is the floor, not a guarantee about the filesystem
  underneath it.

## Reporting something

This is a personal tool with one maintainer. Open an issue for anything that is
not itself sensitive; for anything that is, mail the address in the commit log
and give it a few days before disclosing.
