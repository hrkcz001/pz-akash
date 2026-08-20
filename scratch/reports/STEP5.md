# Step 5 report — Akash: deploy, bid, lease, close, top up; Cloudflare: the zone

**Scope, from the plan's step table:**

> `internal/akash`: deploy, bids, lease, watchdog, close, escrow top-up. `internal/dns` Cloudflare
>
> **Gate:** First real deploy on a throwaway dseq

**Status: complete. The gate ran against the real Akash network and the real Cloudflare zone.** Three throwaway deployments were created, leased and closed; one dedicated IP was assigned, published as a real DNS record, resolved from five resolvers, and removed. The wallet is back to holding only v1's two live deployments, and the zone is back to exactly the records it had before.

The gate earned its keep three times over. It found **three defects that no fake could have modelled**, because all three are facts about how the network and the API actually behave rather than how they are documented. Two were wire-shape bugs in my client; the third is a provider behaviour that turned out to require a product change — a retry loop in the FSM with its own config key.

---

## 1. What these two packages are

`internal/akash` is v1's `deploy_server.sh` plus the Akash half of `pz_controller.py`, which between them shelled out to `curl` and `akash` and parsed the results with `jq`. `internal/dns` is `update_cloudflare.py`.

Neither is allowed to be clever. `internal/akash` owns one question — *is there a lease, and where does it answer?* — and deliberately does **not** own the decision to close one: `run` hands the dseq up to the FSM even on failure, and the FSM closes it. That split is invariant **I1** (never two leases on one world) made structural: one component decides that a lease should exist, and it is the same component that decides one should stop existing.

`internal/dns` answers two unrelated questions that v1 conflated:

| | Where is the dashboard? | Where is the game server? |
|---|---|---|
| Record | apex + `www` | `dns.game_record` (`pz`) |
| Type | proxied CNAME at Cloudflare | **DNS-only A** at the lease's dedicated IP |
| Written by | CI or an operator, `pzctl dns sync --controller` | the controller, on every deploy |
| Precedent | v1's `update_cloudflare.py` | **new in v2** |

The game record is new because v1 had no answer at all: it published a fresh IP to the dashboard after every redeploy and players had to go and read it. The record must never be proxied — Cloudflare's proxy carries HTTP, PZ does not, and a proxied name resolves to Cloudflare's own addresses, so proxying it would send every player to Cloudflare instead of to the server. That is not a setting the package exposes; `dns.proxied` governs the controller records only, and validation refuses `game_record: www` while `include_www` is on.

## 2. The fifth v1 bug: the denomination

Four bugs were reported. This is a fifth, found by reading rather than by running, and it is the one that would have stopped the system dead.

Console-managed wallets hold **`uact`**: a credit pegged at 1e6 to the dollar. v1's SDL declared its bid ceiling in **`uakt`** and then compared incoming `uact` bids against it as though they were micro-AKT.

At AKT ≈ \$1 the two are numerically close and nothing looks wrong. At AKT ≈ \$5 every bid on the network appears five times more expensive than it is, no bid clears the ceiling, and the deploy fails with *"no bids found"* — a message that sends you looking at the market rather than at your own arithmetic.

v2 makes the denomination a config key with a validated set, converts through USD, and puts the reasoning in the file:

```yaml
denom: uact
allowed_denoms: [uact, uakt]
```

The consequence worth stating: **because `uact` is dollar-pegged, the price oracle is not on the critical path.** It is needed only for AKT-denominated bids, and validation demands a rate source (`price_oracle_url` or `akt_usd_fallback`) only when `denom: uakt` is set. v1 required CoinGecko to be reachable in order to deploy anything at all.

## 3. Three live discoveries

### 3.1 `data` is an object, not an array

`GET /v1/deployments` returns

```json
{"deployments": [ … ], "pagination": {"total": …, "skip": …, "limit": …, "hasMore": …}}
```

I had written the client against a paginated array. It decoded into nothing without erroring — an empty deployment list, which reads exactly like a clean wallet. **The failure mode is the dangerous direction:** adoption would have found nothing and the controller would have deployed a second server over a live world. That is invariant I1 broken by a JSON shape.

### 3.2 The list endpoint pairs each deployment with the wrong lease

