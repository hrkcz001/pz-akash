# Step 8 — one Go build, two images, and a gate that can say no

**Deliverable (PLAN §8):** CI: one Go build → two images, `config validate` gate, no
secrets in layers. **Gate:** green workflow.

**Status:** green. `.github/workflows/images.yml` run `32528642006` passed all three
jobs on commit `26bc221`, both images were gated and pushed to ghcr, and the local gate
`scratch/gate8.ps1` passes. Details and the two things the run taught me are in §7.

---

## 1. What step 8 turned out to include

The plan's line is about CI, but two things had to come first, and both were holes
rather than polish.

**The bootstrap had no tests.** `internal/bootstrap` was written at the end of step 7
to close the chicken-and-egg the rendered SDL had documented and not solved: the
manifest sets `PZ_REPO_URL` and friends, and nothing read them, so a container built
from that SDL would die in `config.Find` before doing anything. The package existed;
its promise — "the embedded host keys match `config.yaml`" — was a comment. It is now
`TestEmbeddedKnownHostsMatchTheConfig`, and nine other tests.

**Nothing called it.** `pzctl controller` and `pzctl agent` still went straight to
`config.Find`. `cmd/pzctl/boot.go` is the wiring, and the interesting part is the
resolution order (§3).

So step 8 is three things: finish the boot path, build the images, gate them.

---

## 2. `internal/bootstrap`, tested

Ten test functions, 26 s (they drive real `git` and real go-git fetches over the file
transport). The three that carry weight:

`TestEmbeddedKnownHostsMatchTheConfig` — the drift pin. The bootstrap's trust anchor
cannot come from the file it is fetching, so github.com's host keys are embedded in
the binary; `config.yaml` also carries them, for the bus that runs afterwards. Two
copies of a fact is a bug waiting for a key rotation, so the test compares them and
asserts three host key lines. Rotate one and the build goes red instead of the
deployment going quiet.

`TestFetchWarmsTheMirrorTheBusReuses` — `MirrorDir` is deliberately the same bare
repository the controller's `git.cache_dir` (or the agent's
`agent.paths.repo_cache`) opens seconds later. The test fetches, then opens
`gitbus` on that directory and reads a file **with no `Fetch` call**. If someone
later gives the bootstrap its own scratch directory, this fails — which is the point:
the boot clone is a warm cache, not a throwaway.

`TestFetchErrors` — seven cases, each also asserting that **no file was written**. A
partial `config.yaml` from a failed fetch is worse than none: the fallback path (§3)
would load it. The messages name what to fix — a missing deploy key produces
`{secrets.DeployKeyEnv, "config.yaml", "pz-saves"}`, not "authentication failed".

---

## 3. `bootConfig`: one authority, chosen by the environment

```go
named := explicit != "" || strings.TrimSpace(os.Getenv("PZ_CONFIG")) != ""
if named || !bootstrap.Configured() { return config.Find(explicit) }
```

Three rules, in order:

1. **A named file wins and is never overwritten.** `-c` or `$PZ_CONFIG` means someone
   is pointing at a specific file, including a local edit they are testing. Fetching
   over it would be data loss.
2. **No `$PZ_REPO_URL` means a workstation.** Plain `config.Find`. Nothing fetches
   anything; `go run ./cmd/pzctl` behaves the way it did yesterday.
3. **`$PZ_REPO_URL` set means always fetch.** Not "fetch if the file is missing": a
   leftover `config.yaml` from a previous boot would pin a container to its first
   boot's configuration, and the whole point of git-as-the-bus is that a push changes
   what the next boot does.

Rule 3 has a deliberate fallback. If the fetch fails and a file from a previous boot
is on disk, the controller logs two `WARNING:` lines and continues with it. The
reasoning is money: a lease is already funded, and a controller that cannot reach
GitHub can still serve the dashboard, the downloads and the backup index. A hard exit
there turns a GitHub outage into a dead domain. When the fetch fails *and* there is no
file, the error reported is the **fetch** error, not `config.Find`'s "no config file
found" — the second is a symptom of the first and only the first says what to fix.

