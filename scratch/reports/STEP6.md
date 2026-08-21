# Step 6 report — `internal/httpapi`: auth, streaming upload, download, the index, retention

**Scope, from the plan's step table:**

> `internal/httpapi`: auth, streaming upload (fix the read-whole-body-into-RAM defect), download, `backups.json` index, retention
>
> **Gate:** Manual download/upload round-trip

**Status: complete.** The gate (`scratch/gate6.ps1`) runs a real `pzctl controller` process against a real bare git remote, drives 19 assertions over `curl.exe`, and ends by reading `state/controller:backups.json` off the state branch. It passes. The full Go suite is green with `-race` on Debian, `go vet` clean, `gofmt` clean.

The headline number: **a 256 MiB upload moved the controller's peak RSS by 0 MiB** — 24 MiB before, 24 MiB after. That single line is what this step existed to produce.

---

## 1. What this package replaces

v1's `storage_server.py`: a `BaseHTTPRequestHandler` that served `server.zip` to the agent, accepted the halt backup, and kept `backup_log.txt` and `server_info.json` alongside the archives.

Three of the four reported bugs pass through it, and the third one *is* it:

| v1 bug | Where it lived |
|---|---|
| 3 — `Error reading server_info.json: Expecting value: line 1 column 106` | `storage_server.py` wrote that file; a reader raced a partial write |
| 4 — the requested backup sometimes never arrives | the upload path, and the log that recorded it |
| — (unreported, found by reading) | `body_bytes = self.rfile.read(content_length)` |

v2 keeps the two jobs — serve the packages, hold the backups — and gives up the third: **there is no `server_info.json` and no `backup_log.txt`.** The state a reader wants is on the state branch, written by one writer, and the index of what the directory holds is generated from the directory. Bug 3 is not fixed so much as deleted: nothing writes a JSON file into `backups.dir` any more, so nothing can read a half-written one.

## 2. The RAM defect

```python
# v1, storage_server.py
content_length = int(self.headers['Content-Length'])
body_bytes = self.rfile.read(content_length)
```

`controller.resources.memory` is **2Gi**. `backups.upload_max_bytes` is **2Gi**. A halt backup of a grown world is the largest request the system ever makes, it arrives at the worst moment — the lease is closing — and v1 allocated the whole thing before touching the disk. The container is OOM-killed, the upload is lost, and the world is on a volume that is about to be deleted.

v2's upload path never holds more than 32 KiB of the body:

1. `Content-Length` is checked against `upload_max_bytes` and against free space (`min_free_bytes`) **before the first byte is read**, so an upload that cannot possibly fit is refused at the header rather than after twenty minutes of transfer.
2. The body streams through `io.Copy(io.MultiWriter(tmp, sha256), io.LimitReader(body, max+1))` — a 32 KiB buffer, into a temp file named `.<name>.part<n>` **in the destination directory**, so the final step is a rename within one filesystem and never a copy. The `LimitReader` is not redundant with the header check: a chunked request declares no length and a `Content-Length` is a header the sender controls, so without a bound on the copy itself `upload_max_bytes` would be advice.
3. The digest is compared to the client's `X-PZ-SHA256`. A mismatch removes the temp file and answers **422**; the directory is untouched.
4. Only then, `os.Rename`. There is no window in which `backups.dir` contains a partial archive under a real backup name.

Two bounds sit around it. `http.NewResponseController(w).SetReadDeadline` gives the upload its own deadline (`upload_timeout: 30m`) instead of a server-wide `ReadTimeout` that would also apply to a 2 GiB one; and a `ctxReader` wrapper makes a shutdown cancel a transfer in progress rather than wait it out.

The gate measures the result rather than asserting the shape of the code:

```
    peak RSS 24 MiB before, 24 MiB after a 256 MiB upload
  ok  a 256 MiB upload is streamed to disk, not buffered in RAM
```

with the budget set at `max(96 MiB, size/2)` — loose enough not to be flaky, tight enough that any buffering of the body fails it.

## 3. Invariant I10, and who is allowed to write the index

> **I10.** `backups.json` ≡ `ls backups.dir`.

