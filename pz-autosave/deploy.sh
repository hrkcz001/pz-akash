#!/bin/bash
# =============================================================================
# deploy.sh — deploy the PZ dedicated server on Akash via the Console API
# (managed wallet, everything over REST with x-api-key).
#
# Trigger: pushing a file named `start` to the pz-saves repo. The autosaver
# loop consumes that file and launches this script in the background.
#
# Flow:
#   1. Fetch AKT/USD (CoinGecko), derive max bid (uakt/block) from
#      MAX_PRICE_USD and the escrow deposit from DEPLOY_DAYS.
#   2. Take the SDL from the server's deployment.yaml in the pz-saves repo
#      (fallback: the sdl.template bundled in the image). The pricing
#      amount: is overwritten with the computed max bid (from MAX_PRICE_USD)
#      — the server SDL carries no deploy-policy tokens. Any legacy __TOKEN__
#      left unresolved aborts the deploy.
#   3. POST /v1/deployments  -> dseq
#   4. Poll /v1/bids. Filter: EU, healthy, IP-lease capable, in price band,
#      not on the skip list. Scoring: cheapest wins; any bid within
#      PRICE_TOLERANCE (default 20%) of the cheapest -> closest one wins.
#   5. POST /v1/leases for the winner.
#   6. Wait for the lease to be ready and grab the public IP (IP lease).
#      Push the IP into pz-saves/server_info.json (status "booting") so the
#      server boot does not block on manual IP configuration.
#   7. Failure at any step: close the deployment, remember the provider on the
#      skip list, retry with a fresh deployment. Success -> forget the list.
#
# Required env (set in the autosaver deployment):
#   AKASH_API_KEY   Console managed-wallet API key
#   SERVER_IMAGE    image tag — ONLY if the SDL still uses the
#                   __SERVER_IMAGE__ token (the pz-saves deployment.yaml is
#                   self-sufficient and hardcodes the tag instead)
# No pz-server env vars are needed here: the SDL from pz-saves carries them
# (SSH key, REPO_URL, SERVER_NAME, ADMIN_PASSWORD, memory, etc.).
#
# Optional env (defaults in brackets):
#   API_BASE [https://console-api.akash.network]
#   DEPLOY_DAYS [7]                escrow horizon: schedule.sh tops the lease up
#                                  to this many days at the ACTUAL bid price
#   INITIAL_DEPOSIT_DAYS [1]       initial escrow deposit at deploy time — ONE
#                                  day at max price (small; top-up happens after)
#   MIN_PRICE_USD [0.001]          ignore bids below this (USD/day)
#   MAX_PRICE_USD [3.0]            cap bids + SDL pricing (USD/day). Roughly
#                                  $1-3/day is the market for an 8 vCPU/16Gi
#                                  server; the cap just sets the ceiling, real
#                                  bids land below it.
#   PRICE_TOLERANCE [0.20]         proximity tie-break band (20% = 0.20)
#   BLOCKS_PER_DAY [14400]         Akash mainnet ~6s blocks
#   DEPOSIT_MARGIN [1.2]           escrow cushion multiplier
#   DEPOSIT_SETTLE_SEC [15]        pause after closing a deployment (refund)
#   BID_POLL_SEC [5] / BID_TIMEOUT_SEC [90]
#   LEASE_POLL_SEC [10] / LEASE_READY_TIMEOUT_SEC [600]
#   SERVER_ONLINE_VERIFY [true]    wait for server_info "online" after IP
#   SERVER_ONLINE_TIMEOUT_SEC [1200]
#   MAX_ATTEMPTS [15]              provider attempts per deploy cycle
#   SKIP_TTL_SEC [86400]           failed providers are remembered this long
#   MIN_UPTIME30D [0.95]           provider 30d uptime filter
#   CPU_UNITS [8] MEM_GB [16] STORAGE_GB [30]   capacity filter (match SDL)
#   REF_LAT [52.2297] REF_LON [21.0122]  reference point (Warsaw, central/east EU)
#   EU_COUNTRY_CODES [EU27 + GB CH NO UA]
#   AKT_USD_FALLBACK [unset]       rate used if CoinGecko is unreachable
#   SSH_PORT [2222]
# =============================================================================

