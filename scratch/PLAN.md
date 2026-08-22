# PZ-on-Akash v2 — Design & Execution Plan

Status: **awaiting approval**. Decisions locked by the user on 2026-08-19:

| Decision | Choice |
|---|---|
| State transport | **Git stays the bus**, with strict per-file ownership |
| Backup durability | **None.** User downloads backups periodically and uploads before a start. `/data/backups` is a cache. |
| Migration | **Greenfield only.** Do not modify the existing bash. Live system keeps its bugs until cutover. |
| Secrets | **Reuse current values.** Move them out of git; no rotation. |

Language: **Go 1.23+**, one module, one binary `pzctl` with subcommands `controller` and `agent`.
`build_packages.py` stays in Python (build-time SteamCMD wrangling, already works).

---

## 1. Invariants — enforcement mechanism for each

These are the contract. Every one must be enforced *structurally*, not by convention.

| # | Invariant | Enforcement in v2 |
|---|---|---|
| I1 | Exactly one PZ deployment exists | `state.ActiveDseq` is the singleton; FSM is the only writer; deploy refuses if non-nil |
| I2 | State files are always valid, complete, typed | Marshal from struct → temp → fsync → rename. **Never** string-built JSON. Strict unmarshal (`DisallowUnknownFields`), repair-on-read to zero value + log |
| I3 | `status` only advances legally | Explicit transition table; illegal transitions are rejected and logged, not applied |
| I4 | **Exactly one writer per file** | Ownership enforced by *separate git branches* (§2). Controller physically cannot write agent state and vice versa |
| I5 | Each trigger processed exactly once | Trigger consumed (`git rm` + push) **before** work begins; `state.ProcessedSHAs` ring buffer dedups by push head-SHA |
| I6 | Our own pushes never re-enter us | Webhook filter: (a) ref must be `main`, (b) changed paths must intersect `triggers/`, (c) pusher != bot identity, (d) head SHA not in `ProcessedSHAs` |
| I7 | `intent` is authoritative | `intent: running\|stopped` written by controller, **read by the agent** (today the container never reads it) |
| I8 | Server PID 1 outlives PZ | Agent **parks** (`select{}`) after PZ stops. Never `exit`. Kubelet has nothing to restart. Controller closing the lease is what removes the pod |
| I9 | `restore_target` names a file that exists | Validated against `backups.json` + on-disk stat at set time *and* at read time; missing → explicit `RestoreFailed` state, never silent skip |
| I10 | `backups.json` ≡ `ls /data/backups` | Single generator function, called after every mutation; includes sha256 + size |
| I11 | Restore completes in bounded time | Restore is a **pull**: agent `GET /backups/<name>` with retry. No rendezvous, no 60s window |
| I12 | `players_count` is live | Single writer (agent, from PZ stdin/RCON). No code path may write a literal `0` |
| I13 | Backups are recoverable | Accepted as **manual** per decision. Dashboard surfaces "unsaved backups" prominently; retention by count *and* age; upload/download are first-class |
| I14 | No secrets in git | `config.yaml` has no secret fields (schema has no place to put them); `*.ini` are templates with `__PLACEHOLDER__`; SDLs are rendered in-memory at deploy time and never written to the repo |
| I15 | Agent knows the current controller URL | Controller writes `controller.public_url` to its state branch every resolve; agent reads it from git. No `http://controller:8000` placeholder lie |

---

## 2. Git layout — how single-writer is enforced

`config.layout: branches` (default). Escape hatch `layout: single` puts everything on `main` if the user prefers.

### Branch `main` — human-owned
```
config.yaml            # single source of truth, NO secrets
mods.json
triggers/              # edge triggers; the ONLY paths that fire the webhook
  start
  backup
  halt
  restore
  stop_at
common/                # shared client+server assets
client/
server/                # PZ configs; *.ini are TEMPLATES with __RCON_PASSWORD__ etc.
README.md
```
Deleted: `deployment.yaml` (now rendered from `config.yaml` + env secrets, in memory).

### Branch `state/controller` — controller-owned, force-pushed as a single orphan commit
```
state.json        # lease, dseq, ip, ports, price, status-as-observed, intent, restore_target, processed_shas
backups.json      # derived index: name, size, sha256, mtime
server_info.json  # read-only derived mirror, kept for compatibility with anything external
```

