#!/bin/bash
# =============================================================================
# state.sh — shared helpers for the autosaver container. Sourced by autosave.sh
# (the main loop), trigger.sh (webhook entry) and schedule.sh (stop scheduling).
#
# Trigger files in the pz-saves repo are CONSUMED (removed + pushed), exactly
# like `start`:
#   start  -> launch deploy.sh (background)
#   backup -> one manual safe backup
#   halt   -> one manual safe backup, then RCON quit (graceful shutdown), then
#             close the Akash deployment (billing stops, escrow refunded)
#   stop_at -> handled by schedule.sh (scheduled stop + escrow top-up)
# =============================================================================

set -uo pipefail

SERVES_REPO="${SERVES_REPO:-/root/pz-saves}"
BACKUP_DIR="${BACKUP_DIR:-/data/backups}"
STATE_DIR="${STATE_DIR:-/data}"
RCON_PASSWORD="${RCON_PASSWORD:-}"
RCON_PORT="${RCON_PORT:-27015}"
SSH_CONNECT_TIMEOUT="${SSH_CONNECT_TIMEOUT:-10}"
BACKUP_LOCK="$STATE_DIR/backup.lock"

# RCON credentials come from the server SDL in the pz-saves repo (single
# source of truth) unless explicitly overridden in the autosaver env — the
# autosaver deployment itself carries no server info.
resolve_rcon() {
  if [ -z "$RCON_PASSWORD" ]; then
    if [ -f "$SERVES_REPO/deployment.yaml" ]; then
      RCON_PASSWORD=$(grep -oE 'ADMIN_PASSWORD=[^[:space:]]+' "$SERVES_REPO/deployment.yaml" 2>/dev/null | head -1 | cut -d= -f2-)
    fi
    RCON_PASSWORD="${RCON_PASSWORD:-Qwerty0123**}"
  fi
}

push_with_retry() {
  for i in {1..5}; do
    git push && return 0
    git pull --rebase >/dev/null 2>&1
    sleep $((RANDOM % 3 + 1))
  done
  echo "WARNING: git push failed after 5 retries"
}

git_pull_state() {
  ( cd "$SERVES_REPO" && git pull >/dev/null 2>&1 )
}

consume_file() { # $1 = filename (start|backup|halt|stop_at)
  local f="$1"
  (
    cd "$SERVES_REPO" || exit 1
    git rm -f "$f" 2>/dev/null || rm -f "$SERVES_REPO/$f"
    git commit -m "Consumed trigger: $f removed" || true
    push_with_retry
  )
  echo "[state] consumed trigger file: $f"
}

server_info_val() { # $1 = key, $2 = default
  jq -r --arg k "$1" --arg d "$2" '.[$k] // $d' "$SERVES_REPO/server_info.json" 2>/dev/null || echo "$2"
}

# --- shared Akash lifecycle helpers (used by both the halt trigger below and
# schedule.sh's stop_at path). schedule.sh shadows log/api with its own
# equivalents — identical behavior, just a different log prefix.
log() { echo "[autosaver] $(date -u +%FT%TZ) $*"; }

API_BASE="${API_BASE:-https://console-api.akash.network}"
ACTIVE_DSEQ_FILE="${ACTIVE_DSEQ_FILE:-$STATE_DIR/active_dseq}"
HALT_CONFIRM_SEC="${HALT_CONFIRM_SEC:-180}"

# api METHOD PATH [BODY] — Console API call with x-api-key (no retry; deploy.sh
# brings its own richer helper for the deploy cycle).
api() {
  local method="$1" path="$2" body="${3:-}"
  local args=(-sS -X "$method" "$API_BASE$path" -H "x-api-key: $AKASH_API_KEY" -H "Content-Type: application/json")
  [ -n "$body" ] && args+=(-d "$body")
  curl "${args[@]}" 2>/dev/null
}

