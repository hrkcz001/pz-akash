# Step 3 report — the state machine, the owner goroutine, and the webhook

**Scope, from the plan's step table:**

> `internal/fsm` transition table + owner goroutine + webhook receiver (HMAC, path/ref/author filter). Akash driver stubbed to dry-run.
>
> **Gate:** `pzctl controller --dry-run` walks a full start/backup/halt cycle from real triggers.

**Status: complete, gate passed.** Nothing was deployed. No lease was created, no money was spent, and the live `pz-saves` remote was never written to — the gate builds its own throwaway bare repo from scratch on every run.

---

## 1. The gate

`scratch/gate.ps1` is the evidence. It is self-seeding: the first thing it does is delete `scratch/gate/` and rebuild the world — a bare `remote.git`, a working clone, `pzctl/config.yaml` committed to `main`. A gate that depended on leftovers from the previous run would prove nothing about a cold start, which is the one state the controller is guaranteed to be in after every redeploy.

The agent is written by hand, because the agent binary is step 4. Its document is force-pushed as an orphan commit onto `refs/heads/state/agent`, which is exactly what `gitbus.PutOrphan` does — so the controller cannot tell the difference.

Run it with:

```powershell
cd pzctl; go build -o ..\scratch\pzctl.exe .\cmd\pzctl
powershell -File ..\scratch\gate.ps1
```

### Transcript

```
=== seed ===
seeded ...\scratch\gate\remote.git from ...\pzctl\config.yaml

=== 0. cold start ===
fsm: start at status=offline intent=stopped lease=none agent=starting
fsm: published 4bb8cdb525af — first sight: publishing the initial document
fsm: one pass done: status=offline intent=stopped lease=none
offline                status=offline    intent=stopped  lease=none       request=none

=== 1. start ===
pushed triggers/start
fsm: consumed trigger(s): start
fsm: offline -> deploying (start trigger)
fsm: published 44d1c5d3dde7 — consumed start; deploying: start trigger
fsm: deploy started
dry-run: created dseq 1787167458 on akash1dryrun01 (attempt 1, restore "")
fsm: deploying -> booting (dseq 1787167458 ready at 203.0.113.2:16261)
fsm: published f4c3d5d91333 — dseq 1787167458; endpoint 203.0.113.2; booting: ...
after start            status=booting    intent=running  lease=1787167458 request=none

=== 2. the agent comes online ===
agent says online
fsm: agent phase starting -> online
dry-run: 1 lease(s) carried over from ...\scratch\gate\provider.json
fsm: booting -> online (agent reports the server is accepting connections)
online                 status=online     intent=running  lease=1787167458 request=none

=== 3. operator backup ===
pushed triggers/backup
fsm: consumed trigger(s): backup
fsm: requesting a operator backup (hlhxedx6nx)
fsm: online -> backing_up (backup operator requested)
requested              status=backing_up intent=running  lease=1787167458 request=hlhxedx6nx/operator
agent says online
fsm: backup hlhxedx6nx done: backup_20260819_212427.zip
fsm: backing_up -> online (backup settled)
backup done            status=online     intent=running  lease=1787167458 restore=backup_20260819_212427.zip

=== 4. halt ===
pushed triggers/halt
fsm: consumed trigger(s): halt
fsm: online -> stopping (halt trigger)
fsm: requesting a halt backup (hlhxej4wu2)
stopping               status=stopping   intent=stopped  lease=1787167458 request=hlhxej4wu2/halt
agent says stopped
fsm: backup hlhxej4wu2 done: backup_20260819_212435.zip
fsm: stopping -> closing (agent parked: stopped)
fsm: close dseq 1787167458 started
dry-run: closed dseq 1787167458
fsm: closing -> offline (closed dseq 1787167458)
after the final backup  status=offline   intent=stopped  lease=none       restore=backup_20260819_212435.zip
closed                  status=offline   intent=stopped  lease=none       (idempotent: nothing published)

=== 5. the index and the leftover triggers ===
backups.json: 2 items, newest first, both with size/sha256/created_at
triggers left on main: (none)

GATE PASSED
```

### What the transcript proves, bug by bug

**Bug 2 (the halt/restart loop) — the halt is ordered and happens once.** `online -> stopping` requests a backup *and then waits*. It does not close the lease until the agent has both answered the request and parked (`agent parked: stopped`). Two things in that sequence are the actual fix:

- The halt does not close on a timer or on "the agent went quiet". It closes on `phase=stopped`, which the agent only writes after it has parked instead of exiting. Under Kubernetes `restartPolicy: Always`, exiting is what produced v1's loop: the container came back, the controller saw a live server during a halt, and flapped.
- The final backup is matched by request ID (`hlhxej4wu2`), not by "an archive appeared". The operator backup taken a minute earlier (`hlhxedx6nx`) cannot sign off the halt.

