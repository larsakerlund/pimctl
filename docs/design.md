# pimctl — design notes

Why pimctl is built the way it is: what was measured, what ARM actually does,
and which decisions were reversed on evidence. The README is the manual; this is
the lab notebook behind it.

## The activation listing is a fan-out, not one call

ARM's tenant-wide
`roleAssignmentScheduleInstances?$filter=asTarget()` takes 11-21 s here *and*
silently drops rows: three runs against an unchanged tenant returned 126, 131
and 132 instances, with no `nextLink` to explain the difference. pimctl instead
queries each distinct scope you are eligible at — 25 of them for 136 roles — in
parallel, sized to the number of scopes up to a cap of 32, so the usual tenant
runs in a single wave. That is complete for PIM purposes and the slowest
single call is a few seconds. `--all-scopes` restores the single tenant-wide
call if you need it, with the row-dropping caveat above.

Alternatives that do not work, and why: Azure Resource Graph does not carry the
schedule-instance types at all (0 rows), the preview api-versions reject the
resource type, an `assignmentType` filter is invalid, and `asRequestor()` is
forbidden for this principal.

## Why `list` is fast but `status` is not

Measured on a real tenant:
`cloudctx`+`az` startup ~1-2 s, token ~1.5 s, and the eligibility listing ~2 s
for 136 roles. Activation state costs the fan-out above. So pimctl does not wait
for it unless it has to:

* `pimctl up` opens the picker as soon as eligibility arrives. If the activation
  listing lands before you confirm, already-active roles are marked; if not, ARM
  answers `RoleAssignmentExists` and pimctl reports `ALREADY ACTIVE` anyway.
* `pimctl list` never waits for it. The ACTIVE column comes from this machine's
  record, which is free and can be stale for roles activated elsewhere;
  `--with-active` blocks for Azure's answer.

## The propagation window

Azure's per-scope listing runs minutes behind its
own writes. Measured on a real tenant: a PUT accepted at 13:12:02Z was still
missing from the scope's `roleAssignmentScheduleInstances` at 13:13:42 and
appeared at 13:14:06 — and on another day the same lag ran to four and a half
minutes, while the tenant-wide read was current within forty seconds. Believing
that listing alone makes `status` drop a role activated minutes earlier and
announce it as no longer held, and resurrect one just given up.

A fixed grace period was tried first and is not good enough: any constant is
either shorter than some real lag, and reports access as lost while it is held,
or long enough to keep reporting access that is gone. So pimctl asks instead of
waiting. Every activation it performs has a schedule request, and that request
reports `Provisioned` as soon as the role is granted — minutes before the
listing catches up. For each activation the listing has not shown, `status`
reads that request back (one cheap GET) and:

* `Provisioned`/`Granted` — the role is held. The row stays, marked `~`
  (`"state": "confirming"`), until the listing agrees.
* `Revoked`, `Denied`, `Failed`, `Expired`, or the window has passed — the entry
  goes, **and the loss is named on stderr**. Nothing is ever dropped in silence.
* no answer at all — the entry stands until a 30-minute ceiling, the backstop
  for an activation that cannot be checked. Expiring at the ceiling is reported
  the same way.

A deactivation works the same way in reverse: a tombstone keeps the role off the
table while Azure still lists it, and is dropped once the listing agrees it is
gone.


## How it stays fast

The daily loop should feel instant even though Azure is not. Measured on a
25-scope tenant: minting a token is ~1.2 s, the eligibility listing ~0.7 s, a
role's PIM policy ~1.2 s, and reading activation state means one request per
scope — ~2.4 s at full concurrency, and the single slowest thing pimctl does.

So none of it is on the path to first output:

* **The token, the eligible-role listing and each role's policy are cached**
  (5 min before expiry, 10 min, 24 h). A warm command makes no network call
  before it prints.
* **`pimctl status` answers from a record of what this machine activated**, then
  reconciles against Azure in the background and tells you only what changed —
  silence when it agrees, otherwise a line naming what was activated elsewhere.
  Rows it has not yet confirmed are marked `?`. Piped output and `-o json` wait
  for Azure instead, since a reader cannot see a later correction; `--fast` opts
  in there and `--wait` forces the blocking read anywhere.