An earlier draft of this file justified the order with "the working directory is
tmpfs and empty on every container start, while the mirror is the persistent volume."
Reading `internal/sdl/templates/service.sdl.tmpl` killed it: the SDL renders
`storage: - size:` with **no `persistent: true` and no mount**. Akash storage in these
deployments is the container filesystem, reset from the image whenever a container
restarts. The rules above rely on nothing about where files live.

---

## 4. The Dockerfile

One file, five build stages, two targets.

| stage | what it is for |
|---|---|
| `pzctl` | `CGO_ENABLED=0` static build, then `RUN /out/pzctl version` |
| `steamcmd` | the 32-bit SteamCMD install, shared |
| `packages` | `build_packages.py` + the dashboard's two extras |
| `game` | app 380870, `validate` |
| `controller` / `server` | the two images |

**One Go build for both images.** v1 had `controller.sh`, `storage_server.py`,
`webhook.py`, `state.sh`, `schedule.sh`, `trigger.sh`, `deploy.sh`, `rcon.py`,
`update_cloudflare.py`, `entrypoint.sh` and `sdl.template` copied into two images that
had to agree about JSON field names by hand. Now there is one binary, so a disagreement
is a compile error. The GHA cache makes it a single compile across both jobs in
practice.

**`RUN /out/pzctl version`** — a binary that cannot print its own version has no
business being copied into an image. Cheapest possible smoke test, runs every build.

**No `ENTRYPOINT` in either image; `CMD ["pzctl", "agent"]` / `CMD ["pzctl",
"controller"]`.** This is bug 2 in one line of Dockerfile. v1's `entrypoint.sh` ended
with the server command, so when the server exited the entrypoint returned, and Akash
restarted the container instead of letting it die — which the controller read as a
crash-loop and answered with status flapping, webhook spam and duplicate backups. In
v2 the agent **is** PID 1: when it decides to stop, the container stops.

Explicit CMDs rather than `exec pzctl "$PZ_ROLE"` because a wrong `PZ_ROLE` should not
be able to start the wrong program, and because `docker inspect` then says what an
image is. `PZ_ROLE` stays in the manifest as documentation; the comment in
`internal/sdl/render.go` that claimed the entrypoint read it was fixed in this step.

**`ENV HOME=/home/steam` is load-bearing.** Docker does not read `/etc/passwd` for
`USER`, `agent/pz.go` hands the launcher `os.Environ()`, and PZ resolves its world
path from `HOME`. Unset, the save goes to `/Zomboid` — not `agent.paths.data_dir`, and
not writable. v1 got this by way of gosu.

**Neither image declares `EXPOSE`.** Ports live in `config.yaml` and reach Akash
through the rendered SDL. An `EXPOSE` line would be a second copy that no validator
covers and that would eventually be wrong.

**The server image installs one package: `ca-certificates`.** v1 installed eleven —
sshd, openssh-client, sudo, gosu, python3, zip, jq, git, dos2unix among them — because
backups left the container over scp. In v2 the agent owns its own uploads, so bug 4's
whole failure surface is gone by construction rather than by fix. The controller is
also `ca-certificates` only: go-git speaks SSH itself, which incidentally removes the
`git reset --hard` that used to delete files out from under a running backup.

`docker/build_packages.py` is a **byte copy** of v1's, now with a provenance header.
It is build-time SteamCMD wrangling that works, none of it runs where players are
served, and rewriting it in Go would risk the mod lists for no runtime benefit. The
copy exists because `pz-controller/` is deleted at cutover.

---

## 5. The workflow

`.github/workflows/images.yml`, one file, three jobs. v1 had two workflows and **no
test job at all**: a `controller.sh` with a syntax error published an image, and the
first sign of trouble was a dead deployment.