set -uo pipefail

API_BASE="${API_BASE:-https://console-api.akash.network}"
STATE_DIR="${STATE_DIR:-/data}"
ACTIVE_DSEQ_FILE="$STATE_DIR/active_dseq"
SKIP_FILE="$STATE_DIR/skipped_providers"
LOCK_FILE="$STATE_DIR/deploy.lock"
SERVES_REPO="${SERVES_REPO:-/root/pz-saves}"
SDL_TEMPLATE="${SDL_TEMPLATE:-/usr/local/bin/sdl.template}"
# Primary SDL source is the server's deployment.yaml in the pz-saves repo
# (user-maintained). Falls back to the image template if absent.
SDL_SOURCE="${SDL_SOURCE:-$SERVES_REPO/deployment.yaml}"
SDL_OUT="$STATE_DIR/current_sdl.yaml"
PROVIDERS_ALL="$STATE_DIR/providers_all.json"
PROVIDERS_EU="$STATE_DIR/providers_eu.json"
BIDS_JSON="$STATE_DIR/bids.json"

# --- configuration -----------------------------------------------------------
DEPLOY_DAYS="${DEPLOY_DAYS:-7}"
INITIAL_DEPOSIT_DAYS="${INITIAL_DEPOSIT_DAYS:-1}"
MIN_PRICE_USD="${MIN_PRICE_USD:-0.001}"
MAX_PRICE_USD="${MAX_PRICE_USD:-3.0}"
PRICE_TOLERANCE="${PRICE_TOLERANCE:-0.20}"
BLOCKS_PER_DAY="${BLOCKS_PER_DAY:-14400}"
DEPOSIT_MARGIN="${DEPOSIT_MARGIN:-1.2}"
DEPOSIT_SETTLE_SEC="${DEPOSIT_SETTLE_SEC:-15}"
BID_POLL_SEC="${BID_POLL_SEC:-5}"
BID_TIMEOUT_SEC="${BID_TIMEOUT_SEC:-90}"
LEASE_POLL_SEC="${LEASE_POLL_SEC:-10}"
LEASE_READY_TIMEOUT_SEC="${LEASE_READY_TIMEOUT_SEC:-600}"
SERVER_ONLINE_VERIFY="${SERVER_ONLINE_VERIFY:-true}"
SERVER_ONLINE_TIMEOUT_SEC="${SERVER_ONLINE_TIMEOUT_SEC:-1200}"
MAX_ATTEMPTS="${MAX_ATTEMPTS:-15}"
SKIP_TTL_SEC="${SKIP_TTL_SEC:-86400}"
MIN_UPTIME30D="${MIN_UPTIME30D:-0.95}"
CPU_UNITS="${CPU_UNITS:-8}"
MEM_GB="${MEM_GB:-16}"
STORAGE_GB="${STORAGE_GB:-30}"
REF_LAT="${REF_LAT:-52.2297}"
REF_LON="${REF_LON:-21.0122}"
EU_COUNTRY_CODES="${EU_COUNTRY_CODES:-AT BE BG HR CY CZ DK EE FI FR DE GR HU IE IT LV LT LU MT NL PL PT RO SK SI ES SE GB CH NO UA}"
SSH_PORT="${SSH_PORT:-2222}"
AKT_USD_FALLBACK="${AKT_USD_FALLBACK:-}"

# --- helpers -----------------------------------------------------------------
log() { echo "[deploy] $(date -u +%FT%TZ) $*"; }
die() { log "FATAL: $*"; exit 1; }

require_api_key() {
  [ -n "${AKASH_API_KEY:-}" ] || die "AKASH_API_KEY is not set — deploy aborted."
}

push_with_retry() {
  for i in {1..5}; do
    git push && return 0
    git pull --rebase >/dev/null 2>&1
    sleep $((RANDOM % 3 + 1))
  done
  log "WARNING: git push failed after 5 retries"
}

