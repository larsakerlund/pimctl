// Package azauth acquires Azure Resource Manager access tokens and decodes the
// identity claims the PIM APIs need out of them.
//
// A token is minted by shelling out to `cloudctx exec <context> -- az account
// get-access-token --resource https://management.azure.com/`, which is how each
// tenant's credentials stay in their own cloudctx context. An empty context
// name means the shared `az login` instead — the same az argv with the cloudctx
// prefix dropped — and that is the path `--bare-az` selects. [Acquire] runs one
// of those, [AcquireCached] puts the on-disk cache in front of it, and
// [ListContexts] enumerates what cloudctx knows about so a command can fan out
// across contexts.
//
// [Token] pairs the bearer token with the claims read from it, chiefly `oid`:
// the signed-in user's object id, which is the principalId every activation
// request must carry even when the eligibility is inherited through a group.
//
// The cache exists because minting costs a cloudctx/az process launch on every
// single command — 1.23 s median measured, of which 0.59 s is az's own startup — and it holds to four properties:
//
//   - One file per context, $CLOUDCTX_STORE/pimctl/token-<context>.json, inside
//     that context's own store, with the context name sanitised for use in a
//     filename. Without cloudctx — and for the nameless `az login`, which
//     belongs to no context — it is $XDG_CACHE_HOME/pimctl instead. The shared
//     login has its own file, token-_az_login.json, so it can never be confused
//     with a context's.
//   - Mode 0600 inside a 0700 directory, written atomically via a uniquely
//     named temporary file so two runs finishing at once cannot publish a
//     half-written entry.
//   - A wider mode is refused on read, with [ErrCachePermissions]: a token
//     another account can read is worse than no cache at all. Every other
//     defect — absent, corrupt, wrong version, wrong context, wrong tenant, wrong user — is
//     an ordinary miss that mints a fresh token.
//   - An entry stops being used [TokenCacheMargin] before it expires, because a
//     token that dies halfway through a run is worse than one re-minted up
//     front.
//
// The token itself is never printed, never logged and never placed in an error.
// [ExecError] carries az's stderr verbatim but nothing pimctl parsed out of
// stdout, the cache path is derived from the context name alone, and --debug
// reports only whether the cache was hit.
package azauth