**Bug 4 (the requested backup sometimes never arrives) — the request is a durable, identified object.** It is published to `state/controller` before the agent is asked to do anything, so a controller that is redeployed mid-backup comes back and finds the same request with the same ID still outstanding. `Step 4` in the transcript shows the halt allocating a *new* ID rather than reusing the operator's, and the gate asserts that (`if ($h.id -eq $r.id) { throw ... }`).

**Bug 2's webhook half — a state-branch delivery reaches nothing.** Not visible in this transcript because the gate drives the controller by polling; it is covered end-to-end by `TestServeHTTPDropsAStateBranchDeliveryEndToEnd`, and at the decision layer by the `controller state branch` / `agent state branch` cases in `TestDecide`.

**Triggers are consumed exactly once.** The final check asserts `main:triggers` is empty. Every trigger the gate pushed was removed by the commit that acted on it, and the SHA of that commit went into the document's dedup ring — so the push we made to consume a trigger cannot come back through the webhook as a new one.

---

## 2. Changes made during step 3, and why

Five changes were made that were not in the step's written scope. Each came out of writing the tests or running the gate; none is cosmetic.

### 2.1 `internal/state` no longer owns a clock (API change)

Every mutator now takes the instant as an argument:

```go
func (c *Controller) SetStatus(to Status, at Stamp) error
func (c *Controller) RequestBackup(id, reason string, at Stamp) *BackupRequest
func (c *Controller) Fail(err error, at Stamp)
func (a *Agent) SetPhase(p Phase, at Stamp)
```

Previously they took a `*time.Location` and called `time.Now()` internally. That is not a testing inconvenience, it is a correctness problem: **every timeout in the controller is the difference between a stamp inside a document and the machine's own notion of now.** A document that stamped itself from the wall clock while the machine measured from an injected clock gives two answers to one question, and no timeout can be tested — or trusted.

The machine now has exactly one source of instants:

```go
func (m *Machine) stamp() state.Stamp {
    return state.At(m.now()).In(m.loc)
}
```

This is pinned by `TestMutatorsUseTheSuppliedInstant`, which passes a stamp from 2020 so a stray `time.Now()` anywhere inside shows up as a wildly wrong value rather than a near miss.

It also fixed two real test failures that had been misdiagnosed as flakes: `TestStaleBackupCannotSignOffHalt` and `TestPeriodicBackupPause` were both comparing a document stamped from the wall clock against a machine measuring from a fake clock.

### 2.2 `needsDeployKey` — a local remote does not need an SSH key

`pzctl controller` sets `requireKey: true`, because the controller writes: consuming a trigger and publishing state both push, and a missing key must fail at startup rather than at the first halt. But that gate also fired for a filesystem-path remote, which is served by the local git binary over a pipe with no authentication in the transport at all.

