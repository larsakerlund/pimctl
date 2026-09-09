# pimctl — notes for agents

`pimctl` lists and batch-activates the Azure PIM **resource** roles the signed-in
user is eligible for (management group / subscription / resource group /
resource RBAC). Entra directory roles and PIM for Groups are out of scope: an
az-issued ARM token cannot do them.

Azure calls go through cloudctx for per-tenant credential isolation:
`cloudctx exec <context> -- az ...`. When no context is named pimctl uses the
shared `az login` and prints the tenant and user it resolved to; `--bare-az`
forces that path.

cloudctx is optional, but when present it must be **>= the version in
`azauth.MinCloudctxVersion`** (1.4.0, the release that declared
`docs/companions.md`): pimctl drives `cloudctx exec`, `cloudctx list --names`,
the `key = value` and `store:` lines of `cloudctx show`, `$CLOUDCTX_CONTEXT`,
`$CLOUDCTX_STORE`, the seven managed variables and the exact "unknown context"
phrase — everything that page declares, and nothing it does not. An older
cloudctx is refused with one message naming the version needed; the shared
`az login` path is unaffected. The version is probed once per binary per day
(`cloudctx --version`) and cached, because a cloudctx spawn costs ~0.65 s.

## What is on disk, and why

Four things, none of them authoritative over Azure:

| Where | What | TTL |
|---|---|---|
| `$CLOUDCTX_STORE/pimctl/token-*.json` | ARM access token per context, `0600`, refused if wider | until 5 min before expiry |
| `$CLOUDCTX_STORE/pimctl/active-*.json` | activations this machine performed | until each window ends |
| `$XDG_CACHE_HOME/pimctl/eligibilities-*.json` | eligible-role listing per context | 10 min |
| `$XDG_CACHE_HOME/pimctl/policies-*.json` | each role's PIM policy per (scope, role) | 24 h |
| `$XDG_CACHE_HOME/pimctl/cloudctx-version.json` | the probed cloudctx version, keyed by the binary | 24 h |

The first two belong to one context, so they live inside that context's cloudctx
store and `cloudctx delete` sweeps them; a file written by an earlier pimctl
is moved there on first use. Without cloudctx — or for the shared `az login`,
which belongs to no context — both fall back to `$XDG_CACHE_HOME/pimctl` and
`$XDG_STATE_HOME/pimctl`, which is where they used to live.

The activation record is the one with a correctness edge. It exists because
reading activation state costs a per-scope fan-out (~2.4 s), and it is
**additive to Azure, never a replacement**: it cannot see a role activated in
the portal or by a colleague, so rows from it are marked `?` and every command
that shows activation state still reconciles against Azure behind the scenes.
If you touch it, keep that property — a fast answer that can be silently wrong
is worse than a slow one. It is also not a cache: `pimctl cache clear` clears
the three cache files above and leaves the record alone, because deleting it
makes the next `status` under-report roles that are still held. `cache clear
--all` deletes it and says what that costs.

## Commands

```sh
make check      # MUST be green before you call anything done
make lint       # golangci-lint run ./... + shellcheck over install.sh and scripts/*.sh
make fmt        # gofumpt + goimports + golines, via golangci-lint fmt
make doccheck   # file headers + a doc comment on every top-level declaration
make vuln       # govulncheck ./... — reachable vulnerabilities only
make tui-test   # expect(1) picker tests; needs `expect` installed
make test-nocloudctx  # the race suite with cloudctx stripped from PATH
make test-install  # install.sh against the real latest release; downloads from GitHub
```

## Performance rules of thumb

Measured on a 25-scope tenant, so do not re-derive these by guessing:

- The per-scope activation fan-out is the expensive call (~1.6 s p50 each,
  ~2.4 s wall at full concurrency). Never put it on the path to first output.
- Token mint is ~1.2 s, eligibility ~0.7 s, a policy read ~1.2 s per role.
  All three are cached; a warm command should do no network before printing.
- HTTP/2 and connection reuse are already optimal, and process startup is
  0.02 s. There is nothing to win there — measure before optimising.
- `TestListFromCacheProducesOutputBeforeAnyNetworkCall` blocks every request
  and asserts output still appears. If you add a network call to the warm path
  it hangs, which is the point.
- One scope read in three is a 12-15s outlier, and which one is random. Each
  per-scope call has a 3s soft deadline; scopes that miss it are *named*, never
  silently dropped, and the record keeps their last known state. Preserve that:
  an incomplete answer presented as a complete one is the worst failure this
  tool can have.