# api METHOD PATH [BODY] — Console API call with x-api-key; retries once on 429.
api() {
  local method="$1" path="$2" body="${3:-}"
  local args=(-sS --max-time 30 -X "$method" "$API_BASE$path" -H "x-api-key: $AKASH_API_KEY" -H "Content-Type: application/json")
  [ -n "$body" ] && args+=(-d "$body")
  local raw code
  raw=$(curl "${args[@]}" -w $'\n%{http_code}' 2>/dev/null)
  code=$(printf '%s' "$raw" | tail -n1)
  if [ "$code" = "429" ]; then
    log "Rate limited (429) on $method $path — sleeping 15s and retrying."
    sleep 15
    raw=$(curl "${args[@]}" -w $'\n%{http_code}' 2>/dev/null)
    code=$(printf '%s' "$raw" | tail -n1)
  fi
  if [ -z "$code" ] || [ "$code" = "000" ]; then
    log "WARNING: API call $method $path failed (no response) — continuing with empty result."
    return 1
  fi
  printf '%s' "$raw" | head -n -1
}

fetch_akt_usd() {
  local price
  price=$(curl -sS --max-time 15 "https://api.coingecko.com/api/v3/simple/price?ids=akash-network&vs_currencies=usd" 2>/dev/null | jq -r '."akash-network".usd // empty' 2>/dev/null)
  if [ -n "$price" ] && [ "$price" != "null" ]; then
    echo "$price"
    return 0
  fi
  if [ -n "$AKT_USD_FALLBACK" ]; then
    log "CoinGecko unreachable — using AKT_USD_FALLBACK=$AKT_USD_FALLBACK"
    echo "$AKT_USD_FALLBACK"
    return 0
  fi
  return 1
}

build_sdl() { # $1 = max uakt/block
  # The price lives in the autosaver env (MAX_PRICE_USD). The SDL from
  # pz-saves carries no tokens: after substituting any legacy tokens, we
  # overwrite the numeric `amount:` in the pricing block with the computed
  # max bid. The server file/container never sees the price policy.
  python3 - "$SDL_SOURCE" "$SDL_OUT" "$1" \
    "${SERVER_IMAGE:-}" "${SSH_PRIVATE_KEY_BASE64:-}" "${REPO_URL:-}" \
    "${GIT_USER_NAME:-}" "${GIT_USER_EMAIL:-}" "$SSH_PORT" \
    "${SERVER_NAME:-}" "${ADMIN_PASSWORD:-}" "${SERVER_MEMORY_MAX:-}" \
    "${SERVER_MEMORY_MIN:-}" "${RESTORE_POLL_INTERVAL_SEC:-}" \
    "${WAIT_ON_CRASH_SEC:-}" "${AUTO_CONFIGURE_MAPS:-}" \
    "${AUTO_CONFIGURE_MODS:-}" <<'PYEOF'
import re, sys
tpl, out, max_uakt = sys.argv[1], sys.argv[2], str(sys.argv[3])
tokens = ["__SERVER_IMAGE__", "__SSH_PRIVATE_KEY_BASE64__", "__REPO_URL__",
          "__GIT_USER_NAME__", "__GIT_USER_EMAIL__", "__SSH_PORT__",
          "__SERVER_NAME__", "__ADMIN_PASSWORD__", "__SERVER_MEMORY_MAX__",
          "__SERVER_MEMORY_MIN__", "__RESTORE_POLL_INTERVAL_SEC__",
          "__WAIT_ON_CRASH_SEC__", "__AUTO_CONFIGURE_MAPS__",
          "__AUTO_CONFIGURE_MODS__", "__MAX_PRICE_UAKT__"]
vals = sys.argv[4:19]
s = open(tpl).read()
for t, v in zip(tokens, vals):
    s = s.replace(t, str(v))
# Inject the autosaver-controlled max bid into every numeric pricing amount.
s = re.sub(r'(?m)^(\s*amount:\s*)[0-9]+(\.[0-9]+)?\s*$', r'\g<1>' + max_uakt, s)
open(out, "w").write(s)
print(f"SDL written: {out} (max {max_uakt} uakt/block)")
PYEOF
}

# --- skip list (failed providers) -------------------------------------------
load_skips() {
  SKIPS=()
  [ -f "$SKIP_FILE" ] || return 0
  local now tmp prov ts
  now=$(date +%s)
  tmp=$(mktemp)
  while read -r prov ts; do
    [ -n "$prov" ] || continue
    if [ $((now - ts)) -lt "$SKIP_TTL_SEC" ]; then
      echo "$prov $ts" >> "$tmp"
      SKIPS+=("$prov")
    fi
  done < "$SKIP_FILE"
  mv "$tmp" "$SKIP_FILE"
  if [ "${#SKIPS[@]}" -gt 0 ]; then
    log "Remembered ${#SKIPS[@]} skipped provider(s): ${SKIPS[*]}"
  fi
}