v1 had two sources of truth and believed the wrong one. `backup_log.txt` was appended from the agent's report — the size the agent measured before uploading — so an upload that failed, or landed short, left a log entry describing an archive that was not there. That is bug 4's shape: the controller offers a backup it cannot serve, and you find out during a restore.

v2 enforces I10 structurally, with **one generator**. `Store.rescanLocked` walks the directory and returns the index; every mutation (`Put`, `Prune`, `MarkDownloaded`, `Seed`) ends by calling it. There is no code path that adds an entry without looking at the file, and none that removes one without deleting the file. `internal/httpapi/store_test.go` has `indexMatchesDisk`, called after every mutating test, because the value of routing all writes through one generator is that no sequence of operations can break it — and the way to keep that true is to check after each one.

Two consequences worth naming:

* **Only backup-shaped names are indexed** (`state.IsBackupName`). `backup_log.txt`, a leftover `.part`, and an operator's hand-dropped `world.zip` are all invisible to the index, which is what lets a rescan need no manifest to tell it what the directory holds.
* **Timestamps come from the mtime, not the filename.** An archive an operator uploads from their laptop is named for the day it was made three weeks ago; taking the date from the name would let the next prune expire it under `retention_days` — deleting the exact archive somebody uploaded in order to restore from it.

## 4. The wire contract

`internal/httpapi/api.go` holds the paths, headers and body shapes and **nothing else**, and both sides import it. In v1 the two sides were a `curl` line in `entrypoint.sh` and a hand-rolled `do_GET` router in `storage_server.py`, and the only thing keeping them in agreement was that nobody edited either.

| Path | Method | Realm | Notes |
|---|---|---|---|
| `/healthz` | GET | public | the Akash liveness probe; it must not need a secret |
| `/common.zip`, `/client.zip` | GET | public | mods and non-secret config; `client.zip` is what players download |
| `/server.zip` | GET | `server-files` | `.ini` files with the real passwords substituted in on the way out |
| `/backups.json` | GET | **public** | the index. It carries names, sizes, digests and stamps — no secrets — and the dashboard reads it |
| `/backups/<name>` | GET | `backups` | download; records `downloaded_at` |
| `/backups/<name>` | PUT | `backups` | raw archive as the body, `X-PZ-SHA256` alongside |

Status codes, from `statusFor`:

| Condition | Code |
|---|---|
| PUT stored a new archive | **201** |
| PUT replaced an existing one | **200** |
| no bearer token, or the wrong one | 401 + `WWW-Authenticate: Bearer realm="…"` |
| `ErrBadName` — not a backup filename, or a nested path | 400 / 404 |
| `ErrDigestMismatch` | **422** |
| `ErrTooLarge` | 413 |
| `ErrNoSpace` | 507 |
| read deadline exceeded | 408 |
| client hung up | 499 |
| `ErrNotFound` | 404 |

Two decisions in there are deliberate and easy to get wrong:

**`PUT` is idempotent, and its two success codes are different.** A retry after a half-finished upload has to be safe — that is the whole reason the name is in the path and not in a multipart part. 201-vs-200 is how the uploader learns which happened without having to ask.

