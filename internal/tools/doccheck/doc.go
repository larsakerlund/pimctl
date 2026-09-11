// Package main implements doccheck, the documentation gate pimctl runs as part
// of `make check`.
//
// No linter golangci-lint ships requires a doc comment on an *unexported*
// declaration, and none of them requires a file to say what it owns. Both are
// house rules here (see AGENTS.md), so they need their own checker. doccheck
// parses every non-generated Go file in the packages it is pointed at and
// reports four things:
//
//   - a file with no header comment above its `package` clause;
//   - a top-level declaration with no doc comment, exported or not;
//   - a doc comment that does not start with the name it documents;
//   - a package with more than one package comment.
//
// Test files are held to the header rule only: a table test's shape is its own
// documentation, and demanding a sentence above every `func TestX` produces
// noise rather than description.
//
// The checker is deliberately syntactic. It reports a missing or misnamed
// comment, never a wrong or useless one — no tool can judge that, and pretending
// otherwise would invite comments written to satisfy the gate. Reviewers still
// have to read the prose.
//
// Usage:
//
//	doccheck [packages...]
//
// Arguments are directories, optionally with the `./...` suffix, and default to
// `./...` from the working directory. Findings go to stderr, one per line in
// `file:line: message` form, and any finding exits 1.
package main