add_skip() { # $1 = provider address
  if ! grep -q "^$1 " "$SKIP_FILE" 2>/dev/null; then
    echo "$1 $(date +%s)" >> "$SKIP_FILE"
  fi
  log "Provider $1 FAILED — added to skip list."
}

clear_skips() {
  rm -f "$SKIP_FILE"
  log "Deploy succeeded — all previously failed providers forgotten."
}

# --- deployment lifecycle -----------------------------------------------------
close_deployment() { # $1 = dseq
  local dseq="$1" st
  st=$(api GET "/v1/deployments/$dseq" | jq -r '.data.deployment.state // empty' 2>/dev/null)
  if [ "$st" = "active" ]; then
    log "Closing deployment $dseq ..."
    api DELETE "/v1/deployments/$dseq" >/dev/null
    sleep "$DEPOSIT_SETTLE_SEC"
  fi
  rm -f "$ACTIVE_DSEQ_FILE"
}

close_previous() {
  if [ -f "$ACTIVE_DSEQ_FILE" ]; then
    close_deployment "$(cat "$ACTIVE_DSEQ_FILE")"
  fi
}

already_deployed() {
  local dseq st ip
  if [ -f "$ACTIVE_DSEQ_FILE" ]; then
    dseq=$(cat "$ACTIVE_DSEQ_FILE")
    st=$(api GET "/v1/deployments/$dseq" | jq -r '.data.deployment.state // empty' 2>/dev/null)
    if [ "$st" = "active" ]; then
      if [ -f "$SERVES_REPO/server_info.json" ]; then
        ip=$(jq -r '.ip // "pending"' "$SERVES_REPO/server_info.json" 2>/dev/null)
        st=$(jq -r '.status // ""' "$SERVES_REPO/server_info.json" 2>/dev/null)
        if [ "$ip" != "pending" ] && { [ "$st" = "online" ] || [ "$st" = "booting" ]; }; then
          log "Server already deployed (dseq=$dseq, status=$st at $ip) — nothing to do."
          return 0
        fi
      fi
      log "Deployment $dseq is active but the server is not online — redeploying (will close the old one)."
    fi
  fi
  return 1
}