```
check ──┬── controller image ── check_image.sh ── push
        └── server image     ── check_image.sh ── push
```

`check` is gofmt (a gate, not a suggestion), `go vet`, `go test -race ./...`, and
`pzctl config validate -c config.yaml`. The validate belongs in the gate rather than in
an operator's memory: `config.yaml` is the first thing every container reads, and a
schema change that forgot the config produces two containers that boot and immediately
exit.

Both image jobs `needs: check`, so a failing test cannot produce a pushed tag.

**Build, then check, then push — three steps, in that order.** The first draft had
`push: ${{ github.event_name != 'pull_request' }}` on the build action, which means the
layers are already in the registry by the time the gate could object. Now both builds
are `load: true, push: false`, `check_image.sh` runs against the image that would be
published, and a separate step (`echo "$TAGS" | xargs -n1 docker push`, guarded by
`github.event_name != 'pull_request'`) publishes it. A gate that runs after the push is
a report, not a gate.

The controller job does a second `actions/checkout` of `hrkcz001/pz-saves` with
`secrets.PZ_SAVES_SSH_KEY` into `./pz-saves` — the mod lists, the server inis and the
dashboard's extras. It is the only secret this workflow uses, and it is the same deploy
key v1 used (the locked decision: reuse everything, just move it out of git). It then
validates `pz-saves/config.yaml` if that file exists, and says "pre-cutover; skipping"
if it does not, so the build does not go red for a cutover that has not happened.

The server job frees ~15 GB (`dotnet`, `android`, `ghc`) before the ~3 GB game
download. v1 had the same problem; it is spelled out here because a build that fails
for lack of space reads as a broken Dockerfile.

`.dockerignore` matters more than usual: the context is the repository root and it is
uploaded whole. `**/deployment.yaml` is the first line — v1's rendered manifests hold
the live deploy key, the Akash key and the game passwords, and a build context is
exactly the sort of place they would leak from. `pz-saves/` is deliberately **not**
excluded (the packages stage reads it) and is gitignored at the root instead, so CI's
checkout cannot be committed by accident.

---

## 6. `docker/check_image.sh` — the "no secrets in layers" half

`check_image.sh ROLE IMAGE`, run by CI against the loaded image and equally runnable
by hand. Every assertion is a property v1 got wrong at least once.

Both roles:

- `Config.User` is `pzctl` / `steam`, not root; `Config.Cmd` is the right one;
  `Config.Entrypoint` is empty; `/usr/local/bin/pzctl` is executable.
- No secret-shaped variable in `Config.Env`, and none in `docker history`. Failures
  print **names only** (`cut -d= -f1`) — this runs in a public build log, so the check
  for a leaked secret must not be the leak.
- A `find / -xdev` sweep for `id_*`, `*.pem`, `.git-credentials`, `deployment.yaml`,
  `*/.ssh/*`.

Controller: `/data/packages` holds `packages_manifest.json`, `common.zip`,
`server.zip`; the two dashboard extras are noted, not required (an absent
`game.torrent` turns that card off by design); `/data`, `/data/repo` and
`/data/backups` are writable **by pzctl**, checked through `su` rather than as root,
because a controller that cannot write its mirror fails at the first fetch, minutes
into a lease that is already costing money.

Server: no `/usr/sbin/sshd`, no gosu, no sudo, no `/run/sshd`;
`start-server.sh` present and executable under the game directory (a config naming a
launcher the image lacks is a boot failure the agent can only report — this is the one
place the two can be compared before a deployment); `HOME=/home/steam`.