**A malformed name is refused before any path is joined.** `BackupName` rejects `""`, `.`, `..` and anything containing `/` or `\`, so `/backups/sub/x.zip` and `/backups/../etc/passwd` never reach a `filepath.Join`. The gate asserts the nested case explicitly, because a path-traversal check that lives inside the join is a check that one refactor can remove.

## 5. Auth: closed by default

```go
type Realm string
const (
	RealmPublic      Realm = ""   // the zero value
	RealmServerFiles Realm = "server-files"
	RealmBackups     Realm = "backups"
)
```

The zero value being *public* looks backwards for about a second, and then: **a handler with no realm serves a public file, and every guarded path names its realm.** A forgotten field cannot open something that was closed, because there is nothing to forget — the guarded handlers are the ones that say a name.

The comparison is constant-time over fixed-length SHA-256 digests of the token, not over the tokens themselves: `subtle.ConstantTimeCompare` on two 32-byte digests leaks neither the length nor a prefix. And **a realm with no configured secret denies everything.** It does not fall back to public. A controller started without `PZ_BACKUPS_PASSWORD` serves no backups at all, which is a visibly broken controller rather than a quietly open one.

`BearerToken` returns `""` for anything that is not exactly one `Bearer` credential, so a malformed `Authorization` header can never be mistaken for a match against an empty password.

## 6. `server.zip`: the passwords exist only on the wire

`controller.storage.substitute_entries: [Server/*.ini]` names the entries that get the game passwords substituted as the archive is served. Everywhere else — git, the image layers, the Akash manifest, the SDL — they are `__JOIN_PASSWORD__`, `__RCON_PASSWORD__`, `__ADMIN_PASSWORD__`.

Three details that each cost a bug if got wrong:

* **Matching uses `path.Match`, which knows only `/`.** A zip writer that stored `Server\vsrania.ini` would match nothing and substitute nothing, and the server would come up with a literal `__JOIN_PASSWORD__` as its password. `build_packages.py` writes forward slashes; the gate opens the fixture `server.zip` and **fails unless the entry names contain `/`**, so the assumption is checked rather than assumed.
* **Non-matching entries are copied with `OpenRaw`/`CreateRaw`** — no inflate, no deflate. The mods are the bulk of the archive and re-compressing them on every serve would put a CPU spike in front of every server boot. The gate verifies a 4 KiB `mods.bin` comes back with the digest it was staged with.
* **The scope is `Server/` deliberately.** `client.zip` carries `.ini` files too (mod options, keybinds) and those are public; a wider glob would substitute secrets into the archive players download. `substitute_max_bytes: 4194304` bounds what may be read into memory to be rewritten.

## 7. The Store → FSM handoff

This is the seam the step is really about. The FSM publishes `backups.json`; it does **not** decide the contents.

```go
// fsm.BackupStore — optional
type BackupStore interface {
	Index() *state.Backups
	Seed(published *state.Backups) *state.Backups
	Prune(policy state.RetentionPolicy, protect ...string) ([]string, error)
}
```

Optional, and nil means the machine keeps its own index from agent reports — which is what `--dry-run` on a laptop and most of the FSM unit tests need, since a lifecycle test about a halt should not need a directory. `cmd/pzctl/controller.go` has the one hazard this creates spelled out, because a typed nil pointer stored in an interface is not nil:

```go
// A nil *Store must not be handed over as a non-nil interface: the machine tests
// `store == nil` to decide whether it owns the index.
var backups fsm.BackupStore
if store != nil {
	backups = store
}
```

`handle` calls `refreshIndex()` **before** dispatching any event, so every read below it — the periodic cadence, a restore target's existence, the snapshot — sees the directory as it is now and not as an agent report once described it. When the index changes, `refreshIndex` marks the document dirty and the coalescing window publishes it.

The notification goes the other way as a **nudge, not a payload**:

```go
store, err := openStore(cfg, *backupsDir, logf, func() {
	if m != nil {
		m.Send(fsm.Tick("backups"))
	}
})
```

The machine reads the store when it handles the event, so a burst of uploads costs one publish instead of one per notification — and the machine stays the only thing that writes the branch. Combined with `git.min_push_interval: 5s`, an upload reaches `state/controller:backups.json` within about five seconds; the gate's poll for it succeeds well inside its 30-second budget.

### 7.1 The RFC 3339 precision bug

Found by writing the seed test, and it would have been invisible in production except as a slow start.

`state.Stamp.MarshalJSON` formats with `time.RFC3339` — **second granularity**. An mtime carries nanoseconds. `rescanLocked`'s digest-reuse key is `(name, size, CreatedAt)` compared with `Time.Equal`, so an index seeded from the branch at full precision disagrees with the directory on **every entry**, and the store re-hashes the whole of `backups.dir`.

The cost is not cosmetic: up to `retention_count` × `upload_max_bytes` of reading, on the one occasion when the controller's job is to come back quickly. The fix is one line — `info.ModTime().Truncate(time.Second)`, rendered `.In(s.loc)` — and `TestSeedDoesNotRehashArchivesTheIndexAlreadyKnows` asserts no `hashed` line appears when seeding from a round-tripped index.

### 7.2 What `Seed` is for

With no persistent storage — the locked design — a restarted controller may come back on an empty volume or a warm one and cannot tell which without looking. So:

* **the disk decides what exists.** An entry the published index carries and the directory does not is dropped; offering a download that 404s is worse than forgetting an archive, and `restore_target` could otherwise be pointed at it.
* **the published index is the only memory of `downloaded_at`.** Nothing on the filesystem records whether an operator ever fetched a copy, and that stamp is the only evidence a copy exists off this disk — the thing `disk_warn_percent` warns about.

`Seed` does not fire `onChange`. The caller is what publishes; calling back into it from inside its own startup is a loop.

### 7.3 Retention had no driver

`state.Backups.Expired` and `Store.Prune` were written in an earlier step and **nothing called them**. On a 20 GiB volume that is a controller that fills its own disk and then answers 507 to the halt backup.

Retention now runs on the housekeeping event — `KindTick` in `handle`, and explicitly in `Machine.Once`, because a `--once` controller driven from cron has no tick and would never do housekeeping at all. It is deliberately *not* in `advance`: `advance` is a function of the documents and the clock and nothing else, and this deletes files.

And it protects `restore_target`. An unprotected prune is worse than no prune, because `retention_count: 1` against a pinned older archive deletes the one file the next boot is going to ask for. `TestTheTickPrunesAndProtectsTheRestoreTarget` asserts the protection, the deletion of the middle archive, and that the published index followed the deletion — drift there is exactly what I10 forbids.

## 8. One port or two, and `/healthz` vs `/state`

`controller.webhook_port: 0` folds the webhook onto `http_port` so Akash exposes one endpoint instead of two. Folding can only be honoured when there is a file service to fold into, so the condition is `hook != nil && cfg.WebhookOnHTTPPort() && store != nil`; the SDL reads the same setting, so the two cannot disagree.

The fold moved one path. `httpapi` owns `/healthz` and answers it with **liveness** — which is what the Akash probe wants, and a probe that returned the FSM's status line would restart the container for being offline. The machine's status line therefore lives at **`/state`** on the folded arrangement, where step 7's dashboard will replace it. On the two-port arrangement the webhook server keeps its own `/healthz` with the status line, because there is no `httpapi` on that port to collide with. The stale comment in `controller.go` that still claimed otherwise is corrected.

`config.yaml` currently names `8080`, which is two ports. Folding is a one-key change with a visible consequence at cutover (one exposed endpoint instead of two, so one URL to configure at GitHub and one in Cloudflare) — left as it is rather than changed silently.

## 9. What was tested, and how

**56 tests in `internal/httpapi`**, against a real directory and a real `httptest` server; 398 across the suite; green with `-race` on Debian under WSL, `go vet` and `gofmt` clean.

New this step, beyond the package's own tests:

| Test | What it pins |
|---|---|
| `TestTheIndexComesFromTheStoreAndNotFromTheReport` | I10 at the seam: the report and the directory disagree about size and digest, and the directory wins |
| `TestAReportForAnArchiveTheStoreDoesNotHoldIsNotFollowed` | bug 4's shape: `restore_target` is not moved to an archive that did not land, **and the halt still settles** |
| `TestTheTickPrunesAndProtectsTheRestoreTarget` | retention has a driver, and it cannot delete the next boot's world |
| `TestLoadSeedsTheStoreFromThePublishedIndex` | `Seed` is called exactly once per load, and `downloaded_at` survives a restart |
| `TestSeedKeepsTheDiskAsTheAuthorityAndTheIndexAsTheMemory` | the two halves of §7.2, and that `Seed` publishes nothing |
| `TestSeedDoesNotRehashArchivesTheIndexAlreadyKnows` | §7.1, the RFC 3339 precision fix |
| `TestSeedWithNothingPublishedKeepsWhatTheDiskSays` | a first-ever start still trusts a warm volume |

`newHarness` grew variadic `opts ...func(*Deps)` so a test can inject a storage layer without every lifecycle test acquiring a directory it does not need.

### The gate

`scratch/gate6.ps1` builds the binary first — *a gate that passes against yesterday's binary is worse than no gate* — seeds a throwaway bare remote, patches the committed `config.yaml` into laptop values, builds real `server.zip`/`common.zip`/`client.zip` fixtures, starts a real `pzctl controller --dry-run` process, and drives it over `curl.exe`. The 19 assertions, in the order they run:

```
=== realms ===
  ok  /backups.json is public and starts empty
  ok  a backup download is closed without the backups token, and the challenge names the realm
  ok  a nested backup path is refused before anything joins it onto a directory
=== upload ===
  ok  an upload without the token writes nothing
  ok  PUT /backups/<name> stores the archive and echoes the size and digest it wrote
  ok  the index describes the archive the directory holds, and nobody has downloaded it yet
=== download ===
  ok  the archive comes back byte-identical, with the digest on the response
  ok  the fetch is recorded as downloaded_at (08/21/2026 12:55:58)
=== refusals ===
  ok  a body that does not match its digest is refused and leaves nothing behind
  ok  a name that is not a backup filename is refused, so the directory stays self-describing
=== replacement ===
  ok  PUT is idempotent, and the index follows the bytes that are actually there now
=== streaming ===
    peak RSS 24 MiB before, 24 MiB after a 256 MiB upload
  ok  a 256 MiB upload is streamed to disk, not buffered in RAM
  ok  and it comes back identical
=== server.zip and the public packages ===
  ok  common.zip and client.zip are public
  ok  server.zip is closed without the server-files token
  ok  server.zip is served with the real passwords substituted into Server/*.ini
  ok  the entries that match nothing come through unchanged
=== the index reaches the state branch ===
  ok  state/controller:backups.json is the directory, digests and download stamps included

GATE PASSED (19 assertions)
```

The last one is the step's handoff, and it is checked by reading the branch with `git show state/controller:backups.json` rather than by asking the controller — a gate that verified the index by calling the same process that generated it would pass while the transport was broken.

Three things the gate does that are worth keeping in mind when editing it:

* The digest-mismatch assertion checks for **no leftover `.part` files**, not just the 422. A refusal that leaves debris in `backups.dir` is a slow leak on the volume that has the least room.
* The replacement archive differs in **both size and PRNG seed**. Same-size-different-bytes would have been the stronger test; same-size-*same*-bytes would have made the assertion vacuous, which is what a fixed seed originally did.
* The 256 MiB body is never read into the PowerShell process (`Req -Binary` leaves it on disk). A gate that buffered the archive to check it would be the memory hog it is testing for.

`scratch/gate.ps1` (step 3's gate) needed rewriting for the `/healthz` → `/state` move and now passes again.

## 10. Two things left open

### 10.1 Retention cannot be satisfied on the configured disk

```yaml
backups:
  retention_count: 24
  upload_max_bytes: 2147483648   # 2Gi
controller:
  resources:
    storage: 20Gi
```

24 × 2 GiB = **48 GiB of archives allowed on a 20 GiB volume** that also holds `packages_dir` and needs `min_free_bytes: 2Gi` free. The count rule can never bind, because the disk fills first; the only thing surfacing that is `disk_warn_percent: 70`, and the only thing preventing a 507 on the halt backup is that worlds are currently much smaller than 2 GiB.

Nothing is broken today and no code change is needed. It is a numbers decision — raise `storage`, lower `retention_count`, or accept that the disk is the real limit and the count is decoration — and it belongs to whoever is paying for the volume.

### 10.2 The admin password has no delivery channel

`internal/agent/boot.go` asserts:

> No `-adminpassword`: the agent holds no admin secret, on purpose. The password is substituted into the `.ini` by the controller.

The substituter does know `__ADMIN_PASSWORD__`, and `PZ_ADMIN_PASSWORD` is loaded. But **`vsrania.ini` has no `AdminPassword` key, and Project Zomboid has no such ini setting** — so the channel that comment describes has nowhere to land. Either the server prompts on first boot (in which case the mechanism has to be `-adminpassword` on the command line after all, and the agent does need the secret), or PZ carries the admin account forward in the world's database and the question only arises on a fresh world.

That is one cheap empirical observation — boot a fresh world with no admin password and watch what it does — and it belongs to the step that owns the boot sequence rather than to this one. The comment is wrong as written and is flagged here so it is not read as settled.

## 11. Next

Step 7: the dashboard on `html/template` at RU/EN parity, displaying `pz.<domain>:16261`. It mounts through `ServerOptions.Extra`, which is the same field the folded webhook uses — so step 7's first job is to combine the two rather than replace one with the other.