With two deployments on one wallet, each entry's `leases` array described **the other one** — same `gseq`/`oseq`, the neighbour's `dseq`. I have not established whether this is a bug or an intended aggregate, and it does not matter: anything that needs a lease now calls `GET /v1/deployments/{dseq}`, which is correct. The list endpoint is used only to enumerate `dseq`s.

Had this shipped, adoption would have attached the controller to the game server's lease and then closed it on the next halt.

### 3.3 A provider can win the bid, take the lease, and never assign the IP

This is the one that changed the product.

The SDL asks for a dedicated IP (`ip_lease: true`). The placement filter only bids on providers whose attributes say they can supply one. Run 1 went to the *nearest eligible provider*, which:

- won the bid,
- accepted the lease — status `active`, `available=1`, `ready=1`,
- and then returned `ips: {}`, `uris: []`, and instead:

```json
"forwarded_ports": {"pz-server": [
  {"host": "provider.europlots.com", "port": 16261, "externalPort": 31336, "proto": "UDP"},
  {"host": "provider.europlots.com", "port": 16262, "externalPort": 31476, "proto": "UDP"}
]}
```

Shared-host port forwarding. Every field says success. There is no dedicated IP, the ports are not the ports players were told, and a PZ client cannot be pointed at it.

This is exactly the `uris` / `forwarded_ports` / `ips` confusion v1 lived inside — now confirmed as **real provider behaviour**, not a documentation ambiguity. v2 treats a lease with no address in `ips` as not routable, times out, and skip-lists the provider for `skip_ttl` (24h).

Which exposed the product gap. `run` hands the dseq up without closing; `onDeployResult` closes it; and `onCloseResult` then set intent to `stopped`. So **one bad provider turned every `start` into an offline server needing a human to press start again.**

The obvious fix — leave intent `running` and let `advance` redeploy — is wrong, and a comment already in the tree said why:

> The agent reads intent as a level; the controller reads triggers as edges.

That asymmetry is deliberate: it is what stops a failing provider from producing a redeploy loop, which is v1's bug 2 with a different trigger. So the retry is an explicit **counted edge**, with three properties, each tested:

1. It fires only after a **successful close**, so I1 holds — there is never a second deployment while the first still exists.
2. It **yields to a halt**. The retry is ours; a halt is the operator's, and the operator outranks us.
3. It is bounded by its **own** config key, not the existing one.

That last point is the interesting design call. `akash.max_attempts` is 15 because a close is one API call and abandoning a *billing* lease is far worse than retrying it. A deploy retry costs an escrow funded, a bid window waited out, and a lease-ready timeout — tens of minutes. One knob for both would mean either giving up on a close too early or churning deployments for hours, so:

```yaml
max_deploy_attempts: 4
```

In practice the budget is rarely what stops it: the driver skip-lists each provider that fails, so the network runs out of eligible providers before the counter runs out — which is both cheaper and a better error message than *"attempt 15 of 15"*.

### The market is thinner than the attribute suggests

Of four providers whose attributes claim dedicated IPs:

| Provider | 30-day uptime | On a `kind: ip` request |
|---|---|---|
| HR / europlots | 0.97 | leased, then **never assigned an IP** |
| BG / digitalfrontier | 0.999 | **did not bid at all** — it is already leasing an IP to v1's live server |
| PT / digitalfrontier | 0.961 | **delivered `213.58.173.240` within ~1 minute of leasing** |
| UA / proxyua | 0.981 | untested |

Two of the three tested did not produce a usable IP. The retry is not defensive programming for a hypothetical; it is the normal case.

## 4. The gate, part 1: three real deployments

Every run created a real deployment on the real network with real money, and every one was closed.

| Run | `placement.countries` | dseq | Outcome |
|---|---|---|---|
| 1 | full EU list | 1787265327895 | HR/europlots leased, then `ips: {}` + forwarded ports. Timed out at 10m, provider skip-listed for 24h, closed. |
| 2 | `[BG]` | 1787268468659 | **No acceptable bid** inside the 90s window: *"2 bid(s) seen; bid from a provider that failed the placement filter ×2"*. Closed. |
| 3 | `[PT]` | 1787268854921 | **Success.** Closed. |