**One check is a warning on purpose.** The game passwords must reach the ini as
`__RCON_PASSWORD__` / `__JOIN_PASSWORD__` and be substituted as the archive is served.
Today `pz-saves/server/Server/vsrania.ini` still carries the literal `RCONPassword`
and `Password` values v1 committed — I confirmed this by key name and value *length*
only, never printing them. So the check extracts `server.zip`, looks for non-placeholder
values, and emits `::warning::server.zip carries a literal RCONPassword` — naming the
key, never the value. Making it an error today would mean a red build for a known,
scheduled item; **PLAN step 9 item 5** now says to move the values and flip the `note`
to `fail`, so the regression becomes red the moment it can be.

**And one check exists because a warning could not fire.** The loop originally looked for
`RCONPassword` and `Password`, the two literals pz-saves committed. `AdminPassword` was
absent from it — and could not have been found by it, because the ini has no such key:
v1 passed that value as `ADMIN_PASSWORD` in the server SDL's environment. That is the
case worth catching, because a substituted placeholder and a missing key look identical
to a grep for literals — both produce silence — while a missing key means the controller
replaces nothing, PZ takes its own default, and the placeholder machinery reports success
having done nothing. The loop now covers all three keys and separately reports the ones it
never saw. Exercised under WSL against a synthetic ini in both shapes: a literal is still
counted, an empty value and a `__TOKEN__` still are not, and the counters survive the
process substitution the loop reads from (which is why it is `done < <(…)` and not a pipe).

---

## 7. What was actually verified, and what was not

`scratch/gate8.ps1` is the local half. It runs what CI's `check` job runs, then asserts
the facts a reader of the Dockerfile and the workflow would otherwise have to hold in
their head: both targets and their CMDs, no ENTRYPOINT, exactly **one** `go build`,
`CGO_ENABLED=0`, both `USER` lines and no stage ending as root, no `EXPOSE`,
`ENV HOME`, no `openssh-*`/gosu/sudo/dos2unix in any instruction, no secret-shaped
`ARG`; every `docker/*.sh` the workflow names exists; `needs: check` twice, `load: true`
twice, `push: false` twice, a `docker push` step per image; `.dockerignore` and
`.gitignore` agreeing about `pz-saves`; and `bash -n` on the gate script under WSL.

It also checks **the one seam nothing else covers**: `config.yaml`'s
`controller.image` and the server's `image` must equal
`ghcr.io/<origin slug>-controller` / `-server`, i.e. exactly what the workflow
publishes, and both `image_tag`s must have the `sha-<short>` shape
`docker/metadata-action` produces. A mismatch here is a funded lease pulling a tag that
does not exist, discovered at the worst possible moment. It passes today
(`hrkcz001/pz-akash`), and it will need re-running after the step 9 rename — which is
why the rename is now item 6 of that checklist.

Writing the gate found three real defects: `in()` is a **bash reserved word**, so
`check_image.sh` was a syntax error on its first line of logic (`bash -n` caught it);
the workflow invoked `docker/inspect_image.sh` while the file was written as
`check_image.sh`; and `check_image.sh` was not executable in the git index, which on a
Windows checkout is silent until CI tries to run it (`git add --chmod=+x`, and the gate
now asserts mode `100755`).

The first run found a fourth, and something worse than a defect. The workflow named
`secrets.PZ_SAVES_SSH_KEY`, which **does not exist** in this repository — `gh secret
list` has `SSH_PRIVATE_KEY` (v1's, and v1's workflow accepted either name). The
checkout succeeded anyway, and the reason it succeeded is the finding:

> **`hrkcz001/pz-saves` is a public repository.**

So the literal `RCONPassword` and `Password` in `server/Server/vsrania.ini` are not
"committed secrets" in the mild sense — they are readable by anyone, right now, and
have been for as long as they have been there. The deploy key being public was a known
and accepted cost (the locked decision: reuse, do not rotate). These two are a
different thing: the join password is what stops strangers connecting, and the RCON
password is remote administration. **PLAN step 9 item 5 now recommends rotating both
at cutover**, with the tradeoff spelled out, and asks whether the new pz-saves should
be public — v2 does not care either way, because with placeholders in the ini there is
nothing secret left in that repository.