# wait_server_stopped — poll server_info.json until status == "stopped".
wait_server_stopped() {
  local deadline st
  deadline=$(( $(date +%s) + HALT_CONFIRM_SEC ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    git_pull_state
    st=$(server_info_val status "")
    if [ "$st" = "stopped" ]; then
      return 0
    fi
    sleep 10
  done
  return 1
}

# close_deployment — close the active Akash deployment. This stops billing and
# the unspent escrow is refunded to the wallet. No-op without an active dseq.
close_deployment() {
  local dseq
  dseq=$(cat "$ACTIVE_DSEQ_FILE" 2>/dev/null || echo "")
  [ -n "$dseq" ] || return 0
  log "Closing deployment $dseq."
  api DELETE "/v1/deployments/$dseq" >/dev/null
  rm -f "$ACTIVE_DSEQ_FILE"
}

# run_backup [do_halt] — safe backup: RCON save -> stream zip -> validate ->
# update restore_target. If do_halt=1 also RCON-quit the server afterwards.
run_backup() {
  local do_halt="${1:-0}"
  resolve_rcon
  if ! mkdir "$BACKUP_LOCK" 2>/dev/null; then
    echo "[backup] another backup is already running — skipping"
    return 1
  fi
  # Release the lock when this function returns (RETURN, not EXIT, so the
  # lock doesn't stay held for the rest of the calling script).
  trap 'rmdir "$BACKUP_LOCK" 2>/dev/null || true; trap - RETURN' RETURN

  local ip port
  ip=$(server_info_val ip "")
  port=$(server_info_val port 2222)
  if [ -z "$ip" ] || [ "$ip" = "pending" ]; then
    echo "[backup] server IP is not configured — aborting backup"
    return 1
  fi
  echo "[backup] starting safe backup from $ip:$port (halt=$do_halt)"

  local ts name
  ts=$(date +%Y%m%d_%H%M%S)
  name="backup_$ts.zip"

  if ! python3 /usr/local/bin/rcon.py "$ip" "$RCON_PORT" "$RCON_PASSWORD" "save"; then
    echo "[backup] ERROR: RCON save failed — aborting to prevent corruption"
    return 1
  fi
  sleep 5

  ssh -p "$port" -o StrictHostKeyChecking=no -o ConnectTimeout="$SSH_CONNECT_TIMEOUT" \
    "steam@$ip" "cd /home/steam/Zomboid/Saves && zip -q -r - ." > "$BACKUP_DIR/$name"
  local exit_code=${PIPESTATUS[0]}

  if [ "$exit_code" -ne 0 ] || [ ! -s "$BACKUP_DIR/$name" ]; then
    echo "[backup] ERROR: backup $name failed or is empty (ssh/zip exit=$exit_code). Removing partial file."
    rm -f "$BACKUP_DIR/$name"
    return 1
  fi

  echo "$(date +%s)" > "$STATE_DIR/last_backup_time"
  (
    cd "$SERVES_REPO" || exit 1
    echo "$name" > restore_target
    git add restore_target
    git commit -m "Created safe backup $name and updated restore_target" || true
    push_with_retry
  )
  echo "[backup] backup $name finished and restore_target updated."

  if [ "$do_halt" = "1" ]; then
    echo "[backup] halt requested — sending quit via RCON..."
    python3 /usr/local/bin/rcon.py "$ip" "$RCON_PORT" "$RCON_PASSWORD" "quit" || true
  fi
  return 0
}

# process_triggers — consume and act on start/backup/halt files (call after
# git_pull_state). Deploy runs in the background; backups run synchronously.
process_triggers() {
  if [ -f "$SERVES_REPO/start" ]; then
    echo "[trigger] deploy trigger detected (start) — consuming and launching deploy"
    consume_file start
    if [ -z "${AKASH_API_KEY:-}" ]; then
      echo "[trigger] WARNING: AKASH_API_KEY is not set — cannot deploy, start consumed anyway."
    else
      # Stream deploy output to BOTH the container stdout (Akash Console logs)
      # and the deploy log file.
      nohup /usr/local/bin/deploy.sh 2>&1 | tee -a "$STATE_DIR/deploy.log" &
      echo "[trigger] deploy.sh started in background (pid $!) — logs follow in the console and $STATE_DIR/deploy.log"
    fi
  fi

  if [ -f "$SERVES_REPO/backup" ]; then
    echo "[trigger] manual backup trigger detected (backup) — consuming"
    consume_file backup
    run_backup 0
  fi

  if [ -f "$SERVES_REPO/halt" ]; then
    echo "[trigger] halt trigger detected (halt) — consuming"
    consume_file halt
    run_backup 1
    # Halt = graceful stop + close the Akash deployment (billing stops, unspent
    # escrow refunded), mirroring the stop_at path in schedule.sh.
    if [ -n "${AKASH_API_KEY:-}" ] && [ -f "$ACTIVE_DSEQ_FILE" ]; then
      if wait_server_stopped; then
        echo "[trigger] server reported 'stopped' — closing deployment to stop billing."
      else
        echo "[trigger] server did not report 'stopped' within ${HALT_CONFIRM_SEC}s — closing deployment anyway."
      fi
      close_deployment
    else
      echo "[trigger] AKASH_API_KEY not set or no active deployment — skipped deployment close."
    fi
  fi
}