# --- bid selection ------------------------------------------------------------
# pick_bid BIDS_JSON PROVIDERS_EU_JSON -> "provider gseq oseq hostUri uakt_block usd_day" or empty
pick_bid() {
  python3 - "$1" "$2" "$SKIP_FILE" "$MIN_PRICE_USD" "$MAX_PRICE_USD" \
    "$BLOCKS_PER_DAY" "$PRICE_TOLERANCE" "$REF_LAT" "$REF_LON" \
    "$EU_COUNTRY_CODES" "$AKT_USD" <<'PYEOF'
import json, math, os, sys

bids_path, prov_path, skip_path = sys.argv[1], sys.argv[2], sys.argv[3]
min_usd, max_usd = float(sys.argv[4]), float(sys.argv[5])
bpd = int(sys.argv[6])
tol = float(sys.argv[7])
ref_lat, ref_lon = float(sys.argv[8]), float(sys.argv[9])
eu_codes = set(sys.argv[10].upper().split())
akt_usd = float(sys.argv[11])

EU_NAMES = {"Germany":"DE","France":"FR","Netherlands":"NL","Belgium":"BE",
  "Luxembourg":"LU","Poland":"PL","Czechia":"CZ","Czech Republic":"CZ",
  "Slovakia":"SK","Austria":"AT","Hungary":"HU","Romania":"RO","Bulgaria":"BG",
  "Greece":"GR","Italy":"IT","Spain":"ES","Portugal":"PT","Ireland":"IE",
  "Denmark":"DK","Sweden":"SE","Finland":"FI","Estonia":"EE","Latvia":"LV",
  "Lithuania":"LT","Slovenia":"SI","Croatia":"HR","Cyprus":"CY","Malta":"MT",
  "United Kingdom":"GB","Switzerland":"CH","Norway":"NO","Ukraine":"UA",
  "Serbia":"RS","Moldova":"MD","Romania":"RO"}

def is_eu(code, name):
    code = (code or "").upper()
    if code in eu_codes:
        return True
    if name and EU_NAMES.get(name) in eu_codes:
        return True
    return False

def distance(lat, lon):
    try:
        lat, lon = float(lat), float(lon)
    except (TypeError, ValueError):
        return 1e7  # no coordinates -> treated as far away
    R = 6371.0
    p1, p2 = math.radians(ref_lat), math.radians(lat)
    dp = math.radians(lat - ref_lat)
    dl = math.radians(lon - ref_lon)
    a = math.sin(dp/2)**2 + math.cos(p1)*math.cos(p2)*math.sin(dl/2)**2
    return 2 * R * math.asin(math.sqrt(a))

skips = set()
if os.path.exists(skip_path):
    for line in open(skip_path):
        parts = line.split()
        if parts:
            skips.add(parts[0])

provs = {}
for p in json.load(open(prov_path)):
    provs[p.get("owner")] = p

raw = json.load(open(bids_path))
bids = raw.get("data", []) if isinstance(raw, dict) else raw

cands = []
for entry in bids:
    b = entry.get("bid", entry) if isinstance(entry, dict) else entry
    if b.get("state") not in (None, "open"):
        continue
    bid_id = b.get("id") or {}
    prov = bid_id.get("provider")
    pinfo = provs.get(prov)
    if not pinfo:
        continue
    if not is_eu(pinfo.get("ipCountryCode") or "", pinfo.get("country") or ""):
        continue
    if prov in skips:
        continue
    price = b.get("price") or {}
    denom = (price.get("denom") or "").lower()
    if denom and denom not in ("uakt", "uact"):
        continue
    try:
        # amounts come back as decimal strings, e.g. "32.000000000000000000"
        amount = float(price.get("amount") or 0)
    except (TypeError, ValueError):
        continue
    usd_day = amount / 1e6 * bpd * akt_usd
    if usd_day < min_usd or usd_day > max_usd:
        continue
    cands.append({"provider": prov, "gseq": bid_id.get("gseq", 1),
                  "oseq": bid_id.get("oseq", 1),
                  "hostUri": pinfo.get("hostUri", ""),
                  "uakt_block": amount, "usd_day": usd_day,
                  "dist": distance(pinfo.get("ipLat"), pinfo.get("ipLon"))})

if not cands:
    sys.exit(0)

cands.sort(key=lambda c: (c["usd_day"], c["dist"]))
best = cands[0]["usd_day"]
band = [c for c in cands if c["usd_day"] <= best * (1 + tol)]
pick = min(band, key=lambda c: c["dist"])
print(f'{pick["provider"]} {pick["gseq"]} {pick["oseq"]} {pick["hostUri"]} {pick["uakt_block"]} {pick["usd_day"]:.4f}')
PYEOF
}

# --- lease readiness + IP -----------------------------------------------------
console_lease_ip() { # $1 = dseq
  api GET "/v1/deployments/$1" | python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(0)
leases = (d.get("data") or {}).get("leases") or []
for l in leases:
    if l.get("state") != "active":
        continue
    st = l.get("status") or {}
    svc = (st.get("services") or {}).get("pz-server") or {}
    if (svc.get("ready_replicas") or 0) < 1:
        continue
    for arr in ((st.get("ips") or {}).values()):
        for e in arr or []:
            ip = e.get("ip") or e.get("IP")
            if ip:
                print(ip)
                sys.exit(0)
'
}

provider_lease_ip() { # $1 provider $2 dseq $3 gseq $4 oseq $5 hostUri
  local token host
  token=$(api POST /v1/create-jwt-token '{"data":{"ttl":600,"leases":{"access":"scoped","scope":["status"]}}}' | jq -r '.data.token // empty' 2>/dev/null)
  [ -n "$token" ] || return 1
  host=$(printf '%s' "$5" | sed 's#^https\?://##; s#/$##')
  curl -sk --max-time 15 "https://$host/lease/$2/$3/$4/status" \
    -H "Authorization: Bearer $token" 2>/dev/null | python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(0)
svc = (d.get("services") or {}).get("pz-server") or {}
if (svc.get("ready_replicas") or 0) >= 1:
    for arr in (d.get("ips") or {}).values():
        for e in arr or []:
            ip = e.get("ip") or e.get("IP")
            if ip:
                print(ip)
                sys.exit(0)
'
}