The workflow now uses `${{ secrets.SSH_PRIVATE_KEY || secrets.PZ_SAVES_SSH_KEY }}` and
follows the checkout with a step that fails saying *which secret is missing*, since an
empty key makes checkout fall back to the job token and the resulting error blames the
wrong repository. `gate8.ps1` asserts both. The "private saves" wording in the
Dockerfile, `.dockerignore` and `.gitignore` was factually wrong and is fixed.

Results:

- `pwsh scratch/gate8.ps1` — **all checks pass** (48 assertions + 5 steps).
- `wsl -d Debian -u root -- bash scratch/wsl-test.sh 1` — **exit 0, 0 races, 0 failed
  tests** across the whole module.
- **The workflow is green** (run `32528642006`, commit `26bc221`): `check` 1m49s,
  controller image 4m47s, server image 29m35s. A second run on the follow-up commit
  (`32531909591`) is also green and much faster — `check` 23s, controller 5m12s, server
  **16m36s**, the difference being the `type=gha` cache holding the game download.
- **A third run** (`32533345486`, commit `5a95f3f`) is green on the four `docker/*`
  actions bumped to their Node 24 majors — `setup-buildx@v4`, `login@v4`, `metadata@v6`,
  `build-push@v7`; `check` 20s, controller 8m12s, server 19m33s (start-to-finish per the
  API; `gh run watch` reports the server job as 18m33s, which excludes its queue wait).
  Its annotation list is
  now exactly the two designed password warnings, with the Node 20 deprecation notice
  gone. `build-push-action` v7.0.0's only removals are `DOCKER_BUILD_NO_SUMMARY` and
  `DOCKER_BUILD_EXPORT_RETENTION_DAYS`, neither of which this workflow sets, so the bump
  was mechanical.
- **A fourth run** (`32534673374`, commit `bef8263`) proves the widened password check in
  CI rather than only under WSL — `check` 26s, controller 6m20s, server 17m07s, all green,
  and the controller job prints the line the old loop could not:

  ```
  note: 2 literal password(s) in server.zip — a step 9 (cutover) item
  note: AdminPassword absent from server.zip's ini — nothing for the controller to substitute
  controller: all checks passed
  ```

  A note, not a failure: the absence is pz-saves' current state, and step 9 item 5 is
  what changes it.

Both images built, passed `check_image.sh`, and were pushed:

```
ghcr.io/hrkcz001/pz-akash-controller:sha-26bc221  sha256:34d8dc7e…  (also :v2-pzctl)
ghcr.io/hrkcz001/pz-akash-server:sha-26bc221      sha256:262e0007…  (also :v2-pzctl)
```

The controller gate printed 16 `ok:` lines — runs as `pzctl`, `CMD [pzctl controller]`,
no entrypoint, `pzctl` executable, no secret-shaped env, no secret-shaped build args,
no key-shaped files, all three packages plus both dashboard extras present, all three
directories writable **by pzctl** — and then the two warnings it was designed to
produce:

```
##[warning]server.zip carries a literal RCONPassword
##[warning]server.zip carries a literal Password
note: 2 literal password(s) in server.zip — a step 9 (cutover) item
```

That is the finding of §7 below confirmed from the shipped archive rather than from the
working tree. The server gate printed 14 `ok:` lines including the five absences — no
`sshd`, no gosu (either path), no sudo, no `/run/sshd` — `start-server.sh` executable,
and `HOME=/home/steam`.

So the claim "no secrets in layers" is now measured, not asserted, with one known
exception that has a scheduled fix and a check that names it every build.

Two costs the run made concrete. The server job is **29m35s**, nearly all of it the
SteamCMD download and then `load: true` moving a ~4 GB image into the daemon; the
controller is under five minutes because the Go stage is a cache hit from the other
job. And `check` at 1m49s means the cheap gate really is cheap, which is the ordering
argument working.

