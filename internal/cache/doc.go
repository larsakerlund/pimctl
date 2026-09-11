// Package cache stores eligible roles and PIM policies for an authenticated
// account. Every value has a TTL; a cache miss or failed write never prevents
// the caller from asking ARM directly.
//
// # Files and ownership
//
// Files live under [store.Dir]: $XDG_CACHE_HOME/pimctl, or ~/.cache/pimctl.
// Directories use [store.DirMode] (0700), files use 0600, and [store.WriteAtomic]
// publishes each replacement without exposing a partially written file.
//
//   - eligibilities-<context>-<account>.json holds one account's role listing
//     for [TTL], ten minutes. Scoped variants append a target digest and
//     retain ARM ancestry/membership evidence under the same TTL and clearing.
//   - policies-<context>-<account>.json holds policies keyed by [PolicyKey],
//     each valid for [PolicyTTL], 24 hours. [DropPolicy] removes a rejected
//     policy so the next activation reads its replacement from ARM.
//
// [store.Owner.FileName] combines a sanitised context name with a digest of
// context, tenant, and principal. Each file also stores its owner and checks it
// on read. Missing ownership and older formats cause a miss. An empty context
// means the shared Azure CLI login.
//
// # Related state
//
// internal/azauth owns token files and their account and permission checks.
// internal/cli owns activation records. Named contexts keep both inside their
// cloudctx store; the shared login uses XDG cache and state directories.
//
// `pimctl cache clear` removes tokens, role listings, and policies. Activation
// records survive unless --all is given: they describe recent changes whose
// Azure listings may still lag. Neither cached data nor local records replace
// Azure as the authority on access.
package cache