wait_for_lease() { # $1 dseq $2 provider $3 gseq $4 oseq $5 hostUri -> prints IP or nothing
  local dseq="$1" provider="$2" gseq="$3" oseq="$4" hostUri="$5"
  local deadline n ip
  deadline=$(( $(date +%s) + LEASE_READY_TIMEOUT_SEC ))
  n=0
  while [ "$(date +%s)" -lt "$deadline" ]; do
    ip=$(console_lease_ip "$dseq")
    if [ -n "$ip" ]; then
      echo "$ip"
      return 0
    fi
    n=$((n + 1))
    if [ $((n % 6)) -eq 0 ]; then
      ip=$(provider_lease_ip "$provider" "$dseq" "$gseq" "$oseq" "$hostUri")
      if [ -n "$ip" ]; then
        echo "$ip"
        return 0
      fi
    fi
    sleep "$LEASE_POLL_SEC"
  done
  return 1
}

# --- server_info.json (pz-saves) ----------------------------------------------
mark_server_ip() { # $1 = ip
  local ip="$1"
  ( cd "$SERVES_REPO" && git pull >/dev/null 2>&1 )
  echo "{\"ip\": \"$ip\", \"port\": $SSH_PORT, \"status\": \"booting\"}" > "$SERVES_REPO/server_info.json"
  ( cd "$SERVES_REPO" && git add server_info.json \
    && git commit -m "Deployed server at $ip (status booting)" || true \
    && push_with_retry )
  log "server_info.json updated: server booting at $ip:$SSH_PORT"
}