## UI rules

- Every command that touches the network shows a spinner on a TTY. A blank
  terminal is a bug.
- The picker and the justification prompt draw inline and erase themselves;
  scrollback keeps a one-line summary, not a dead frame.
- No confirmation in the interactive flow — picking and justifying are already
  two confirmations. Unattended runs confirm only for `--all` or >10 roles.
- The justification prompt appears only when a selected role's policy has the
  Justification rule.

`make check` = fmt-check + lint + vet + doccheck + vuln + `go test -race` + test-nocloudctx + tui-test. Do not report a change as finished
on anything less. `make test-install` is deliberately outside it: it downloads a
release from GitHub and needs a token, so it runs in CI on both platforms rather
than on every local save.

cloudctx is optional, and `test-nocloudctx` is what keeps it that way: a test
that asks the host whether cloudctx exists passes on a developer's machine and
fails everywhere else, so the presence of cloudctx is pinned per test package
and this target is the machine that does not have it.

`make vuln` runs govulncheck, which reports only advisories pimctl's own call
graph reaches — so a finding there is real. Most will be standard-library ones:
the fix is to raise `toolchain` in go.mod, not to suppress the finding.

Never run `make install`. It writes to `~/.local/bin` and the zsh completion
directory; that is the owner's decision, not yours. `install.sh` installs to the
same place, so run it only with `PIMCTL_INSTALL_DIR` pointing somewhere
temporary — which is what `make test-install` does.

## Lint policy

`.golangci.yml` is strict on purpose: `default: all`, with every disabled linter
carrying a one-line reason in the file. It is pinned to the golangci-lint
version named at the top of that file, matching `.github/workflows/ci.yml`.

- Fix findings in the source. Do not loosen the config to make one go away —
  that needs the owner's approval.
- `//nolint` is allowed only with the linter named and a reason on the same
  line: `//nolint:errcheck // best-effort cleanup, the caller has the real
  error`. `nolintlint` rejects anything less, including a nolint that no longer
  suppresses anything.
- Behaviour is fixed. A lint-driven refactor must keep the same CLI flags, the
  same error wording and the same exit codes. If a finding can only be fixed by
  changing behaviour, leave it and tell the owner.

## Documentation standard

This repo is meant to be readable by an engineer who has never seen it. `make
check` enforces the mechanical half of that; the rest is judgement.

- One `doc.go` per package and nothing else in it: the `Package x …` comment,
  saying what the package is for, its main types, how the rest of pimctl uses
  it, and its invariants. Each `cmd/*/main.go` carries its own.
- Every `.go` file, tests included, opens with a one-to-five-line comment saying
  what it owns **and what it deliberately does not**. Leave a blank line before
  `package`, or the comment becomes a second package comment and `revive` fails.
- Every top-level declaration, **exported and unexported**, has a doc comment
  starting with its own name. Say what it does, the meaning and units of its
  arguments, what it returns, which errors it returns and when, and any side
  effect that matters: a file write, a network call, blocking, concurrency.
  Never restate the signature. Test files need only the header.
- A doc comment above a parenthesised `const`/`var`/`type` block explains the
  set; it does not stand in for the members. Each member still carries its own
  line, and a trailing `// …` is enough for one.
- **Every struct field and interface method carries a comment**, doc or
  trailing, in non-test files — nested anonymous structs included, since a wire
  format's inner fields are where its meaning hides. "Not self-evident" is the
  rule a human applies; a checker cannot, so the mechanical rule is stricter
  than the judgement it stands in for.
- A doc comment has to say something beyond its own identifier — "Timeout is
  the timeout." fails — and ends in a full stop (`godot`, scope `all`; an
  indented code block or list is exempt from the full stop).
- Explain **why**, anchored in a fact: a measured latency, an ARM behaviour, the
  failure the code exists to prevent. If you cannot say why, say precisely what
  and stop. An invented rationale is worse than none.
- **Comments describe the code as it is, not as it was.** "A fixed grace period
  cannot work, because any constant is either shorter than some real lag or
  longer than the access" belongs in the code; "a three-minute grace was tried
  first and expired mid-lag" is the same fact wearing a commit message, and
  belongs in `git log` or `docs/design.md`. The distinction is the tense, and
  the test is whether a reader who has never seen the old version needs the
  sentence. Keep the fact, drop the chronology; the history section of
  docs/design.md is where a reversal worth remembering goes.