Run 3's output is the line that closes out the Akash half:

```
akash: chose akash1hgulk6aekakqzc0v6wukrd3dy9n90f5gkl4ezk in PT: 70 uact/block = 1.0080 USD/day, 2801 km
akash: leased dseq 1787268854921 from akash1hgulk6aekakqzc0v6wukrd3dy9n90f5gkl4ezk
dseq      1787268854921
provider  akash1hgulk6aekakqzc0v6wukrd3dy9n90f5gkl4ezk (gseq 1, oseq 1)
price     70 uact/block = $0.0420/hour = $1.01/day
endpoint  213.58.173.240 game 16261 udp 16262 rcon 0
```

So **create → bids → placement filter → selection → lease → dedicated IP → close → escrow → adopt** are all proven against the real network, not against a fake. The selection line is worth reading twice: it reports the price in dollars per day *and* the distance from the reference point, which is what makes a placement decision reviewable after the fact. v1 logged neither.

`pzctl akash leases` afterwards claims only v1's game server — identification is by the service name `pz-server`, so the controller can never be adopted by mistake.

## 5. The gate, part 2: the real Cloudflare zone

The game record `pz.vsrania.online` had never existed, so this was its first write. `--game` was passed **alone**, deliberately: `dnsSync` runs the two halves independently, so the apex and `www` CNAMEs — which currently point at the **live v1 controller** — were never read, let alone written.

| Step | Result |
|---|---|
| `dns zones` | one zone, `vsrania.online`, active |
| `dns sync --game … --dry-run` | plan produced, **nothing written** |
| `dns sync --game 213.58.173.240` | `created A pz.vsrania.online -> 213.58.173.240 (dns-only, ttl 60)` |
| the same sync again | `unchanged` — and zero writes |
| authoritative NS (`julian`, `lily`) | `213.58.173.240` after **~10s** |
| `8.8.8.8`, `1.1.1.1`, `9.9.9.9` | `213.58.173.240` |
| `dns clear-game` | `deleted`; both authoritative NS `NXDOMAIN` |
| apex and `www`, after all of it | `188.114.96.9, 188.114.97.9` — untouched, still proxied |

Three things this established that the unit tests could not:

**The record is DNS-only in practice, not just in the request body.** The IP that comes back is the provider's, not Cloudflare's. A proxied record would have returned a `188.114.x` address here, the same as the apex does two rows down — so this table is the actual proof of the property the whole package is built around.

**Propagation is about ten seconds.** The first attempt queried `8.8.8.8` roughly two seconds after the create, got `NXDOMAIN`, and looked like a failure. It was not: polling the authoritative servers showed the record appearing at ~10s and every public resolver agreeing immediately after. This is harmless in production — the controller writes the record at deploy time and the game then takes minutes to boot — but it is exactly the kind of thing that gets misdiagnosed as a bug at 3 a.m., so it is written down here.

**`game_ttl: 60` is doing visible work.** After `clear-game`, `8.8.8.8` kept serving the deleted address while both authoritative servers said `NXDOMAIN` — the TTL, live, in front of me. That is precisely the window the config comment warns about: *"a resolver holding the old one sends players to a machine that is no longer ours until it expires."* At Cloudflare's default of 300 that window would be five minutes after every redeploy.

## 6. The defect the DNS gate found: a dry run that reported like a real one

`--dry-run` exists for one reason — it is the only safe way to point v2 at a zone v1 has been managing — and its entire value is that the report can be trusted. It printed this:

```
created A pz.vsrania.online -> 213.58.173.240 (dns-only, ttl 60)
```

Nothing was written; the only marker was a `cloudflare: DRY RUN would POST …` line on **stderr**. Redirect stderr away, or read the operator-facing stdout report on its own, and a rehearsal is indistinguishable from the real thing. This is the same class of defect as §3.1: a report that cannot be told apart from the state it reports on.

Fixed at the source rather than at the print site, because there is more than one printer — the command, the driver's log line, and eventually the dashboard:

```go
// Planned means the write was withheld because of Options.DryRun, so the Action
// is what would have happened rather than what did. It is carried on the change
// rather than handled by whoever prints one because there is more than one such
// printer, and a report of a dry run that reads exactly like a report of a real
// run is how an operator comes to believe a record exists.
Planned bool
```