wait_server_online() {
  local deadline st
  deadline=$(( $(date +%s) + SERVER_ONLINE_TIMEOUT_SEC ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    ( cd "$SERVES_REPO" && git pull >/dev/null 2>&1 )
    st=$(jq -r '.status // ""' "$SERVES_REPO/server_info.json" 2>/dev/null)
    if [ "$st" = "online" ]; then
      return 0
    fi
    sleep 20
  done
  return 1
}

reset_server_info() {
  ( cd "$SERVES_REPO" && git pull >/dev/null 2>&1 )
  echo "{\"ip\": \"pending\", \"port\": $SSH_PORT, \"status\": \"stopped\"}" > "$SERVES_REPO/server_info.json"
  ( cd "$SERVES_REPO" && git add server_info.json \
    && git commit -m "Deploy cycle failed - reset to stopped/pending" || true \
    && push_with_retry )
}

# --- one deploy attempt ---------------------------------------------------------
# returns 0 = success, 1 = provider failure (skipped, retry), 2 = no candidates left
attempt_deploy() { # $1 = attempt number
  local attempt="$1"

  close_previous

  # Fetch AKT/USD once per deploy cycle (CoinGecko rate-limits quickly if
  # hammered on every attempt).
  if [ -z "${AKT_USD:-}" ]; then
    AKT_USD=$(fetch_akt_usd) || die "Cannot get AKT/USD (CoinGecko unreachable and AKT_USD_FALLBACK unset) — aborting cycle."
  fi

  local max_uakt deposit_usd
  max_uakt=$(python3 - "$MAX_PRICE_USD" "$AKT_USD" "$BLOCKS_PER_DAY" <<'PYEOF'
import sys
usd, rate, bpd = float(sys.argv[1]), float(sys.argv[2]), int(sys.argv[3])
print(max(0, int(usd * 1e6 / (rate * bpd))))
PYEOF
)
  # Initial escrow deposit: one day at max price. schedule.sh tops the escrow
  # up to DEPLOY_DAYS at the ACTUAL lease price shortly after the deploy, so
  # the wallet only needs to cover this small amount to get the server up.
  deposit_usd=$(python3 - "$MAX_PRICE_USD" "$INITIAL_DEPOSIT_DAYS" "$DEPOSIT_MARGIN" <<'PYEOF'
import sys
usd, days, margin = float(sys.argv[1]), int(sys.argv[2]), float(sys.argv[3])
print(max(0.5, round(usd * days * margin, 2)))
PYEOF
)
  [ "$max_uakt" -gt 0 ] || die "Computed max bid is 0 uakt/block (MAX_PRICE_USD too low for AKT/USD=$AKT_USD)."

  log "AKT/USD=$AKT_USD · max bid=${max_uakt} uakt/block · initial deposit=${deposit_usd} USD (${INITIAL_DEPOSIT_DAYS}d at max price; schedule tops up to ${DEPLOY_DAYS}d at actual price)"

  build_sdl "$max_uakt" || die "SDL build failed."
  if grep -q '__[A-Z_]*__' "$SDL_OUT"; then
    die "Unresolved SDL tokens: $(grep -o '__[A-Z_]*__' "$SDL_OUT" | sort -u | tr '\n' ' ') — set the env var or hardcode the value in the SDL."
  fi

  local resp dseq manifest
  resp=$(api POST /v1/deployments "{\"data\":{\"sdl\":$(jq -Rs . < "$SDL_OUT"),\"deposit\":$deposit_usd}}")
  dseq=$(echo "$resp" | jq -r '.data.dseq // empty' 2>/dev/null)
  if [ -z "$dseq" ]; then
    log "Deployment create failed: $(echo "$resp" | head -c 400)"
    if echo "$resp" | grep -qi 'insufficient balance\|PaymentRequired'; then
      log "FATAL: wallet balance too low for the initial deposit (\$${deposit_usd} USD) — top up AKT in the Akash Console, then push start again."
      return 2
    fi
    return 1
  fi
  manifest=$(echo "$resp" | jq -r '.data.manifest // empty' 2>/dev/null)
  [ -n "$manifest" ] || manifest=$(cat "$SDL_OUT")
  echo "$dseq" > "$ACTIVE_DSEQ_FILE"
  log "Deployment created: dseq=$dseq"

  # wait for bids
  local bids deadline
  deadline=$(( $(date +%s) + BID_TIMEOUT_SEC ))
  while :; do
    bids=$(api GET "/v1/bids?dseq=$dseq")
    if echo "$bids" | jq -e '.data | length > 0' >/dev/null 2>&1; then
      break
    fi
    if [ "$(date +%s)" -ge "$deadline" ]; then
      log "No bids within ${BID_TIMEOUT_SEC}s — closing deployment."
      close_deployment "$dseq"
      return 1
    fi
    sleep "$BID_POLL_SEC"
  done
  echo "$bids" > "$BIDS_JSON"
  log "Bids received ($(jq -r '.data | length' "$BIDS_JSON"))."

  local pick provider gseq oseq hostUri
  pick=$(pick_bid "$BIDS_JSON" "$PROVIDERS_EU")
  if [ -z "$pick" ]; then
    log "No acceptable bids (EU + price band + not skipped). Closing deployment."
    close_deployment "$dseq"
    return 2
  fi
  provider=$(echo "$pick" | awk '{print $1}')
  gseq=$(echo "$pick" | awk '{print $2}')
  oseq=$(echo "$pick" | awk '{print $3}')
  hostUri=$(echo "$pick" | awk '{print $4}')
  log "Picked provider $provider (host $hostUri, $(echo "$pick" | awk '{print $6}') USD/day)."

  resp=$(api POST /v1/leases "{\"manifest\":$(jq -Rs . <<<"$manifest"),\"leases\":[{\"dseq\":\"$dseq\",\"gseq\":$gseq,\"oseq\":$oseq,\"provider\":\"$provider\"}]}")
  if ! echo "$resp" | jq -e '.data.deployment.state == "active"' >/dev/null 2>&1; then
    log "Lease create failed: $(echo "$resp" | head -c 400)"
    add_skip "$provider"
    close_deployment "$dseq"
    return 1
  fi
  log "Lease created with $provider (dseq=$dseq)."

  local ip
  ip=$(wait_for_lease "$dseq" "$provider" "$gseq" "$oseq" "$hostUri")
  if [ -z "$ip" ]; then
    log "Lease $dseq did not become ready within ${LEASE_READY_TIMEOUT_SEC}s — provider failed to deploy."
    add_skip "$provider"
    close_deployment "$dseq"
    return 1
  fi
  log "Server container is live at $ip."

  mark_server_ip "$ip" || die "Failed to write server_info.json."

  if [ "$SERVER_ONLINE_VERIFY" = "true" ]; then
    log "Waiting for the server to report 'online' (timeout ${SERVER_ONLINE_TIMEOUT_SEC}s)..."
    if ! wait_server_online; then
      log "Server did not reach 'online' within ${SERVER_ONLINE_TIMEOUT_SEC}s on $provider — treating as failed deploy."
      add_skip "$provider"
      close_deployment "$dseq"
      return 1
    fi
  fi

  echo "$ip" > "$STATE_DIR/last_deploy_ip"
  log "Attempt $attempt SUCCEEDED (dseq=$dseq, provider=$provider, ip=$ip)."
  return 0
}

# --- main ----------------------------------------------------------------------
main() {
  require_api_key

  if [ -f "$LOCK_FILE" ] && kill -0 "$(cat "$LOCK_FILE")" 2>/dev/null; then
    log "Another deploy is already running (pid $(cat "$LOCK_FILE")). Exiting."
    exit 0
  fi
  echo $$ > "$LOCK_FILE"
  trap 'rm -f "$LOCK_FILE"' EXIT

  load_skips

  if already_deployed; then
    exit 0
  fi

  # Pick the SDL source: the server's deployment.yaml from pz-saves, else the image template.
  ( cd "$SERVES_REPO" && git pull >/dev/null 2>&1 )
  if [ -f "$SERVES_REPO/deployment.yaml" ]; then
    SDL_SOURCE="$SERVES_REPO/deployment.yaml"
    log "Using SDL from pz-saves: $SDL_SOURCE"
  elif [ -f "$SDL_TEMPLATE" ]; then
    SDL_SOURCE="$SDL_TEMPLATE"
    log "WARNING: $SERVES_REPO/deployment.yaml not found — falling back to image template $SDL_TEMPLATE"
  else
    die "No SDL available: $SERVES_REPO/deployment.yaml is missing and the image template $SDL_TEMPLATE does not exist."
  fi

  log "Fetching provider list (EU + online + IP-lease capable + capacity + uptime)..."
  api GET "/v1/providers?scope=all" > "$PROVIDERS_ALL" 2>/dev/null || die "Failed to fetch providers."
  local cpu_milli mem_bytes storage_bytes
  cpu_milli=$((CPU_UNITS * 1000))
  mem_bytes=$((MEM_GB * 1024 * 1024 * 1024))
  storage_bytes=$((STORAGE_GB * 1024 * 1024 * 1024))
  jq -c --argjson min_up "$MIN_UPTIME30D" --argjson cpu "$cpu_milli" \
     --argjson mem "$mem_bytes" --argjson stor "$storage_bytes" \
     '[if type == "array" then .[] else .data[] end |
      select(.isOnline == true and .isValidVersion == true and .featEndpointIp == true) |
      select((.uptime30d // 0) >= $min_up) |
      select((.stats.cpu.available // 0) >= $cpu) |
      select((.stats.memory.available // 0) >= $mem) |
      select((.stats.storage.ephemeral.available // 0) >= $stor) |
      {owner, ipCountryCode, country, ipLat, ipLon, hostUri}]' \
     "$PROVIDERS_ALL" > "$PROVIDERS_EU"
  log "$(wc -l < "$PROVIDERS_EU") candidate provider(s) after filtering."

  local attempt=0 rc
  while [ "$attempt" -lt "$MAX_ATTEMPTS" ]; do
    attempt=$((attempt + 1))
    log "=== Deploy attempt $attempt/$MAX_ATTEMPTS ==="
    attempt_deploy "$attempt"
    rc=$?
    if [ "$rc" -eq 0 ]; then
      clear_skips
      exit 0
    fi
    if [ "$rc" -eq 2 ]; then
      log "No more acceptable candidates — ending the deploy cycle."
      break
    fi
    log "Attempt $attempt failed."
  done

  log "Deploy cycle failed after $attempt attempt(s)."
  reset_server_info
  exit 1
}

main "$@"