### Branch `state/agent` — agent-owned, force-pushed as a single orphan commit
```
agent.json        # phase, players_count, pz_pid_alive, restart_count, last_error, liveness_ts, backup_report
```

Why branches: a single force-pushed commit per branch means **zero history growth**, webhook noise is trivially filtered by `ref`, and neither side can touch the other's file even by accident. `main`'s history stays human-readable.

Push rate: controller pushes on state change (coalesced, `git.min_push_interval_sec: 5`). Agent pushes only on **phase change** plus a liveness stamp at most every `agent.liveness_push_sec: 600`. Player-count pushes are rate-limited by `agent.players_push_min_interval_sec: 120`. Expected: single-digit pushes per hour, not per minute.

---

## 3. Proposed simplification: drop SSH and (optionally) RCON

**Recommended, flagged for approval — this is beyond the literal ask.**

Today the controller SSHes into the server to zip `Saves/` and SCPs restores in. Instead:

- **Backups are agent-initiated and uploaded.** The agent owns the PZ process, so it writes `save` to PZ's **stdin** (no RCON needed), zips `Saves/` locally, and `POST /upload`s to the controller with retry. Controller verifies sha256, updates `backups.json`.
- **Restores are agent-pulled.** `GET /backups/<name>` before PZ launch.
- Consequence: **no sshd in the server image, no port 2222, no scp/ssh timeouts, no remote-zip pipeline.** Bug 4 is fixed *by construction* — the party holding the bytes is the party doing the transfer, and it can retry.
- **RCON becomes optional** (`server.rcon.enabled`, default `false`): player count comes from the agent parsing PZ output. That closes port 27015 too, leaving the SDL exposing only `16261/udp` + `16262/udp`.

Trade-off: `players_count` now costs a (rate-limited) git push instead of a controller-side poll. Set `rcon.enabled: true` to get the old behaviour back.

Net: server SDL goes from 4 exposed ports to 2, and the pz-saves deploy key loses one of its two uses.

---

## 4. Controller FSM

Single owner goroutine. All inputs arrive as messages on one channel — webhook, ticker, agent-state-change, deploy result. **A long operation in flight makes duplicate events no-ops.** This is the structural fix for Bug 2b.

States: `Offline → Deploying → Booting → Online → {Backing, Stopping} → Closing → Offline`, plus `Failed`.

Events: `TriggerStart` `TriggerBackup` `TriggerHalt` `TriggerRestore` `StopAtElapsed` `AgentPhase(p)` `DeployResult(r)` `BackupUploaded(id)` `Tick`.

Halt sequence (compare to the current one, which consumes `halt` *last*):
1. Consume `triggers/halt` + push. **First.** Any further `TriggerHalt` while state ∈ {Stopping, Closing} is dropped with a log line.
2. `intent = stopped`, push state.
3. Cancel any in-flight deploy via `context`.
4. `status = stopping`.
5. Request backup from agent; wait for `BackupUploaded` (bounded by `backups.halt_timeout_sec`).
6. Tell agent to stop PZ; wait for `AgentPhase(stopped)` (agent then parks).
7. Close the Akash deployment. Clear `ActiveDseq`.
8. `status = offline`.

---

## 5. Agent boot sequence

1. Read env: `CONTROLLER_URL`, `SERVER_FILES_PASSWORD`, `BACKUPS_PASSWORD`, deploy key, git identity.
2. Clone pz-saves; fetch `main` (config) and `state/controller` (intent, restore_target, urls).
3. If `CONTROLLER_URL` unset/unreachable → read `controller.public_url` from `state/controller`.
4. `GET /common.zip`, `GET /server.zip` (auth). **Controller substitutes `__RCON_PASSWORD__` / `__ADMIN_PASSWORD__` / `__JOIN_PASSWORD__` when serving** — so no game secret ever lands in the image layer or the Akash manifest.
5. If `restore_target` set: `GET /backups/<name>`, verify sha256 against `backups.json`, unzip into `Saves/`. On failure → `phase = restore_failed`, push, **park**. Never boot a fresh world over a requested restore.
6. Render `ProjectZomboid64.json` JVM args from `config.server.memory_*`.
7. Launch PZ with stdin/stdout pipes. Watch for `*** SERVER STARTED ***` → `phase = online`.
8. Supervise: on PZ exit, if `intent == running` and `restarts < server.crash.max_restarts` → relaunch after `backoff_sec`; else `phase = stopped` and **park forever**.

