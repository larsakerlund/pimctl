// Package cache holds the copies of Azure's answers that pimctl can re-derive:
// the eligible-role listing per context, and each role's PIM policy. Nothing
// here is authoritative — every value carries a TTL, a miss is never an error,
// a write that fails is silent, and `pimctl cache clear` removes the lot.
//
// # What it keeps, and for how long
//
// Both files live in the directory [store.Dir] returns: $XDG_CACHE_HOME/pimctl,
// or ~/.cache/pimctl when that variable is unset. Directories are created with
// [store.DirMode], 0700, so role names and scope ids stay off a shared machine.
//
//   - eligibilities-<context>.json — one context's eligible-role listing,
//     usable for [TTL], ten minutes. Eligibility changes on the timescale of an
//     access review, not a working session.
//   - policies-<context>.json — every (scope, role) policy that context has
//     read, keyed by [PolicyKey], each usable for [PolicyTTL], 24 hours. A
//     policy read costs two sequential ARM GETs, measured at 1.18s per role,
//     and a stale one cannot cause a wrong activation — only a rejected one
//     that ARM explains, after which [DropPolicy] removes the entry.
//
// The <context> part is the cloudctx context name put through [store.FileName],
// which sanitises it — every run of characters outside [A-Za-z0-9_.-] becomes a
// single underscore — and substitutes [AzLoginName] when the result is empty,
// which is what the ambient `az login` sanitises to, having no name of its own.
// One helper does it for every file pimctl writes, so no two of them can decide
// to spell the nameless context differently.
//
// Every file is published by [store.WriteAtomic]: written to a uniquely-named
// temporary file beside its destination and renamed over it, so a concurrent
// pimctl can never read a half-written cache.
//
// # What it does not keep
//
// Two other files pimctl writes are deliberately not this package's. The ARM
// access token shares this directory as token-<context>.json but belongs to
// internal/azauth, which owns the refusal to read one whose mode has been
// widened. Nothing is shared between the two packages directly: the directory,
// the naming rule and the atomic write they both use live in internal/store,
// which imports nothing beyond the standard library. That is the point of it —
// internal/cache imports internal/armclient for the shapes it stores, and
// internal/azauth has no business depending on ARM types to write a file.
//
// The record of the activations this machine performed belongs to internal/cli
// and lives under $XDG_STATE_HOME, not here, because it is not a cache: it is a
// note of something that happened rather than a copy of something re-derivable.
// It is additive to Azure and never a replacement — it cannot see a role
// activated in the portal or by a colleague — and deleting it makes the next
// `status` under-report roles that are still held. `pimctl cache clear`
// therefore clears the caches and leaves the record alone; only `cache clear
// --all` deletes it, and it says what that costs.
//
// The same asymmetry is the reason activation state is not cached here at all.
// `status` has to tell the truth about what is held right now, and a fast
// answer that can be silently wrong is worse than a slow one.
package cache
