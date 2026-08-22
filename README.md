# Project Zomboid on Akash

A self-hosted, self-healing Project Zomboid dedicated server that lives on the
[Akash Network](https://akash.network), pays for itself by the block, and puts every
knob it has in one file.

One Go program, `pzctl`, is the whole system. It builds into two images from one
`docker/Dockerfile`, and which half runs is a subcommand:

| | |
| :--- | :--- |
| `pzctl controller` | The long-lived deployment. Serves the dashboard and the file store, orchestrates game-server deployments on Akash, keeps the backups, owns the DNS records. |
| `pzctl agent` | Runs *inside* the game container beside Project Zomboid. Fetches the server files, renders the `.ini`, launches the game, measures the player count, takes the backups, and stops when told to. |

Everything else the binary does is an operator tool — `pzctl config validate`,
`pzctl sdl render`, `pzctl akash leases`, `pzctl dns sync`. Run `pzctl help` for the
full surface.

This is v2, a ground-up rewrite. v1 was ~4,000 lines of bash and Python and is
preserved unchanged in [`pz-akash-proto`](https://github.com/hrkcz001/pz-akash-proto);
its four standing bugs and the reasons they cannot recur here are at the end of this
file.

---

## Configuration: one file, no literals

Everything that could be a hardcoded value is in **`config.yaml`, in the `pz-saves`
repository** — ports, image tags, timeouts, escrow limits, bid ceilings, retention
counts, the Cloudflare zone, the game's `.ini` values, the JVM heap. The Akash SDLs
are *rendered* from it rather than committed, so there is no second copy of a port
number to forget:

```sh
pzctl config validate -c config.yaml    # every problem at once, not the first
pzctl config dump                       # the effective config, defaults filled in
pzctl sdl render server                 # what would actually be deployed
```

CI validates both copies — the draft in `pzctl/config.yaml` and the authoritative one
in `pz-saves` — and refuses to build an image if either is wrong. A controller cannot
start without a readable `config.yaml`, so this gate is the one that matters most.

## Secrets: ten variables, none in git

Secrets are the only thing config.yaml does not hold. They reach a container through
its Akash manifest as environment variables, and `pzctl config secrets` lists them.

| Variable | Held by | What it is |
| :--- | :--- | :--- |
| `PZ_DEPLOY_KEY_B64` | both | The `pz-saves` deploy key, write-enabled — each side pushes its own state branch. |
| `PZ_AKASH_API_KEY` | controller | Akash console API. This is the one that spends money. |
| `PZ_WEBHOOK_SECRET` | controller | Verifies GitHub's push webhook. |
| `PZ_CLOUDFLARE_API_TOKEN` | controller | Edits the zone's records. |
| `PZ_STORAGE_PASSWORD` | controller | Fallback credential for both HTTP realms. |
| `PZ_SERVER_FILES_PASSWORD` | both | The `server.zip` realm. |
| `PZ_BACKUPS_PASSWORD` | both | The backups realm: list, download, upload. |
| `PZ_RCON_PASSWORD` | controller | Substituted into the game's `.ini`. |
| `PZ_ADMIN_PASSWORD` | controller | Substituted into the `.ini`; the agent passes it to the game as `-adminpassword`. |
| `PZ_JOIN_PASSWORD` | controller | Substituted into the `.ini`. What players type. |

The three game passwords are worth a note, because this is where v1 leaked. They are
committed to `pz-saves` as the placeholders `__RCON_PASSWORD__`, `__ADMIN_PASSWORD__`
and `__JOIN_PASSWORD__`, and the controller substitutes the real values into the
response body **as it serves `server.zip`** — over TLS, behind the server-files
credential. They never touch an image layer, a git object, or the game container's
Akash manifest. `docker/check_image.sh` fails the build if a literal password appears
in a built archive, so a regression is a red CI run rather than a discovery.

Project Zomboid has no `AdminPassword` `.ini` key at all — the admin account lives in
the world's save database, and the only door is the `-adminpassword` command-line flag.
So that one value makes an extra hop: the agent reads it back out of the substituted
`.ini` at boot and puts it on the game's command line. It is therefore visible in that
container's process list, which is the trade — a manifest is readable by the provider,
`/proc` needs a shell the image does not ship.

---

## Git is the bus, and every branch has one writer

The controller and the agent never call each other to agree on state. They publish
documents to `pz-saves` and read each other's, and the reason it works is that no
branch has two writers:

| Branch | Written by | Holds |
| :--- | :--- | :--- |
| `main` | **you** | `config.yaml`, mod lists, the `.ini`s, `triggers/`, the player guide. Neither program writes here. |
| `state/controller` | controller | Intent, status, the URLs it was assigned, the backups index. |
| `state/agent` | agent | Phase, player count, restart count, the last error, the backup it just finished. |

That is the whole of the concurrency design, and it is deliberately boring. v1 had both
processes committing to `main`, so every state change was a potential conflict, a
force-push, or a half-written file another reader picked up mid-write.

### Triggers

Operator commands are files under `triggers/` on `main`. Push one; the controller
consumes it and deletes it in the same commit that acts on it.

| File | Effect |
| :--- | :--- |
| `triggers/start` | Deploy a game server and bring the world up. |
| `triggers/halt` | Back up, stop the world, close the deployment, stop the billing. |
| `triggers/backup` | Take a backup now, without interrupting play. |
| `triggers/stop_at` | An epoch or `YYYY-MM-DD HH:MM` — halt at that time, topping up the escrow until then. |
| `triggers/restore` | Boot the world from a named archive instead of the one on disk. |

A GitHub webhook makes this immediate; the controller also polls, so a missed webhook
costs a poll interval rather than the command.

## The HTTP surface

The controller serves the dashboard and the file store on `controller.http_port`
(8000), fronted by Cloudflare on the configured domain.

| | |
| :--- | :--- |
| Public | `/` (dashboard, RU/EN), `/common.zip`, `/client.zip`, `/game.torrent`, `/healthz` |
| Server-files realm | `/server.zip` |
| Backups realm | `/backups.json`, `/backups/<name>` — `GET` to download, `PUT` to upload |

Uploads and downloads stream. v1 read the whole archive into memory on both sides,
which is a 2 GB spike in a container sized for a web page.

## Timestamps are Prague, always

Every timestamp — backup names included — comes from `identity.timezone` and never from
the machine clock. The containers run UTC and the zone database is embedded in the
binary, so `backup_20260822_090621.zip` means 09:06 in Prague no matter which
provider's cluster produced it.

## Backups

The agent asks the game to save, waits for the confirmation, zips `Saves/`, and streams
it to the controller, which verifies the digest and indexes it. One runs on a timer
(`backups.interval`, hourly), one on every halt, and one whenever you push
`triggers/backup`. Retention is both an age and a count (`retention_days: 7`,
`retention_count: 24` — an archive is deleted when it fails either). Storage is the
controller's own 50 GiB volume, which is ephemeral in the Akash sense: if that
deployment closes, the archives go with it. Downloading from `/backups/` is the only
durability there is, and the dashboard nags about it once the disk passes
`disk_warn_percent`.

`Server/` is never in an archive. That is the directory the substituted passwords live
in, and this is what keeps them from riding a backup out of the cluster.

---

## The four v1 bugs

The rewrite exists because of these. Each is a test in `pzctl/internal/agent`, not a
claim.

**1. The player count was always 0.** v1's parser returned `0` for a line it did not
recognise, and PZ's answer format varies. v2 returns *no answer* instead: an
unrecognised line leaves the count unknown, and the dashboard shows the count only when
something really measured it. The agent asks the game on a timer and reads the reply off
its stdout.

**2. Halt looped.** v1's container entrypoint exited when the game did. Akash restarted
the pod, the fresh container brought the world back up mid-halt, the controller read
that as a flap and answered with another halt — another webhook, another backup, on and
on. In v2 the agent is the process the container runs; the game is its child. When a
halt finishes, the agent *parks*: still running, still reporting, world down, until the
deployment is closed or told to start again.

**3. `Error reading server_info.json: Expecting value: line 1 column 106`.** Two writers
on one branch, one of them mid-write. v2 has one writer per branch, typed documents on
both sides, and atomic file replacement — a reader sees the old document or the new one.

**4. Requested backups sometimes never arrived.** v1 could not tell which backup
answered which request, so a stale archive could sign off a halt. Every request in v2
carries an id; the archive is uploaded with that id in a header and the report names it
back, along with the size and digest the controller verified. An unmatched report is not
an answer.

---

## Repository layout

```text
pzctl/                  the Go program: cmd/pzctl + internal/{config,gitbus,state,fsm,
                        httpapi,agent,akash,dns,sdl,dashboard,secrets,denom}
docker/                 one Dockerfile, two targets; check_image.sh is the gate
.github/workflows/      images.yml — test, then build, then check, then push
scratch/                gate scripts, plans and step reports from the rewrite
```

`pz-saves` (private) holds `config.yaml`, the mod lists, the world's `.ini` and Lua
files, `triggers/`, and the player guide the dashboard renders.

## CI

`images.yml` runs `gofmt`, `go vet`, `go test -race` and both config validations first;
only then does it build. Each image is built, loaded, and put through
`docker/check_image.sh` — which asserts the container does not run as root, has no
`sshd`/`sudo`/`gosu`, has no secret-shaped environment variable or credential file in a
layer, and carries placeholders rather than passwords — and only then pushed. Every
image is tagged `sha-<short>`, which is what `config.yaml` pins.

## Operating it

```sh
pzctl akash leases                  # anything still billing? run this after any crash
pzctl akash escrow --dseq DSEQ      # what is left to spend
pzctl akash close  --dseq DSEQ      # stop the billing, refund the rest
pzctl dns sync --controller URL     # point the apex at a new controller
pzctl state show                    # what both sides currently believe
```

`pzctl akash` spends real money. A deploy that fails partway still prints its dseq,
because that number is the only way to close what it funded.