---

## 6. `pz-saves/config.yaml` — schema outline

Loaded with `KnownFields(true)`: a typo is a startup error. `pzctl config validate` gates CI.

```yaml
version: 1
identity:      { server_name, timezone }
git:           { repo_url, branch, user_name, user_email, layout, min_push_interval_sec }
controller:    { http_port, webhook_port, image, image_tag, resources{cpu,memory,storage},
                 pricing_uakt, poll{tick_sec, idle_sec, active_sec} }
server:        { image, image_tag, resources{cpu,memory,storage},
                 memory_max, memory_min,
                 ports{game, game_udp2, rcon, ssh},
                 ip_lease, rcon{enabled},
                 crash{max_restarts, backoff_sec},
                 online_timeout_sec }
akash:         { api_base, deploy_days, max_attempts,
                 price{max_usd_per_day, min_usd_per_day, tolerance, akt_usd_fallback},
                 placement{countries[], ref_lat, ref_lon, skip_ttl_sec},
                 timeouts{bid_wait_sec, lease_ready_sec},
                 funds{check_sec, min_topup_usd, margin, blocks_per_day} }
backups:       { interval_sec, retention_days, retention_count,
                 on_halt, halt_timeout_sec, pause_file,
                 disk_warn_percent, upload_max_bytes }
dns:           { enabled, provider, domain, zone_id, proxied, ssl_mode }
game:          { map, max_players, pause_empty, public, open, save_world_every_minutes }
dashboard:     { default_locale, locales[] }
agent:         { liveness_push_sec, players_push_min_interval_sec }
```

`placement.countries` is an explicit ISO-code list — the current fuzzy country-name map is replaced.

---

## 7. Secrets (values reused, locations changed)

Live **only** in the controller's SDL env (untracked local file, as `AKASH_API_KEY` already does):

`AKASH_API_KEY`, `PZ_SAVES_DEPLOY_KEY` (b64), `WEBHOOK_SECRET`, `STORAGE_PASSWORD`,
`SERVER_FILES_PASSWORD`, `BACKUPS_PASSWORD`, `RCON_PASSWORD`, `PZ_ADMIN_PASSWORD`,
`PZ_JOIN_PASSWORD`, `CLOUDFLARE_API_TOKEN`

Server SDL env (rendered by controller at deploy time, never committed):
`CONTROLLER_URL`, `SERVER_FILES_PASSWORD`, `BACKUPS_PASSWORD`, `PZ_SAVES_DEPLOY_KEY`, git identity.
Game passwords are **not** here — they arrive substituted inside `server.zip`.