`make doccheck` runs `internal/tools/doccheck`, a small `go/ast` checker that
fails on a file with no header, a top-level declaration with no doc comment, a
doc comment that does not start with its identifier or that says nothing beyond
it, an undocumented struct field or interface method, or a package documented
twice. A `//nolint` or `//go:generate` line does not count as documentation: it
is a directive, and `CommentGroup.Text` drops it, which is exactly the question
the checker asks. It is in `make check` and in CI, so a new declaration carries its comment
in the same change or the build stops.

## Tenant safety for anything that touches a real tenant

E2E work against a live tenant is tightly bounded. Within one session:

- Only the context, roles and scopes listed in `CLAUDE.local.md` (untracked). No
  such file means no envelope, and no envelope means no live tenant: ask.
- Duration ≤ 1 hour, and leave **zero** roles activated: deactivate everything
  you activated before you finish.
- Never a bare `az`. Never print, log or echo a token, and never write one to a
  file.
- Anything outside that envelope — another context, another role, another scope,
  a longer window — is the owner's call, not yours.

## Exit-code contract

| Code | Meaning |
|---|---|
| 0 | every selected role reached a good terminal state (or was already active) |
| 1 | at least one role failed or never reached a final status in time; also every usage and setup error |
| 2 | nothing failed, but at least one role is waiting on an approver — the change has not taken effect (after `up` the role is not active; after `down` it is still active) |
| 130 | interrupted (SIGINT/SIGTERM); requests already sent may be in flight |

Exit 2 exists so a script does not read "queued for approval" as success. A
request that stalls below a terminal status is a plain failure (1), because no
approver will resolve it.

## ARM facts that are easy to get wrong

These are pinned by tests against responses shaped like ARM's. Do not "simplify"
them.

- **api-version is `2020-10-01`** for the role schedule request, instance and
  policy endpoints.
- **`principalId` is the signed-in user's own object id** (the `oid` claim of
  the ARM token), even when the eligibility is inherited through a group.
- **`roleDefinitionId` must be re-qualified to the activation scope.** The
  eligibility carries the definition id under the scope it was granted at; the
  request needs it under the scope being activated.
- **`linkedRoleEligibilityScheduleId` goes through verbatim**, exactly as the
  eligibility reported it. Rewriting it makes ARM reject the request.
- **The tenant-wide active listing drops rows.** ARM's
  `roleAssignmentScheduleInstances?$filter=asTarget()` is both slow (12-20s here)
  and lossy — three runs against an unchanged tenant returned 126, 131 and 132
  instances with no nextLink. pimctl therefore fans out per scope by default.
  `--all-scopes` keeps the old single call for comparison. Do not make the
  tenant-wide call authoritative again.
- A named deactivation does not trust the listing either: it asks ARM about the
  named roles and reports ARM's answer per role, because a dropped row once made
  `deactivate --all` exit 0 while the roles were still held.

## Layout

Every binary lives under `cmd/`, the layout a Go reader expects:

- `cmd/pimctl` — the shipped binary: signal handling and the exit code, with the
  body in `run()` so the handler is torn down before `os.Exit`.
- `cmd/tuiprobe` — the pty test probe, behind the `tuiprobe` build tag. Nothing
  behind that tag ships in `pimctl`.
- `internal/cli` — cobra commands, the plan/execute pipeline, the activation
  record. One file per subject; every `*_test.go` names the file it covers,
  except `fake_test.go` and `testhelpers_test.go`, which are the test rig.
- `internal/armclient` — the ARM REST calls, retries and error classification.
- `internal/azauth` — token acquisition through cloudctx.
- `internal/cache` — the eligibility and policy caches. Not the activation
  record: that is not a cache, and lives in `internal/cli`.
- `internal/store` — the file primitives every on-disk thing shares: the
  directory, the context-name-to-filename rule, the atomic write and the bulk
  remove. Imports nothing outside the standard library, which is the point:
  `internal/azauth` writes a token file without depending on ARM types.
- `internal/config` — presets and remembered state under `$XDG_CONFIG_HOME`.
- `internal/picker` — the type-to-filter multi-select.
- `internal/term` — is this a terminal, the colour it may use, the spinner.
- `internal/tools/doccheck` — the documentation gate `make doccheck` runs. Not
  part of the pimctl binary.
- `scripts/*.exp` — the expect(1) scripts driving `cmd/tuiprobe` through a pty.
