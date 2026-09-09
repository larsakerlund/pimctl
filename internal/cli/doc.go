// Package cli is pimctl's command tree and everything between a keystroke and
// an exit code: parsing flags, deciding which roles the user meant, asking ARM
// to activate or release them, and printing what happened.
//
// Everything that is not a network call, a file format or a terminal detail
// lives here. What does not: the ARM REST surface ([armclient]), the access
// token ([azauth]), the eligibility and policy caches ([cache]), presets and
// remembered state ([config]), the type-to-filter picker ([picker]) and the
// terminal predicates ([term]). The one thing on disk that this package owns is
// the activation record, described below, because it is not a cache.
//
// # Commands
//
// Each command is a constructor returning a [github.com/spf13/cobra.Command];
// [NewRootCmd] assembles them. The body of a command that touches a tenant is a
// separate run function, so the cobra plumbing and the work stay legible apart:
//
//	pimctl ls, list        newListCmd        list.go
//	pimctl up, activate    newUpCmd          activate.go  -> runActivate
//	pimctl down            newDownCmd        deactivate.go -> runDeactivate
//	pimctl deactivate      newDeactivateCmd  deactivate.go -> runDeactivate
//	pimctl status          newStatusCmd      status.go    -> runStatus
//	pimctl preset …        newPresetCmd      preset.go
//	pimctl cache clear     newCacheClearCmd  cachecmd.go  -> runCacheClear
//	pimctl cache path      newCachePathCmd   cachecmd.go
//	pimctl logout          newLogoutCmd      cachecmd.go
//	pimctl version         newVersionCmd     root.go
//	pimctl help auth       newAuthHelpTopic  root.go
//
// `up` and `down` are the primary spellings; `activate` and `deactivate` are the
// original names, kept working for scripts, the README and the Claude skill.
// A bare `down` means "give up everything I hold here", which `deactivate` does
// not, so the two are not aliases of one command.
//
// # The pipeline
//
// The verbs that change something share one shape, and each stage has a file:
//
//	prepare      run.go       resolve contexts, open sessions, start the
//	                          activation fan-out in the background
//	gather       eligible.go  the eligible roles, from the cache when it is warm
//	select       select.go, row.go, interactive.go
//	                          --role/--scope/--key, a preset, or the picker
//	plan         plan.go, report.go, confirm.go
//	                          the table shown before anything is sent
//	execute      execute.go, request.go
//	                          one bounded-concurrency runner for both verbs
//	report       report.go, exit.go
//	                          per-role outcomes, then the process exit code
//	finish       run.go       flush timings, name any scope left unconfirmed
//
// # Activation state has two sources, and only one of them is Azure
//
// Reading activation state from ARM costs one request per scope, and ARM's
// tenant-wide listing is lossy, so pimctl fans out per scope and gives each call
// a soft deadline. A scope that misses it is *named* in the output; it is never
// silently dropped, because an incomplete answer presented as a complete one is
// the worst failure this tool can have.
//
// The other source is the activation record under $XDG_STATE_HOME/pimctl:
// record.go writes it, reconcile.go and localrecord.go read it. It exists
// because that fan-out takes seconds and `ls` must print before it finishes.
// Three properties keep it honest, and any change here has to preserve them:
//
//   - It is additive to Azure, never a replacement. It cannot see a role
//     activated in the portal or by a colleague, so its rows are marked "?" and
//     every command that shows activation state still reconciles against ARM.
//   - A row is confirmed against that activation's own schedule request, not
//     against a bare listing that is known to drop rows.
//   - Nothing it holds is discarded without saying so.
//
// It is also not a cache: `pimctl cache clear` leaves it alone, because deleting
// it makes the next `status` under-report roles that are still held.
//
// # Exit codes
//
// exit.go owns the mapping, and it is a contract scripts depend on: 0 when every
// selected role reached a good terminal state, 1 for a failure or a usage error,
// 2 when nothing failed but something is waiting on an approver, and 130 for an
// interrupt. See [ExitCode].
package cli
