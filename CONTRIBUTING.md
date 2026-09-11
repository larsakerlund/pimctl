# Contributing

Thanks for looking. pimctl is small and opinionated, and two of its opinions
will fail your build before a human reads the change, so they are first.

## `make check` is the gate

```sh
make check
```

That is the whole contract: formatters in diff mode, golangci-lint, shellcheck
over `install.sh`, `go vet`, the documentation checker, govulncheck, the
race-enabled tests — twice, the second time with cloudctx stripped from `PATH`,
because pimctl has to work without it — and the expect(1) picker tests. It needs
the Go toolchain, [golangci-lint](https://golangci-lint.run) at the version
pinned in `.golangci.yml`, `shellcheck`, `expect`, and network access for
govulncheck.

`make test-install` is separate because it downloads a release; CI runs it on
ubuntu and macOS.

## The documentation checker will fail you first

`make doccheck` is a small `go/ast` checker in `internal/tools/doccheck`, and it
is stricter than anything golangci-lint ships. It fails a change when:

- a `.go` file — tests included — has no header comment saying what it owns;
- any top-level declaration, **exported or unexported**, has no doc comment
  starting with its own name;
- a doc comment says nothing beyond the identifier ("Timeout is the timeout.");
- a struct field or interface method in a non-test file has no comment;
- a package is documented twice.

New code carries its comments in the same commit or the build stops. This is
the part first-time contributors hit blind, so: write the comments as you go,
and run `make doccheck` before you push.

## Comments describe the code as it is

Explain **why**, anchored in a fact — a measured latency, an ARM behaviour, the
failure the code exists to prevent. If you cannot say why, say precisely what
and stop; an invented rationale is worse than none.

And keep the chronology out. "A fixed grace period cannot work, because any
constant is either shorter than some real lag or longer than the access" belongs
in the code. "A three-minute grace was tried first and expired mid-lag" is the
same fact wearing a commit message: it goes in `git log`, or in the "Decisions
that were reversed" section of [docs/design.md](docs/design.md). The test is
whether a reader who has never seen the old version needs the sentence.

`.golangci.yml` enables every linter golangci-lint ships and disables only what
carries a one-line reason in the file. Fix findings in the source rather than
loosening the config; when a rule genuinely blocks you, `//nolint:<linter> //
reason` on that line, with the linter named and the reason given.

## Commits

Small, and one idea each. The subject line is a sentence in the imperative —
"Install without a token", not "fix(install): remove token" — and the body says
what changed and why it had to, in prose. Attribution lines and tool footers are
not used.

If a change alters behaviour, it comes with a test that fails without it, and a
`CHANGELOG.md` entry under `## [Unreleased]`.
Entries describe what changes for users; omit documentation updates, refactoring
and other maintenance that leaves behaviour unchanged.

## Reporting

Bugs and questions: open an issue. Anything sensitive:
[SECURITY.md](SECURITY.md).

`AGENTS.md` is the long version of all of this — conventions, ARM facts that are
easy to get wrong, and a map of the layout. It is written for coding agents, and
it is the best thing to read before a first change.
