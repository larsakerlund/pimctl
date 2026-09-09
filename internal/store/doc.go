// Package store holds the file primitives every on-disk thing pimctl writes
// shares: where the cache directory is, how a context name becomes a filename,
// how a file is published atomically, and how a family of files is removed.
//
// It exists to be the bottom of the import graph. internal/cache imports
// internal/armclient for the eligibility types it stores, so anything taking a
// directory or an atomic write from there would depend on ARM types it never
// touches — internal/azauth writes a token file and needs none of them. This
// package imports nothing outside the standard library, and nothing here knows
// what a role, a scope or a token is.
//
// The invariant worth keeping: every per-context file goes through [FileName],
// so the four families pimctl writes — token-, eligibilities-, policies- and
// active- — can never disagree about how to spell a context, and the nameless
// ambient `az login` gets exactly one stand-in ([AzLoginName]) rather than one
// per caller. [FileName] always returns a single safe path element, so a
// context name can never steer a write out of the directory it belongs in.
package store
