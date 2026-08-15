#!/bin/bash
# =============================================================================
# schedule.sh — scheduled shutdown + escrow top-up. Called by the autosaver
# loop every iteration (cheap no-ops when nothing is scheduled).
#
# stop_at file (pz-saves): epoch seconds OR "YYYY-MM-DD HH:MM[:SS]" (UTC).
#   - When reached: manual save + backup, graceful RCON quit, then the Akash
#     deployment is closed (stops billing). The file is consumed.
#   - If edited later -> funds are topped up so the escrow always covers the
#     remaining time (with DEPOSIT_MARGIN headroom).
#   - If already in the past -> immediate graceful stop on next iteration.
# =============================================================================

set -uo pipefail
source /usr/local/bin/state.sh

API_BASE="${API_BASE:-https://console-api.akash.network}"
ACTIVE_DSEQ_FILE="${ACTIVE_DSEQ_FILE:-$STATE_DIR/active_dseq}"
DEPLOY_DAYS="${DEPLOY_DAYS:-7}"
BLOCKS_PER_DAY="${BLOCKS_PER_DAY:-14400}"
DEPOSIT_MARGIN="${DEPOSIT_MARGIN:-1.2}"
FUNDS_CHECK_SEC="${FUNDS_CHECK_SEC:-600}"
MIN_TOPUP_USD="${MIN_TOPUP_USD:-0.5}"
HALT_CONFIRM_SEC="${HALT_CONFIRM_SEC:-180}"
AKT_USD_FALLBACK="${AKT_USD_FALLBACK:-}"

log() { echo "[schedule] $(date -u +%FT%TZ) $*"; }