* **`pimctl ls` never waits** for activation state. The ACTIVE column comes from
  the local record; `--with-active` blocks for Azure's answer.
* **`pimctl up` reaches the picker as soon as eligibility is known**, and streams
  each role's result as it lands rather than printing nothing until the slowest
  finishes.
* **One slow scope cannot stall the answer.** Roughly one read in three has a
  single random scope take 12–15 s. Each gets a 3-second deadline; any that miss
  it are named — `2 scope(s) unconfirmed (slow ARM): contoso-qa, contoso-test` — rather
  than silently omitted, and their last known state is kept. `--wait` raises the
  deadline to 60 s for the authoritative read.
* **Every command that touches the network shows a spinner** on a terminal, so
  nothing is ever a blank screen.

The record is additive to Azure, never a replacement: it cannot see a role
activated in the portal or by a colleague. That is why it is always reconciled
and why unconfirmed rows say so. Each entry keeps the request id it came from —
which is what the propagation check above reads back — and records whether its
start time came from ARM or from this machine's clock (`startSource`), so a
reader can tell a fact from an approximation. `pimctl cache clear` removes all of it, and
`--refresh` bypasses the caches for one run.


## Token caching

The access token is cached per context and reused while at least five minutes
of validity remain — otherwise every command pays 1.23 s for an `az` process
launch, 0.59 s of which is az starting up.

A context's token lives inside that context: `$CLOUDCTX_STORE/pimctl/`, the
directory cloudctx's companion contract sets aside for tools like this one. So
`cloudctx delete <name>` sweeps the credential with the rest of that context's
store, and `--keep-store` keeps it. A token written by an earlier pimctl, under
`$XDG_CACHE_HOME/pimctl`, is moved there the first time the store answers.
Without cloudctx, and for the shared `az login` — which belongs to no context —
the XDG directory is still where it goes.

Before reuse, the cached token must match the tenant and user selected in the
Azure CLI profile. Named contexts read the profile in their own Azure store
and also check any tenant pinned by `cloudctx show`; the shared login reads
`~/.azure/azureProfile.json`. An absent or unfamiliar profile causes a fresh
mint. These checks need no `az` spawn. Concurrent 401 responses share one
refresh, which must preserve the command's tenant and principal.

Role listings, policies, and activation records are separately bound to context,
tenant, and principal, both in their filenames and stored ownership. Switching
accounts cannot reuse the previous account's roles. Older files without this
ownership are ignored; role caches are fetched again from ARM.

`pimctl cache clear` removes the entries outright, in the XDG directory and in
every context's store.

The file is `0600` inside a `0700` directory, written atomically, and **refused
on read** if anything has widened those permissions: a token another account can
read is worse than no cache at all. `az` itself already keeps access and refresh
tokens unencrypted in `~/.azure/msal_token_cache.json` at `0600`, so this is a
second copy of a credential the machine already holds rather than a new class of
exposure. The token is never printed, never logged, never put in an error, and
`--debug` reports only `cache hit` or `miss`.

If ARM rejects a cached token — revoked, or invalidated by a Conditional Access
change before its stated expiry — pimctl drops it and retries once with a fresh
one. A second 401 is a real authorization failure and is reported.

`pimctl cache clear` deletes the cached tokens, role listings and policies —
everything pimctl can re-derive. It leaves the activation record alone, because
deleting that makes the next `status` under-report roles you are still holding
until Azure's listing catches up; `pimctl cache clear --all` deletes it too and
warns. Neither ends your `az` session: run `az logout`, or `cloudctx exec <ctx>
-- az logout`, for that.

## Requiring a cloudctx version

