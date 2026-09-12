// Package main implements doccheck, the documentation gate pimctl runs as part
// of `make check`.
//
// No linter golangci-lint ships requires a doc comment on an *unexported*
// declaration, and none of them requires a file to say what it owns. Both are
// house rules here (see AGENTS.md), so they need their own checker. doccheck
// parses every non-generated Go file in the packages it is pointed at and
// reports five things:
//
//   - a file with no header comment above its `package` clause;
//   - a top-level declaration with no doc comment, exported or not;
//   - a doc comment that does not start with the name it documents, says
//     nothing beyond that name, or does not end in a full stop;
//   - a struct field or interface method with no comment, in non-test files;
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
//	doccheck [-h] [packages...]
//
// Packages are directories, with or without a trailing `/...` for "and
// everything below", and default to `./...` from the working directory. `-h`
// prints the same rules and exit statuses and exits 0. Findings go to stderr,
// one per line in `file:line:col: message` form, and a one-line summary goes
// to stdout.
//
// The exit status is 0 when every file examined is documented, 1 on at least
// one finding or a package that cannot be walked or parsed, and 2 for a flag
// doccheck does not know. The last is distinct so a Makefile can tell "the
// code is undocumented" from "the command line was wrong". The usage const in
// main.go is the authoritative copy of this text; the two must agree.
package main