api() { # METHOD PATH [BODY]
  local method="$1" path="$2" body="${3:-}"
  local args=(-sS -X "$method" "$API_BASE$path" -H "x-api-key: $AKASH_API_KEY" -H "Content-Type: application/json")
  [ -n "$body" ] && args+=(-d "$body")
  curl "${args[@]}" 2>/dev/null
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

# parse_stop_at -> epoch seconds, or empty if no/invalid stop_at
parse_stop_at() {
  [ -f "$SERVES_REPO/stop_at" ] || return 0
  python3 - "$(cat "$SERVES_REPO/stop_at" | tr -d '\r\n')" <<'PYEOF'
import datetime, sys, time
s = sys.argv[1].strip()
if not s:
    sys.exit(0)
if s.isdigit():
    print(int(s)); sys.exit(0)
for fmt in ("%Y-%m-%d %H:%M:%S", "%Y-%m-%d %H:%M", "%Y-%m-%dT%H:%M:%S", "%Y-%m-%dT%H:%M"):
    try:
        print(int(datetime.datetime.strptime(s, fmt).replace(tzinfo=datetime.timezone.utc).timestamp()))
        sys.exit(0)
    except ValueError:
        pass
sys.exit(1)
PYEOF
}

# --- scheduled stop -----------------------------------------------------------
# wait_server_stopped / close_deployment are shared helpers from state.sh.
# (schedule.sh's own api()/log() shadow the state.sh versions — identical
# behavior, just a [schedule] log prefix.)

check_stop_time() {
  local stop_epoch now
  stop_epoch=$(parse_stop_at) || {
    log "stop_at is unreadable — ignoring (fix or remove the file)."
    return 0
  }
  [ -n "$stop_epoch" ] || return 0
  now=$(date +%s)
  if [ "$now" -lt "$stop_epoch" ]; then
    return 0
  fi

  log "stop_at reached (now=$now >= stop=$stop_epoch) — saving and stopping the server."
  consume_file stop_at
  run_backup 1
  if wait_server_stopped; then
    log "Server reported 'stopped' — closing deployment to stop billing."
  else
    log "Server did not report 'stopped' within ${HALT_CONFIRM_SEC}s — closing deployment anyway."
  fi
  close_deployment
}

# --- escrow top-up --------------------------------------------------------------
# Ensure the deployment escrow covers the remaining time until stop_at (or
# DEPLOY_DAYS when no stop_at is set) with DEPOSIT_MARGIN headroom.
ensure_funds() {
  [ -n "${AKASH_API_KEY:-}" ] || return 0
  [ -f "$ACTIVE_DSEQ_FILE" ] || return 0
  [ -f "$SERVES_REPO/server_info.json" ] || return 0

  local st ip
  st=$(server_info_val status "")
  ip=$(server_info_val ip "")
  { [ "$st" = "online" ] || [ "$st" = "booting" ]; } || return 0
  [ -n "$ip" ] && [ "$ip" != "pending" ] || return 0

  local last now
  last=$(cat "$STATE_DIR/last_funds_check" 2>/dev/null || echo 0)
  now=$(date +%s)
  [ $((now - last)) -ge "$FUNDS_CHECK_SEC" ] || return 0
  echo "$now" > "$STATE_DIR/last_funds_check"

  local dseq resp state amount balance rate
  dseq=$(cat "$ACTIVE_DSEQ_FILE")
  resp=$(api GET "/v1/deployments/$dseq")
  state=$(echo "$resp" | jq -r '.data.deployment.state // ""' 2>/dev/null)
  [ "$state" = "active" ] || return 0

  amount=$(echo "$resp" | jq -r '[.data.leases[]? | select(.state == "active") | .price.amount] | map(tonumber) | add // 0' 2>/dev/null)
  balance=$(echo "$resp" | jq -r '[.data.escrow_account.state.funds[]? | select(.denom == "uakt" or .denom == "uact") | .amount] | map(tonumber) | add // 0' 2>/dev/null)
  rate=$(fetch_akt_usd) || { log "Cannot get AKT/USD — skipping funds check."; return 0; }

  local usd_day bal_usd
  usd_day=$(python3 - "$amount" "$rate" "$BLOCKS_PER_DAY" <<'PYEOF'
import sys
uakt, rate, bpd = float(sys.argv[1]), float(sys.argv[2]), int(sys.argv[3])
print(uakt / 1e6 * bpd * rate)
PYEOF
)
  bal_usd=$(python3 - "$balance" "$rate" <<'PYEOF'
import sys
print(float(sys.argv[1]) / 1e6 * float(sys.argv[2]))
PYEOF
)

  local stop_epoch days_left needed deficit topup
  stop_epoch=$(parse_stop_at) || stop_epoch=""
  if [ -n "$stop_epoch" ]; then
    days_left=$(python3 - "$stop_epoch" "$now" <<'PYEOF'
import sys
print(max(0.0, (int(sys.argv[1]) - int(sys.argv[2])) / 86400.0))
PYEOF
)
  else
    days_left="$DEPLOY_DAYS"
  fi

  needed=$(python3 - "$usd_day" "$days_left" "$DEPOSIT_MARGIN" <<'PYEOF'
import sys
usd, days, margin = float(sys.argv[1]), float(sys.argv[2]), float(sys.argv[3])
print(max(0.0, usd * days * margin))
PYEOF
)
  deficit=$(python3 - "$needed" "$bal_usd" <<'PYEOF'
import sys
print(round(float(sys.argv[1]) - float(sys.argv[2]), 2))
PYEOF
)
  if python3 -c "import sys; sys.exit(0 if float('$deficit') > 0 else 1)"; then
    topup=$(python3 - "$deficit" "$MIN_TOPUP_USD" <<'PYEOF'
import sys
print(max(float(sys.argv[2]), float(sys.argv[1])))
PYEOF
)
    log "Escrow low: balance=\$${bal_usd}, needed until stop=\$${needed} (${days_left} days @ \$${usd_day}/day + margin) — topping up \$${topup}."
    api POST /v1/deposit-deployment "{\"data\":{\"dseq\":\"$dseq\",\"deposit\":$topup}}" >/dev/null
    log "Top-up sent (dseq=$dseq)."
  else
    log "Escrow OK: balance=\$${bal_usd}, needed until stop=\$${needed}."
  fi
}

main() {
  [ -n "${AKASH_API_KEY:-}" ] || { log "AKASH_API_KEY not set — schedule/funds disabled."; exit 0; }
  check_stop_time
  ensure_funds
}

main "$@"