pimctl drives cloudctx from the outside, and 1.4.0 is the release that makes
that a declared arrangement. Its companion contract
([docs/companions.md](https://github.com/eliknut/cloudctx/blob/main/docs/companions.md))
names the surfaces a companion tool may build on and pins them with cloudctx's
own tests: `cloudctx exec` to run a command inside a context,
`cloudctx list --names` for the context names, `cloudctx show` for the tenant a
context is pinned to and the store it owns, `$CLOUDCTX_CONTEXT` and
`$CLOUDCTX_STORE`, the managed variables a bare `az` is spawned without, and the
exact wording of "unknown context". pimctl uses those and nothing else, so what
it does rests on a documented interface with a version number on it rather than
on output shaped for a person to read.

Requiring the version is what makes that hold: one check, rather than probing
each surface and carrying a second way to do everything. A cloudctx older than
`azauth.MinCloudctxVersion` is refused with a single message naming the version
needed and `cloudctx self-update`. Two things sit outside the gate on purpose:
no cloudctx at all, which is a supported configuration, and the shared
`az login`, which never spawns cloudctx.

The version comes from `cloudctx --version`, cached in
`$XDG_CACHE_HOME/pimctl/cloudctx-version.json` against the binary's path, size
and mtime for a day. A cloudctx spawn costs ~0.65 s here — Python startup, the
same order as the token mint the whole cache layer exists to avoid — so probing
on every command would double the cost of a warm one, while an update changes
the binary and invalidates the entry at once.

## Why the shared `az login` is allowed at all

When no context is named, pimctl uses the shared `az login` and prints the
tenant and user it resolved to. That announcement is the point. An earlier
version *refused* to use the shared
login at all, on the grounds that it is scoped to no tenant in particular, and
keeping one tenant's session apart from another's is exactly what cloudctx is
for. That
made pimctl unusable for anyone not running cloudctx, so the refusal was dropped
deliberately — but picking a tenant without saying so would be worse, so pimctl
prints the one it picked. If you work across tenants, cloudctx is still the
better answer: it keeps one Azure CLI store per context.


## One bad context does not stop the rest

With several `-c` flags or `--all-contexts`, a context whose login has expired
(or whose tenant returns an error) is reported as a warning on stderr and
skipped; the contexts that answered are still listed or activated. The run then
exits **1**, so a script cannot mistake a partial answer for a complete one.
Only a run where *no* context could be opened fails outright.


## Decisions that were reversed

Kept here rather than in the code, which describes what pimctl does now:

- **A fixed grace period for the propagation lag.** Three minutes, then thirty.
  Any constant is wrong at one end or the other, and on a day when the listing
  lagged 4.5 minutes it expired mid-lag: `status` printed "No roles are
  currently activated." while the roles were held. Replaced by asking the
  role's own schedule request, which answers immediately. The ceiling survives
  only as a backstop for an activation that cannot be asked about.
- **Refusing the shared `az login` entirely.** See above: it made pimctl
  unusable for anyone not running cloudctx, so the refusal became an
  announcement instead.
- **The tenant-wide activation listing.** It is 11–21 s and drops rows; the
  per-scope fan-out replaced it, and `--all-scopes` keeps it available for
  comparison.
- **Replaying the last justification unasked.** A justification is written into
  a PIM audit log, so it should be a sentence someone chose for this activation.
  The remembered text is now only a prefill for the prompt; when nobody is
  asked, a neutral default goes out.


## Design notes

Three things about the ARM PIM API are easy to get wrong, and pimctl pins all
three with tests against responses shaped like ARM's:

1. **`principalId` is your own object id**, even when the eligibility belongs to
   a group. Sending the group's id fails.
2. **`linkedRoleEligibilityScheduleId` is required** for group-derived
   eligibility and is taken from the eligibility instance verbatim — its scope
   may be *broader* than the scope you are activating at.
3. **`roleDefinitionId` is scope-qualified.** At subscription scope ARM stores
   `/subscriptions/<id>/providers/Microsoft.Authorization/roleDefinitions/<guid>`;
   at management-group scope it stores the bare
   `/providers/Microsoft.Authorization/roleDefinitions/<guid>`. pimctl activates
   at the eligibility's own scope and reuses its id verbatim, re-qualifying only
   if the activation scope ever differs.

The request name is a client-generated GUID, created once per role per run, so a
retry updates the same request instead of creating a duplicate.

Deactivation is the same `PUT` with `requestType: SelfDeactivate` and only
`principalId` and `roleDefinitionId` alongside it — no schedule, no linked
eligibility. Two PIM behaviours around it were found by testing against the live
API rather than the docs: a role must have been active for **five minutes**
before it can be given up (`ActiveDurationTooShort`), and for a few seconds
after activation the assignment has not propagated, so a deactivation in that
window returns `RoleAssignmentDoesNotExist` while the role is still held — which
is why pimctl treats that code as a failure rather than as "already gone". The
reverse lag exists too:
for up to a minute after deactivating, a fresh activation of the same role is
answered with `RoleAssignmentExists` even though `status` already shows nothing.

Azure allows several management groups to share one display name — this tenant
has three called "Contoso landing zones" — so tables append the scope's leaf id
whenever a name would otherwise be ambiguous.

## Troubleshooting in depth

**`MfaRule` / `RoleAssignmentRequestPolicyValidationFailed`.** Every PIM policy
seen in practice enables the MultiFactorAuthentication rule, so the token must
carry an MFA claim (`amr: ["pwd","mfa"]`). A cached or silently refreshed
sign-in may not. Re-authenticate: `cloudctx login <context>`.

**Conditional Access authentication context
(`RoleAssignmentRequestAcrsValidationFailed`, or a 401/403 carrying `claims=`).**
The role's policy requires a stepped-up token. pimctl parses the challenge out
of the response body or the `WWW-Authenticate` header, base64-encodes it if the
service handed it over raw, and prints the exact recovery command:

```sh
cloudctx exec <ctx> -- az login --tenant <tid> \
  --scope "https://management.core.windows.net//.default" \
  --claims-challenge "<base64 claims>"
```

Azure CLI **2.76.0 or newer** is needed for this: older versions return an
opaque *"Resource was disallowed by policy"* instead of the claims challenge.

**`HTTP 429` / throttling.** ARM rate-limits role-management reads per account.
pimctl retries throttled and transient (503/504) responses up to four times,
honouring the `Retry-After` header, before giving up with an explanation.

**`RoleAssignmentExists`.** The role is already active. pimctl reports
`ALREADY ACTIVE` and does not treat it as a failure.

**Ticketing required.** If a role's policy enables the Ticketing rule and you
did not pass `--ticket-number`, that role fails with an explicit message; other
roles in the same batch still proceed.

**`ActiveDurationTooShort` when deactivating.** PIM requires a role to stay
active for at least five minutes before it will accept a `SelfDeactivate`.
pimctl reports this in plain words; wait and retry.

**`FAILED ... not finished propagating` when deactivating.** For a minute or two
after an activation, ARM answers a `SelfDeactivate` with
`RoleAssignmentDoesNotExist` even though the role is still very much held.
pimctl reports this as a **failure**, not as "already gone" — treating it as
success would tell you that you had dropped access you still have. Check
`pimctl status` and retry.

**Ctrl-C during a batch.** pimctl exits 130 and marks each role honestly:
`ABORTED` for one whose request was sent but whose outcome was never observed,
`SKIPPED` for one it never got to. Run `pimctl status` to see what actually
landed — an `ABORTED` role may well be active.

**`ALREADY ACTIVE` right after deactivating.** Deactivation removes the PIM
schedule instance immediately — `pimctl status` goes empty — but the underlying
role assignment takes up to a minute or so to disappear. Re-activating inside
that window returns `RoleAssignmentExists`. Wait a minute and retry.

**`pimctl status` says nothing is active but you know you hold a role.** ARM's
activation listing has been observed dropping genuinely-active roles for around
ten minutes at a time — gone from both `roleAssignmentScheduleInstances` and
`roleAssignmentSchedules`, with no revocation in the request log, and
re-activating afterwards returned the *original* `startDateTime`. It is a
read-side inconsistency in ARM, not a local cache.

Because of this, a **named** deactivation does not trust the listing:
`pimctl down <preset>`, `--role`, `--scope` and `--key` send the
`SelfDeactivate` regardless and report ARM's per-role answer (`DEACTIVATED`, or
`NOT ACTIVE` if ARM really says you do not hold it). Only a bare `pimctl down`
depends on the listing, and it prints this caveat when it finds nothing.

If you suspect you are holding something invisible, name it rather than relying
on `--all`.

**A preset stopped working.** Roles you are no longer eligible for are listed on
stderr as skipped. Re-save the preset with `--save-preset` after re-selecting.