Now: `would create`, `would update`, `would delete` — and `unchanged` stays `unchanged`, since a record that already matches would be left alone either way and *"would unchanged"* is not English. My first attempt got that wrong and a test caught it, which is the small pleasure of writing the test first.

`--dry-run` had **no test at all** before this — the flag existed and was unproven. It now has three, including the one that matters most: the plan a dry run produces must be the plan a real run would have made, verified against a zone that already holds a record. A dry run that says `unchanged` about a record it never compared is worse than no dry run, because it is evidence.

## 7. Four things v1's `update_cloudflare.py` got wrong

These are not live discoveries; they are what reading the Python against the Cloudflare API turns up. Each has a test named after the behaviour rather than after the function.

**It read the success flag and threw the answer away.** Cloudflare wraps every response in an envelope with its own `success` field, so a call can fail two ways: the status line, and the body. `upsert_dns_record` returned `False`, nobody looked at the return value, and the boot log said nothing. A zone could quietly stop being updated. v2 treats `HTTP 200` + `success: false` as an error carrying Cloudflare's own code and message, and does **not** retry it — the request was wrong, and repeating it buys the same answer for another unit of quota.

**It took whichever zone the token could see first.** Correct until the account holds two domains, and then it silently reconfigures the wrong one. `dns.zone_id` is now config; `pzctl dns zones` is how an operator finds the value.

**It emptied whole rulesets.** v1 deleted every rule in the origin phase. A zone this system does not exclusively own would lose rules nobody could recover. v2 replaces only rules carrying its own marker and leaves foreign ones alone — with a test that seeds someone else's rule and asserts it survives. Relatedly, v1 put the port in the rule *description*, so matching on the whole string would leave one rule per port ever deployed with whichever matched first deciding where traffic went; v2 matches on the marker prefix and replaces.

**It wrote to the zone on every boot whether anything had changed or not.** These syncs run on every deploy. A zone whose history holds one write per redeploy is a zone where the write that *mattered* is invisible. v2 compares first and reports `unchanged` — reported rather than omitted, so an operator sees that the record was checked instead of inferring it from silence.

One thing v1 got right and v2 keeps: **nothing in this package is allowed to fail a deploy.** A record that did not get written costs an address read off the dashboard, which is where v1 left it anyway. A lease that failed because Cloudflare returned 502 costs a redeploy and a world rollback.

## 8. Config surface added in this step

| Key | Why it is config and not a literal |
|---|---|
| `akash.price.denom`, `allowed_denoms` | The fifth v1 bug. A wallet's denomination is a property of the wallet, not of the code. |
| `akash.price.max_usd_per_day`, `min_usd_per_day`, `tolerance` | v1 hardcoded a `uakt` integer, which is a price in a currency that floats. |
| `akash.price.akt_usd_fallback`, `price_oracle_url` | `0` means *"no fallback, abort the cycle"* — v1's behaviour, now a choice. |
| `akash.max_deploy_attempts` | §3.3. Separate from `max_attempts` because the two failures cost different things. |
| `akash.api.retries`, `retry_wait`, `timeout` | v1 used `curl` with no retry, so one 502 cost a whole deploy cycle. `timeout` is validated against `bid_wait`, or one stalled connection consumes the entire bid window. |
| `akash.provider_status.*` | Asking the provider directly when the Console API's lease status has not caught up. `insecure_skip_verify` is now a decision: v1 used `curl -sk` unconditionally. A verified connection is always tried first. |
| `akash.placement.*` | Country list, reference point for distance ranking, `skip_ttl`, `deny_providers`, `min_uptime_30d`. |
| `akash.adopt_unleased` | The wreckage a controller leaves if it dies between creating and leasing: escrow funded, nothing running. |
| `dns.game_record`, `game_ttl` | New capability. §5 shows the TTL earning its place. |
| `dns.zone_id`, `api_base`, `timeout`, `retries`, `retry_wait` | v1 hardcoded all five; a hardcoded timeout is why a Cloudflare outage could hold a deploy open indefinitely. |
| `dns.relax_security`, `clear_redirect_rules` | v1 did both unconditionally. `clear_redirect_rules` defaults to **false** because it deletes every rule in the phase, including hand-written ones. |