**GitHub Actions secrets: no additions.** One key, for the build-time checkout of pz-saves. Held as `SSH_PRIVATE_KEY` (the workflow also accepts v1's name `PZ_SAVES_SSH_KEY`, so a half-configured repository still builds). This survived the cutover unchanged as a *count* while changing in every other respect: the key is new, the repository it opens is private, and `gh` did have to be installed after all — for the renames and the deploy key, not for secrets.

Out of git, concretely:
- delete `pz-saves/deployment.yaml`
- `pz-saves/server/Server/vsrania.ini` → template with `__RCON_PASSWORD__`, `__ADMIN_PASSWORD__`, `__JOIN_PASSWORD__`

Noted and accepted: the deploy key remains valid and remains in the history of both repos (`827e005`, `7d9f0fd`, `78f8d28`, `1359ced`).

---

## 8. Execution steps

Work happens in a new top-level `pzctl/` directory and a `v2` branch of pz-saves. **Nothing in `pz-controller/` or `pz-server/` is touched.**

| Step | Deliverable | Gate |
|---|---|---|
| **1** | `pzctl/` module skeleton; `internal/config` full typed schema; `pzctl config validate`; `config.yaml` authored from today's real values; `pzctl sdl render` printing both SDLs for byte-level review | You diff the rendered SDLs against the current ones |
| **2** | `internal/state` (typed, atomic, repair-on-read) + `internal/gitbus` (branch model, force-push, SHA dedup, self-push filter). Unit tests incl. corrupt-input fuzz | Tests green; `pzctl state show` reads the live repo |
| **3** | `internal/fsm` transition table + owner goroutine + webhook receiver (HMAC, path/ref/author filter). Akash driver stubbed to dry-run | `pzctl controller --dry-run` walks a full start/backup/halt cycle from real triggers |
| **4** | `internal/agent`: boot, config render, pull-restore, PZ supervise, park-on-stop, agent-side backup+upload | Runs locally against a stub controller |
| **5** | `internal/akash`: deploy, bids, lease, watchdog, close, escrow top-up. `internal/dns` Cloudflare | First real deploy on a throwaway dseq |
| **6** | `internal/httpapi`: auth, streaming upload (fix the read-whole-body-into-RAM defect), download, `backups.json` index, retention | Manual download/upload round-trip |
| **7** | Dashboard ported to `html/template` at feature parity (RU/EN + pluralization + downloads) | Visual diff against current |
| **8** | CI: one Go build → two images, `config validate` gate, no secrets in layers | Green workflow |
| **9** | Cutover | see below |

### Step 9 — cutover checklist
1. ~~**Download all backups from the current controller first**~~ — **not done, by decision.** The user's instruction was "Don't download old backups, forget about them, just kill old deployments and deploy new", and, asked directly whether the live world should be rescued, "Kill it, start a fresh world". v2 therefore starts on an empty map and item 9 below pushes `triggers/start` with no `restore_target`.
2. ~~Halt the server through the old system~~ — skipped for the same reason. A graceful halt exists to produce a final backup nobody is keeping.
3. **Close both v1 deployments — the controller first.** Done 2026-08-22: `dseq 1787078661931` (service `controller`, $0.24 unspent) then `dseq 1787103872228` (service `pz-server`, the live world, $2.15 unspent). ~$2.39 settles back to the wallet.
   - The order is not cosmetic. v1's controller reconciles `desired_state: running` by creating a server deployment, so closing the server while the controller lived would have spent the refund on a replacement.
   - `pzctl akash leases` finds only the server: `Adopt` identifies a deployment by service name and deliberately never claims the controller's own. The controller's dseq came from the console API directly — `scratch/v1-identify.ps1`, which also proves which is which by service name rather than by creation order.
   - **This gates the renames.** GitHub redirects pushes for a renamed repository, so a v1 controller that outlived the rename would push its bus files into whatever repository next occupied the old name — the clean v2 one.
4. Merge pz-saves `v2` → `main` (adds `config.yaml`, `triggers/`, templated inis; removes `deployment.yaml`).
   - **There is no `v2` branch of pz-saves yet** — `git branch -a` shows `main` and nothing else, and `main` has neither `config.yaml` nor `triggers/`. The v2 config lives in this repository as `pzctl/config.yaml`, the draft the CI gate validates. So this item begins by *creating* that branch from today's `main` and adding the three things to it; there is nothing to merge until then.
   - Done as an **orphan** branch (`c1a2f04`, 20 files), not a branch off `main`: `main`'s history is 167 commits of v1 bus traffic and four published passwords, and the new repository is the one place that history can be left behind rather than rewritten.
5. **Replace the literal passwords in `server/Server/*.ini` with `__RCON_PASSWORD__` and `__JOIN_PASSWORD__`**, and move the two values into the controller's environment (`PZ_RCON_PASSWORD`, `PZ_JOIN_PASSWORD` — `internal/secrets`). The committed ini still carries the real values v1 put there; the substitution happens in the controller as the archive is served, so nothing in the repo or in an image layer needs them. Until this lands, `docker/check_image.sh` emits a `::warning::` for each literal it finds in `server.zip`; **after** it lands, change that `note` to `fail` so a regression is a red build.
   - **`AdminPassword` is not in the ini at all** — `server/Server/vsrania.ini` has
     `RCONPassword` (line 167) and `Password` (line 188) and no admin key, because v1
     passed the admin password as `ADMIN_PASSWORD` in the server SDL's environment
     instead. So this item *adds* `AdminPassword=__ADMIN_PASSWORD__` rather than
     replacing a literal. The controller side is already built and tested: step 6's
     `httpapi.Substituter` maps `__ADMIN_PASSWORD__` to `secrets.Set.AdminPassword`,
     loaded from `PZ_ADMIN_PASSWORD` on the controller only, and substituted into the
     response body as `server.zip` is served — never onto disk, never into a layer,
     never into the game container's environment or its `/proc` command line.
   - **`hrkcz001/pz-saves` is a public repository** (found in step 8: the CI checkout succeeded with no deploy key). So those two passwords are not merely in git, they are readable by anyone, along with every value the ini holds. **Recommend rotating both at cutover** rather than reusing them — this is the one place the "reuse everything, just move it out of git" decision meets a value that is already public, and it is your call. Rotation costs one edit to two GitHub secrets and one message to the players; reuse costs nothing and leaves a known-public join password in place.
   - **The exposed set is four values, not two, and three of them are distinct.** Public
     `pz-saves` also tracks `deployment.yaml` at HEAD (commit `5201938`, 2026-08-19) —
     v1's *server* SDL, carrying `ADMIN_PASSWORD` (12 chars) and `STORAGE_PASSWORD`
     (19 chars) as plain env entries. With the ini's `RCONPassword` (19) and `Password`
     (4) that is four live credentials anyone can read. `RCONPassword` and
     `STORAGE_PASSWORD` are **the same string**, so rotating either one means rotating
     both — they are one value doing two jobs (RCON login and the dashboard's storage
     realm). §7 already says to delete this file; it is listed here because it is an
     exposure, not only a layout problem.
   - **Two of those values were echoed into this session's terminal output** on
     2026-08-22, by a gate command of mine whose helper function named `H` collided with
     PowerShell's alias for `Get-History`; the binding error printed the argument it
     could not convert. The affected values are the shared `RCONPassword`/
     `STORAGE_PASSWORD` string and `ADMIN_PASSWORD`. They were already world-readable in
     a public repository, so this changes no attacker's access — but it does mean they
     now also sit in a terminal scrollback and a session transcript, and it removes any
     remaining argument for reuse. **Rotate all three distinct values at cutover.**
   - Decide whether the new `pz-saves` is public or private. v2 needs neither: with placeholders in the ini, nothing secret is in the repository, and the workflow's `ssh-key:` line works either way.
6. **Delete `pz-controller/` and `pz-server/`** — moved here from item 11, and it has to be here. Both v1 workflows are path-filtered to those directories, and a push that creates a repository's first branch counts every file as added, so a new repository containing v1's tree would have built v1's bash images and published them as its own packages before anything else ran. The two `deployment.yaml` files are gitignored and survive in the working copy, which is why `.dockerignore` still excludes both directories.
7. Rename the repositories: `pz-akash` → `pz-akash-proto`, `pz-saves` → `pz-saves-proto`, then create the new `pz-akash` (public) / `pz-saves` (**private**) from the v2 trees. Both image names in `config.yaml` and the workflow's `${{ github.repository }}` follow the slug, and `scratch/gate8.ps1` asserts the two agree — run it after the rename.
   - Done 2026-08-22. Four repositories now: `pz-akash` (public, empty until item 9), `pz-saves` (private, `main` = the orphan `c1a2f04`), and both `-proto` originals left public and untouched.
   - **A renamed remote is a live hazard for as long as a working copy points at it.** `git remote rename origin proto` keeps the *URL*, and that URL now resolves to the new repository — so `proto` in `~/pz-saves` briefly named the clean repo, one `git push proto v1-proto:main` away from restoring 167 commits of v1 bus traffic and four published passwords into it. Both remotes are now explicit: `origin` → the new repository, `proto` → `…-proto.git`. The same redirect is why item 3 had to close v1 first.
   - GHCR packages are linked to a repository by id, not by name, so the rename may leave `pz-akash-controller` and `pz-akash-server` attached to `pz-akash-proto` and the first push from the new repository 403s. Both packages certainly exist — `sha-6b9f327` and `sha-bcba937` are what `config.yaml` currently pins — and the `gh` token has neither `read:packages` to inspect the linkage nor `delete:packages` to break it. Item 9's CI run answers it for free rather than guessing, because the pins are being replaced anyway; if it 403s, ask.
8. Install the new deploy key on the new pz-saves (**write-enabled** — the controller pushes state), then set the secrets CI needs on the new pz-akash.
   - Done 2026-08-22. Key `160994786`, ed25519, write-enabled, verified by a `git ls-remote` that used it and nothing else (`IdentitiesOnly=yes`). v1's `autosave` key stays on `pz-saves-proto`, where it can only reach v1's history.
   - **One secret, not eleven: `SSH_PRIVATE_KEY`.** §7 said "no additions" and that still holds. The ten `PZ_*` values reach a container through its Akash manifest, rendered in memory at deploy time from `~/.pz-akash/secrets.env` — no workflow reads them, and an Actions secret cannot be read back, so a copy there would not even serve as a backup. It would be eleven values to rotate where one is needed. Losing `secrets.env` costs a rotation, not a recovery: every value in it is either regenerable here or reissued from the Akash and Cloudflare consoles.
   - What made the key *mandatory* rather than convenient is item 7's decision: `hrkcz001/pz-saves` is private now, and the job token cannot read another repository. LF endings and a trailing newline were checked before setting it — OpenSSH rejects a key without the final newline, and the failure surfaces as an unhelpful checkout error. Set by file redirect through `cmd`, never as an argument.
9. Run CI. **Only after the new pz-saves exists**: `docker/check_image.sh` now *fails* on a literal password in `server.zip`, and the old repository still has four of them. Then pin `controller.image_tag` / `server.image_tag` to the new shas in both copies of `config.yaml` and re-run `scratch/gate8.ps1`.
   - Run 1 (`32571211644`, workflow_dispatch on `5f4d122`): **check green in 1m46s; both images built and both passed `check_image.sh`; both pushes failed** with `denied: permission_denied: write_package`. Item 7's GHCR hazard, confirmed rather than guessed.
   - What that run did prove, and could only prove here: the private `pz-saves` checkout works with the new deploy key, `pzctl config validate` passes against the *shipped* config, and the flipped gate passes against the templated inis — `__RCON_PASSWORD__`, `__ADMIN_PASSWORD__`, `__JOIN_PASSWORD__` in `server.zip` where four literals used to be.
   - **A push does not reliably trigger this workflow when it creates the default branch.** The first push carried v1's whole history, and GitHub's path filters did not match; `gh workflow run` did. Not worth designing around — it happens once per repository — but it is why run 1 is a `workflow_dispatch`.
   - Unblocking chosen by the user: **grant the new `pz-akash` Write access to both existing packages** rather than delete them. The alternative recreates the packages under the new repository, where GHCR creates them *private*, and an Akash SDL carries no registry credentials — a private image is not an error at deploy time, it is a lease that never pulls. It would also discard v1's images (~50 server tags, ~40 controller), which is a smaller loss than it sounds since both `-proto` repositories still build, but a loss for nothing.
   - Both packages are public today and shared between v1 and v2. That is load-bearing: `image:` in `config.yaml` has no credential field to pair with it.
10. Point the GitHub webhook at the new controller.
11. Deploy the new controller; verify the state branches initialise; push `triggers/start` for a **fresh world** (no `restore_target` — see item 1).
12. Verify: player count nonzero with someone connected; halt does not loop; no JSON errors; backup downloadable.

---

## 9. Defaults I am assuming (veto at approval)

1. Git layout = 3 branches (§2). Alternative `layout: single`.
2. SSH removed; RCON off by default (§3).
3. Trigger files move to `triggers/`, keeping the names `start`/`backup`/`halt`/`stop_at`, plus a new `restore`.
4. Dashboard ported at feature parity — no redesign, no new features.
5. IP lease retained (`server.ip_lease: true`), though with SSH+RCON gone only 2 UDP ports need it.
6. Single world / single server. No multi-instance support.
7. Cloudflare integration retained as-is.
8. The new system tolerates garbage left behind by the old one (repair-on-read), since the bash keeps corrupting state until Step 9.