Not verified locally, and I will not claim otherwise: **nothing was built on this
machine.** The Docker daemon here is not running (`npipe:////./pipe/docker_engine`
unavailable) and the WSL Debian has no container builder, so every image fact above
comes from CI. That is what the gate is for; it is also why the question in §8 about a
local builder is worth answering — a 30-minute round trip for a one-line Dockerfile
change is the wrong loop.

---

## 8. Still open

Carried from step 7 §11, unchanged, and not blocking:

1. **The retention arithmetic.** `retention_count: 24` × `upload_max_bytes: 2 GiB`
   permits 48 GiB on a 20 GiB volume that also holds `packages_dir` and must keep
   `min_free_bytes: 2 GiB` free — the count rule can never bind. ~8 is the honest
   number.
2. ~~**`PZ_ADMIN_PASSWORD` has no delivery channel**~~ — **resolved, and it was already
   resolved when this was written.** Step 6 built the channel: `httpapi.Substituter` maps
   `__ADMIN_PASSWORD__` to `secrets.Set.AdminPassword`, loaded from `PZ_ADMIN_PASSWORD` on
   the controller only (`internal/secrets/secrets.go:106`), wired at
   `internal/httpapi/server.go:92` and selected per-file at `handlers.go:44`;
   `internal/sdl/render_test.go:267` asserts the variable reaches the controller SDL. The
   comment at `internal/agent/boot.go:289-292` is accurate. What is actually missing is one
   line in pz-saves: `server/Server/vsrania.ini` has no `AdminPassword` key at all, so
   there is no placeholder for the substituter to hit. v1 passed the value as
   `ADMIN_PASSWORD` in the server SDL's environment instead — which is exactly the
   arrangement that comment describes as v1's. Step 9 item 5 now says to *add*
   `AdminPassword=__ADMIN_PASSWORD__`, not to replace a literal.
3. **The downloaded/not-downloaded tag** on the dashboard: keep or drop (step 7 §6.11).

New, noticed while writing this and not urgent: both runtime images are
`debian:bookworm-slim`, which is oldstable — the IDE's scanner reports 2 critical and 2
high CVEs in that base. The builder (`golang:1.25-bookworm`, 3 critical / 9 high) does
not ship: `CGO_ENABLED=0` means nothing from it reaches an image but the binary.
Moving the runtime to `debian:trixie-slim` would clear most of it, and it is not a
one-line change to make blind — SteamCMD's i386 libraries and the JRE Project Zomboid
ships have to work there. Worth doing after cutover, on a branch, with the gate to
prove it.

New, and worth an answer before step 9 rather than after: **is there a local image
builder I may use?** Right now every image iteration is a push and a CI round trip,
which is slow and burns Actions minutes on things `bash -n` could have caught. Starting
Docker Desktop is the zero-install option; a podman install in the WSL Debian is the
other. I am not going to work around it — the question is yours to answer.

New, and the one item here that is a decision rather than a cleanup: **the full inventory
of publicly-readable live credentials is four values, three of them distinct.** The image
gate's two warnings name the ini's `RCONPassword` and `Password`; closing them out led to
pz-saves' tracked `deployment.yaml`, which is v1's server SDL and carries `ADMIN_PASSWORD`
and `STORAGE_PASSWORD` as plain env entries in the same public repository. `RCONPassword`
and `STORAGE_PASSWORD` are one string doing two jobs, so they rotate together. Two of the
three were also echoed into this session's terminal output by a mistake of mine — a helper
function I named `H`, which is PowerShell's alias for `Get-History`, so the binding error
printed the argument it could not convert. No attacker gained anything they could not
already `curl` from a public repository, but it does put the values in a scrollback and a
transcript as well. The rotation recommendation in PLAN step 9 item 5 is therefore no
longer balanced against "reuse costs nothing": rotate all three at cutover.