That is not hypothetical — it made the gate impossible to run, since `--dry-run --repo <path>` against a throwaway clone is the only way to walk the lifecycle without touching the live repository. `needsDeployKey` ([controller.go:42](pzctl/cmd/pzctl/controller.go#L42)) now answers per remote: SSH remotes need the key, paths and `file://` and `http(s)://` do not.

### 2.3 `DryRun.StateFile` — a simulated provider has to outlive the process

**This was the gate's most useful finding**, because it first looked like an FSM bug and was not:

```
fsm: dseq 1787166145 is gone; status failed until a start trigger clears it
fsm: backup trigger ignored: status is failed
```

`DryRun` kept its leases in a map in memory. Every `--once` invocation is a fresh process, so on pass 2 the controller asked a brand-new stub whether the lease it had created on pass 1 was still alive, was told no, and — entirely correctly — declared it vanished.

**A real provider outlives the controller. That is the whole reason `reconcileLease` exists.** The FSM was right and the fake was wrong. `DryRun` now takes an optional `StateFile` and persists `{seq, live}` across processes. Empty keeps it in memory, which is what the unit tests want: a fake that writes to disk between subtests is a fake that leaks between them.

The transcript line `dry-run: 1 lease(s) carried over from ...provider.json` is that fix working.

### 2.4 The first publish is unconditional

Publishing is change-driven — handlers call `m.dirty(reason)` and one commit covers a whole sequence. But a cold start changes nothing, because `offline`/`stopped` is what the zero document already says. The result was that `state/controller` **did not exist at all** until the first trigger arrived, and the first gate run showed it:

```
fatal: invalid object name 'state/controller'.
```

Anything reading that branch — the dashboard in step 7, `pzctl status`, an operator with a browser — would have to treat "no branch" and "no controller" as the same thing. One commit at first sight ([fsm.go:407](pzctl/internal/fsm/fsm.go#L407)) makes the branch's existence something the rest of the system can rely on instead of a side effect of having once been used.

### 2.5 The method-set tripwire caught a real addition

`TestBusMethodSetsArePinned` (written in step 2) failed, reporting an unpinned `Exists` on `ControllerBus`. Step 3 had added it for the periodic-backup pause file.

This is the ownership invariant (I4) doing its job: ownership in this design is *structural* — the controller cannot write `agent.json` because it has no method that does — so any new method on a bus is a change to what that side is capable of, and has to be looked at. `Exists` is a single `ReadFile` that answers `false` for a missing path. Reading either side is unrestricted; only writing is owned. The pin was amended with a comment recording why the addition is safe, rather than loosened.

---

## 3. What is in the tree

```
package    code   test
config     1005    313
fsm        1545    874
gitbus      983    984
pzctl       909    126     (cmd/pzctl)
sdl         251    380
secrets     227     91
state      1647   1474
webhook     304    487
                        161 test functions, all green
```

```
go test ./...
ok  cmd/pzctl              4.817s
ok  internal/config       (cached)
ok  internal/fsm          98.262s
ok  internal/gitbus       (cached)
ok  internal/sdl          (cached)
ok  internal/secrets      (cached)
ok  internal/state        (cached)
ok  internal/webhook      (cached)
gofmt -l .   (clean)
go vet ./... (clean)
```

New this step: `internal/fsm/{fsm,advance,triggers,driver,harness_test,fsm_test}.go`, `internal/webhook/{webhook,decide_test,webhook_test}.go`, `internal/state/request_test.go`, and `cmd/pzctl/controller.go`.

### The webhook's deliberate asymmetry

The two failure directions do not cost the same thing, so they are not treated the same way:

- **A delivery we let through wrongly** can start a funded lease. A bad signature is therefore refused hard: 401, constant-time compare, and the body size limit applied *before* verification so an unauthenticated caller cannot make us allocate in order to be told to go away.
- **A delivery we drop wrongly** costs latency only, because the poll loop asks the same question on its own schedule and reaches the same conclusion. So a correctly signed payload whose *shape* we failed to anticipate is answered `200 reconciling anyway` and dispatched as a synthesised push — and `TestServeHTTPActsOnASignedButUnparseablePayload` additionally asserts that synthesised push survives its own filter chain, which it would not if it named the wrong branch.

`TestVerifyRejectsEverythingElse` covers eight rejection cases (missing header, no algorithm prefix, wrong algorithm, non-hex, truncated digest, wrong secret, tampered body, empty digest). `TestDecide` covers nine routing cases, including a `triggers-old/start` prefix collision that must *not* count as being inside `triggers/`.

---

## 4. One behavioural decision worth your veto

Everything else in this step follows from the invariants you already approved. This one is a judgement call I made, it is easy to reverse, and it changes what happens to your world — so it should be an explicit decision rather than something buried in a function.

**After every successful backup, the controller sets `restore_target` to that backup** ([advance.go:299](pzctl/internal/fsm/advance.go#L299)).

The reasoning: you chose *no persistent storage* ("I'll periodically download backups, and upload them before server start"). So the server's disk does not survive its lease. A boot that does not restore therefore starts a **fresh world** — silently, on top of a perfectly good archive. Making `restore_target` follow the newest backup makes "start again" mean "continue", which is what an operator means by it.

The cost: a boot always restores, so an archive that is itself corrupt gets restored repeatedly rather than being skipped over, and "start me a clean world" now requires an explicit `triggers/restore` with an empty body (which is implemented — it clears the target). The alternative is `restore_target` only ever being set by hand, and the first forgotten one costs you a world.

**No action needed if you agree — this is already the behaviour.** Say the word and I will invert it.

---

## 5. Limits of this gate — stated plainly

- **The Akash driver is a stub.** It creates nothing. Bids, escrow, provider selection, and the endpoint actually becoming routable are all step 5, and none of them are exercised here. The interface it satisfies is deliberately the one the real client will implement, so the lifecycle above is what runs against a real provider too — but that is an argument, not evidence.
- **The agent is a PowerShell function, not the agent.** It writes a well-formed document and answers request IDs correctly because the gate makes it do so. Whether the real agent parks instead of exiting — the actual fix for bug 2 — is step 4's gate.
- **Bug 1 (player count stuck at 0) and bug 3 (the `server_info.json` parse error) are not addressed by this step.** Both live in the agent and the storage server. The document field and the "unstamped means unknown, not zero" handling are in place on the controller side; the thing that populates them is step 4, and the streaming-upload fix for the parse error's neighbourhood is step 6.
- **The `fsm` suite takes 98 seconds**, which is slow enough to discourage running it. It is real git operations against real bare repos, and it is worth it, but I intend to look at whether the harness can share a remote across subtests once the tree stops moving.
- **Nothing is committed.** The whole `pzctl/` tree is still untracked in git, and `scratch/` with it. I have not committed or pushed anything, since neither was asked for and both are outward-facing.

---

## 6. Next

Proceeding to **step 4** — `internal/agent`: boot, config render, pull-restore, PZ supervise, park-on-stop, agent-side backup and upload. Its gate is "runs locally against a stub controller", which needs no deployment and no lease.

**Step 5's gate is blocked and will stay blocked.** It reads "first real deploy on a throwaway dseq", and a throwaway dseq is still a funded lease on the real network. I will implement `internal/akash` and `internal/dns` in full and stop at the gate, flagged, for you to run when you are back. **Step 9 (cutover) is production and will not be executed.**