## 9. Test results

Full suite, Debian under WSL2 on ext4, `go vet` clean, `go test ./... -count=1 -race`:

```
ok  cmd/pzctl          1.120s      ok  internal/dns        6.664s
ok  internal/agent    35.509s      ok  internal/fsm       15.954s
ok  internal/akash     9.535s      ok  internal/gitbus     7.346s
ok  internal/config    8.711s      ok  internal/sdl        2.847s
ok  internal/denom     1.142s      ok  internal/secrets    1.599s
ok  internal/state     1.196s      ok  internal/webhook    1.135s
```

`run 1: exit=0 races=0 failed_tests=0`. 50 tests in `internal/akash`, 49 in `internal/dns`.

The two new FSM tests are the ones that matter for §3.3:

| Test | Property |
|---|---|
| `TestDeployFailureAfterLeaseClosesIt` | every deployment the retries created was **closed** (`wantLive(0)`), the retry count is exactly `max_deploy_attempts`, and giving up leaves intent `stopped` so nothing resumes behind the operator's back |
| `TestDeployRetryYieldsToAHalt` | a halt consumed while a failed deploy is still parked ends it: one deploy, no retry |

The second needed the harness's `holdDeploys` gate to be meaningful. My first version wrote the halt trigger but never polled, so the whole retry chain ran inside `settle()` and the test passed for the wrong reason — a sleep would only have made the race probable, whereas the gate makes the ordering exact.

## 10. Bug scorecard after step 5

| # | v1 bug | Status |
|---|---|---|
| 1 | Player count always 0 | Fixed in step 4 (agent reads PZ's console; absence is `unknown`, never `0`) |
| 2 | Halt/restart loop | Fixed in steps 3–4 (park don't exit; triggers as edges) — and **defended again here**: the deploy retry is a counted edge precisely so it cannot become this bug with a new trigger |
| 3 | `server_info.json` parse error | Fixed in step 2 (one writer per branch, atomic orphan commits, typed documents) |
| 4 | Backup request failure | Fixed in step 3 (request IDs; a stale report cannot sign off a halt) |
| 5 | **Denomination arithmetic** (found by reading) | Fixed here, §2 |

## 11. State of the tree

Branch `v2-pzctl`. Nothing merged to `main`, nothing renamed — the rename is step 9, as agreed.

Committed and pushed: `fb48a6e` (18 files, +1101/−44), recording the three live discoveries. The dry-run fix from §6 and the `countries: [PT]` gate-config edit are the follow-up commit.

**The wallet.** Only v1's deployments remain open:

| Deployment | dseq | Left | Rate |
|---|---|---|---|
| v1 game server | 1787103872228 | \$2.85 | 34 uact/block ≈ \$0.49/day |
| **v1 controller** | 1787078661931 | **\$0.35** | ≈ \$0.07/day — **about 5 days** |

Two things need your attention, neither of them blocking:

**The v1 controller runs out of money in roughly five days.** When it does, the apex and `www` stop resolving to anything useful, because they are proxied CNAMEs pointing at its provider hostname. Nothing in v2 depends on it; it is your current dashboard.

**v2 requires an admin password that v1 never had as an environment variable.** `internal/secrets` demands `PZ_ADMIN_PASSWORD` unconditionally for `RoleController`, and v1's manifest has neither it nor `PZ_JOIN_PASSWORD` — they were presumably baked into `server.zip`. Eight of the ten GitHub secrets can be transcribed from the existing deployment under the *"reuse everything, just move it out of git"* decision. **These two need values from you**, and they are the only inputs I cannot supply myself.

## 12. Next: step 6

> `internal/httpapi` server side: streaming upload, download, `backups.json` index, retention, `server.zip` secret substitution

The interesting part is the last one, and it is why the passwords above are not simply baked in: the controller substitutes `RCONPassword`, `Password` and `AdminPassword` into `server.zip` *as it serves it*, so they never reach an image layer or an Akash manifest. The client half already exists and is exercised by step 4's tests against a stub; step 6 replaces the stub with the real thing.

