#!/bin/bash
# =============================================================================
# state.sh — shared helpers for the controller container. Sourced by controller.sh
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
SSH_PORT="${SSH_PORT:-2222}"
SSH_CONNECT_TIMEOUT="${SSH_CONNECT_TIMEOUT:-10}"
BACKUP_LOCK="$STATE_DIR/backup.lock"

# RCON credentials come from the server SDL in the pz-saves repo (single
# source of truth) unless explicitly overridden in the controller env — the
# controller deployment itself carries no server info.
resolve_rcon() {
  if [ -z "$RCON_PASSWORD" ]; then
    if [ -f "$SERVES_REPO/deployment.yaml" ]; then
      RCON_PASSWORD=$(grep -oE 'RCON_PASSWORD=[^[:space:]]+' "$SERVES_REPO/deployment.yaml" 2>/dev/null | head -1 | cut -d= -f2-)
      [ -z "$RCON_PASSWORD" ] && RCON_PASSWORD=$(grep -oE 'STORAGE_PASSWORD=[^[:space:]]+' "$SERVES_REPO/deployment.yaml" 2>/dev/null | head -1 | cut -d= -f2-)
    fi
    RCON_PASSWORD="${RCON_PASSWORD:-}"
  fi
}

GIT_LOCK_DIR="${GIT_LOCK_DIR:-$STATE_DIR/git_repo.lock}"

# Mutex wrapper to prevent concurrent git operations from colliding
with_git_lock() {
  local lockdir="$GIT_LOCK_DIR"
  local timeout=45
  local start_time
  start_time=$(date +%s)
  
  while true; do
    if mkdir "$lockdir" 2>/dev/null; then
      break
    fi
    # If lock directory is older than 60s, stale cleanup
    local age
    age=$(( $(date +%s) - $(stat -c %Y "$lockdir" 2>/dev/null || date +%s) ))
    if [ "$age" -gt 60 ]; then
      rm -rf "$lockdir" 2>/dev/null || true
    fi
    if [ $(( $(date +%s) - start_time )) -ge "$timeout" ]; then
      echo "[state] WARNING: git lock timed out after ${timeout}s - breaking lock." >&2
      rm -rf "$lockdir" 2>/dev/null || true
      mkdir "$lockdir" 2>/dev/null || true
      break
    fi
    sleep 0.4
  done
  
  "$@"
  local ret=$?
  rm -rf "$lockdir" 2>/dev/null || true
  return $ret
}

_push_with_retry_internal() {
  (
    cd "$SERVES_REPO" || return 1
    for i in {1..5}; do
      [ -f .git/index.lock ] && rm -f .git/index.lock
      git push origin HEAD:main >/dev/null 2>&1 && return 0
      git rebase --abort >/dev/null 2>&1 || true
      git merge --abort >/dev/null 2>&1 || true
      git fetch origin main >/dev/null 2>&1 || true
      if ! git pull --rebase origin main >/dev/null 2>&1; then
        git rebase --abort >/dev/null 2>&1 || true
        git checkout -B main origin/main >/dev/null 2>&1 || true
      fi
      sleep $((RANDOM % 3 + 1))
    done
    echo "[state] WARNING: git push failed after 5 retries" >&2
    return 1
  )
}

push_with_retry() {
  with_git_lock _push_with_retry_internal
}

_git_pull_state_internal() {
  (
    cd "$SERVES_REPO" || return 1
    [ -f .git/index.lock ] && rm -f .git/index.lock
    [ -d .git/rebase-apply ] && git rebase --abort >/dev/null 2>&1 || true
    [ -d .git/rebase-merge ] && git rebase --abort >/dev/null 2>&1 || true
    [ -f .git/MERGE_HEAD ] && git merge --abort >/dev/null 2>&1 || true

    for attempt in 1 2 3; do
      if git fetch origin main >/dev/null 2>&1; then
        git reset --hard origin/main >/dev/null 2>&1 || git checkout -B main origin/main >/dev/null 2>&1 || true
        return 0
      fi
      sleep 1
    done
    return 1
  ) || {
    echo "[state] WARNING: git pull in $SERVES_REPO failed — state may be stale" >&2
    return 1
  }
  return 0
}

git_pull_state() {
  with_git_lock _git_pull_state_internal
}

_consume_file_internal() {
  local f="$1"
  (
    cd "$SERVES_REPO" || exit 1
    [ -f "$f" ] || return 0
    git rm -f "$f" >/dev/null 2>&1 || rm -f "$SERVES_REPO/$f"
    git commit -m "Consumed trigger: $f removed" >/dev/null 2>&1 || true
    _push_with_retry_internal
  )
}

consume_file() { # $1 = filename (start|backup|halt|stop_at)
  local f="$1"
  with_git_lock _consume_file_internal "$f"
  echo "[state] consumed trigger file: $f"
}

server_info_val() { # $1 = key, $2 = default
  jq -r --arg k "$1" --arg d "$2" '.[$k] // $d' "$SERVES_REPO/server_info.json" 2>/dev/null || echo "$2"
}

LOCK_FILE="${LOCK_FILE:-$STATE_DIR/deploy.lock}"

# Terminate running deploy process tree if any
kill_running_deploy() {
  if [ -f "$LOCK_FILE" ]; then
    local dpid
    dpid=$(cat "$LOCK_FILE" 2>/dev/null || echo "")
    if [ -n "$dpid" ] && kill -0 "$dpid" 2>/dev/null; then
      echo "[deploy] Terminating running deploy.sh process (PID $dpid)..."
      kill -TERM "$dpid" 2>/dev/null || true
      pkill -P "$dpid" 2>/dev/null || true
      sleep 1
      if kill -0 "$dpid" 2>/dev/null; then
        kill -KILL "$dpid" 2>/dev/null || true
      fi
    fi
    rm -f "$LOCK_FILE"
  fi
  pkill -f "/usr/local/bin/deploy.sh" 2>/dev/null || true
  pkill -f "deploy.sh" 2>/dev/null || true
}

reset_server_info_offline() {
  git_pull_state
  local current_st current_ip
  current_st=$(server_info_val status "offline")
  current_ip=$(server_info_val ip "")
  if [ "$current_st" != "offline" ] || [ -n "$current_ip" ]; then
    echo "{\"ip\": \"\", \"port\": $SSH_PORT, \"game_port\": ${GAME_PORT:-16261}, \"status\": \"offline\", \"players_count\": 0}" > "$SERVES_REPO/server_info.json"
    ( cd "$SERVES_REPO" && git add server_info.json \
      && git commit -m "Server offline - reset status and clear IP" || true \
      && push_with_retry )
    log "server_info.json updated: status=offline, IP cleared."
  fi
}

reset_server_info_stopped() {
  reset_server_info_offline
}

mark_server_stopping() {
  (
    cd "$SERVES_REPO" || return 1
    git_pull_state >/dev/null 2>&1 || true
    local cur_ip cur_port p_hr p_day
    cur_ip=$(jq -r '.ip // empty' server_info.json 2>/dev/null || echo "")
    cur_port=$(jq -r '.port // 2222' server_info.json 2>/dev/null || echo 2222)
    p_hr=$(jq -r '.price_per_hour // empty' server_info.json 2>/dev/null || echo "0.011")
    p_day=$(jq -r '.price_per_day // empty' server_info.json 2>/dev/null || echo "0.26")
    echo "{\"ip\": \"$cur_ip\", \"port\": $cur_port, \"game_port\": ${GAME_PORT:-16261}, \"status\": \"stopping\", \"players_count\": 0, \"price_per_hour\": $p_hr, \"price_per_day\": $p_day}" > "$SERVES_REPO/server_info.json"
    git add server_info.json \
      && git commit -m "Server stopping - graceful shutdown initiated" || true \
      && _push_with_retry_internal
  )
  log "server_info.json updated: status=stopping (SHUTTING DOWN)"
}

mark_server_booting() {
  local p_hr="${1:-}"
  local p_day="${2:-}"
  (
    cd "$SERVES_REPO" || return 1
    git_pull_state >/dev/null 2>&1 || true
    if [ -f server_info.json ]; then
      [ -z "$p_hr" ] && p_hr=$(jq -r '.price_per_hour // empty' server_info.json 2>/dev/null || echo "")
      [ -z "$p_day" ] && p_day=$(jq -r '.price_per_day // empty' server_info.json 2>/dev/null || echo "")
    fi
    p_hr="${p_hr:-0.011}"
    p_day="${p_day:-0.26}"
    echo "{\"ip\": \"pending\", \"port\": $SSH_PORT, \"game_port\": ${GAME_PORT:-16261}, \"status\": \"booting\", \"players_count\": 0, \"price_per_hour\": $p_hr, \"price_per_day\": $p_day}" > "$SERVES_REPO/server_info.json"
    git add server_info.json \
      && git commit -m "Server deployment initiated - status set to booting" || true \
      && _push_with_retry_internal
  )
  log "server_info.json updated: status=booting (STARTING UP)"
}

# --- shared Akash lifecycle helpers (used by both the halt trigger below and
# schedule.sh's stop_at path). schedule.sh shadows log/api with its own
# equivalents — identical behavior, just a different log prefix.
log() { echo "[controller] $(date -u +%FT%TZ) $*"; }

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

# wait_server_offline — poll server_info.json until status == "offline" (or "stopped").
wait_server_offline() {
  local deadline st
  deadline=$(( $(date +%s) + HALT_CONFIRM_SEC ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    git_pull_state
    st=$(server_info_val status "")
    if [ "$st" = "offline" ] || [ "$st" = "stopped" ]; then
      return 0
    fi
    sleep 10
  done
  return 1
}

wait_server_stopped() {
  wait_server_offline
}

# save_active_dseq DSEQ — write dseq to both local state and pz-saves (survives controller redeploy).
save_active_dseq() {
  local dseq="$1"
  echo "$dseq" > "$ACTIVE_DSEQ_FILE"
  (
    cd "$SERVES_REPO" || return 1
    echo "$dseq" > active_dseq
    git add active_dseq
    git commit -m "Track active deployment dseq=$dseq" || true
    _push_with_retry_internal
  )
}

# clear_active_dseq — remove dseq from both local state and pz-saves.
clear_active_dseq() {
  rm -f "$ACTIVE_DSEQ_FILE"
  (
    cd "$SERVES_REPO" || return 1
    if [ -f active_dseq ]; then
      git rm -f active_dseq > /dev/null 2>&1 || rm -f active_dseq
      git commit -m "Clear active_dseq (deployment closed)" || true
      _push_with_retry_internal
    fi
  )
}

# close_deployment [dseq] — close the active Akash deployment. This stops billing and
# the unspent escrow is refunded to the wallet. No-op without an active dseq.
close_deployment() {
  local dseq="${1:-$(cat "$ACTIVE_DSEQ_FILE" 2>/dev/null || echo "")}"
  [ -n "$dseq" ] || return 0
  log "Closing deployment $dseq."
  api DELETE "/v1/deployments/$dseq" >/dev/null
  clear_active_dseq
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

  local ip port st
  ip=$(server_info_val ip "")
  port=$(server_info_val port 2222)
  st=$(server_info_val status "offline")

  # If server is currently booting, wait up to 300s for it to become online before backup
  if [ "$st" = "booting" ] || [ "$ip" = "pending" ]; then
    echo "[backup] server is currently booting — waiting for server to be online before backup..."
    local wait_deadline=$(( $(date +%s) + 300 ))
    while [ "$(date +%s)" -lt "$wait_deadline" ]; do
      git_pull_state
      st=$(server_info_val status "")
      ip=$(server_info_val ip "")
      if [ "$st" = "online" ] && [ -n "$ip" ] && [ "$ip" != "pending" ]; then
        break
      fi
      sleep 10
    done
  fi

  if [ -z "$ip" ] || [ "$ip" = "pending" ]; then
    echo "[backup] server IP is not configured — aborting backup"
    return 1
  fi
  echo "[backup] starting safe backup from $ip:$port (halt=$do_halt)"

  local ts name
  ts=$(date +%Y%m%d_%H%M%S)
  name="backup_$ts.zip"

  # Issue save via RCON with retries
  local rcon_saved=false
  for rcon_try in {1..3}; do
    if python3 /usr/local/bin/rcon.py "$ip" "$RCON_PORT" "$RCON_PASSWORD" "save"; then
      rcon_saved=true
      break
    fi
    sleep 3
  done

  if [ "$rcon_saved" = "false" ]; then
    echo "[backup] WARNING: RCON save failed (server may be starting or busy). Proceeding with SSH backup."
  fi
  # Guaranteed time for save data to flush to disk
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

DESIRED_STATE_FILE="${DESIRED_STATE_FILE:-$STATE_DIR/desired_state}"

# parse_stop_at -> epoch seconds, or empty if no/invalid stop_at
parse_stop_at() {
  [ -f "$SERVES_REPO/stop_at" ] || return 0
  python3 - "$(cat "$SERVES_REPO/stop_at" 2>/dev/null | tr -d '\r\n')" <<'PYEOF'
import datetime, sys
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

set_desired_state() { # $1 = "running" | "stopped"
  local st="$1"
  echo "$st" > "$DESIRED_STATE_FILE"
  (
    cd "$SERVES_REPO" || return 0
    echo "$st" > desired_state
    git add desired_state
    git commit -m "Update desired_state=$st" || true
    _push_with_retry_internal
  )
}

get_desired_state() {
  # If stop_at date is reached, desired state is unconditionally stopped
  local stop_epoch now
  stop_epoch=$(parse_stop_at 2>/dev/null || echo "")
  if [ -n "$stop_epoch" ]; then
    now=$(date +%s)
    if [ "$now" -ge "$stop_epoch" ]; then
      echo "stopped"
      return 0
    fi
  fi

  # If halt trigger file exists in repo, desired state is stopped
  if [ -f "$SERVES_REPO/halt" ]; then
    echo "stopped"
    return 0
  fi

  if [ -f "$DESIRED_STATE_FILE" ]; then
    cat "$DESIRED_STATE_FILE" | tr -d '\r\n'
    return 0
  fi
  if [ -f "$SERVES_REPO/desired_state" ]; then
    cat "$SERVES_REPO/desired_state" | tr -d '\r\n'
    return 0
  fi
  if [ -f "$ACTIVE_DSEQ_FILE" ]; then
    echo "running"
    return 0
  fi
  echo "stopped"
}

trigger_deploy() { # $1 = reason
  local reason="${1:-deploy requested}"
  if [ -z "${AKASH_API_KEY:-}" ]; then
    echo "[deploy] WARNING: AKASH_API_KEY is not set — cannot deploy."
    return 1
  fi

  # Check if previous deploy process is actively running
  if [ -f "$LOCK_FILE" ]; then
    local old_pid
    old_pid=$(cat "$LOCK_FILE" 2>/dev/null || echo "")
    if [ -n "$old_pid" ] && kill -0 "$old_pid" 2>/dev/null; then
      echo "[deploy] deploy.sh is already running (pid $old_pid) — skipping duplicate launch."
      return 0
    fi
  fi

  # Cooldown check to prevent runaway loops (at least 20s between starts)
  local last_dep now diff
  last_dep=$(cat "$STATE_DIR/last_redeploy_time" 2>/dev/null || echo "0")
  now=$(date +%s)
  diff=$((now - last_dep))
  if [ "$diff" -lt 20 ]; then
    echo "[deploy] Redeploy cooldown active (${diff}s < 20s) — skipping duplicate trigger."
    return 0
  fi
  echo "$now" > "$STATE_DIR/last_redeploy_time"

  echo "[deploy] Launching deploy.sh in background (reason: $reason)..."
  nohup /usr/local/bin/deploy.sh 2>&1 | tee -a "$STATE_DIR/deploy.log" &
  echo "[deploy] deploy.sh started in background (pid $!)"
}

# process_triggers — consume and act on start/backup/halt files (call after
# git_pull_state). Deploy runs in the background; backups run synchronously.
process_triggers() {
  local has_start=false has_backup=false has_halt=false
  [ -f "$SERVES_REPO/start" ] && has_start=true
  [ -f "$SERVES_REPO/backup" ] && has_backup=true
  [ -f "$SERVES_REPO/halt" ] && has_halt=true

  # Priority 1: start (deploy)
  if [ "$has_start" = "true" ]; then
    if [ "$has_halt" = "true" ]; then
      # If start and halt were pushed simultaneously, halt takes precedence
      echo "[trigger] start and halt detected simultaneously — cancelling start trigger in favor of halt."
      consume_file start
    else
      echo "[trigger] deploy trigger detected (start) — setting desired_state=running and launching deploy"
      set_desired_state "running"
      mark_server_booting
      consume_file start
      trigger_deploy "start trigger"
    fi
  fi

  # Priority 2: backup (guaranteed safe backup before halt)
  if [ "$has_backup" = "true" ]; then
    echo "[trigger] manual backup trigger detected (backup) — executing safe backup"
    local cur_st cur_ip
    cur_st=$(server_info_val status "offline")
    cur_ip=$(server_info_val ip "")

    if { [ "$cur_st" = "offline" ] || [ "$cur_st" = "stopped" ]; } && [ -z "$cur_ip" ] && [ ! -f "$ACTIVE_DSEQ_FILE" ]; then
      echo "[trigger] server is completely offline and no deployment exists — consuming backup trigger."
      consume_file backup
    else
      if run_backup 0; then
        consume_file backup
      else
        echo "[trigger] backup execution did not complete — keeping backup trigger for retry."
      fi
    fi
  fi

  # Priority 3: halt (guarantees server is stopped and closed)
  if [ "$has_halt" = "true" ]; then
    echo "[trigger] halt trigger detected (halt) — executing graceful stop and teardown"
    set_desired_state "stopped"

    # Immediately terminate any in-flight deploy.sh process so it releases lock and stops polling
    kill_running_deploy

    # Mark server as stopping immediately so UI and status reflect it
    mark_server_stopping

    # Run safe backup with halt flag (RCON save -> stream zip -> RCON quit)
    run_backup 1 || true

    # Close the Akash deployment (billing stops, unspent escrow refunded)
    if [ -n "${AKASH_API_KEY:-}" ] && [ -f "$ACTIVE_DSEQ_FILE" ]; then
      if wait_server_offline; then
        echo "[trigger] server reported 'offline' — closing deployment to stop billing."
      else
        echo "[trigger] server did not report 'offline' within ${HALT_CONFIRM_SEC}s — closing deployment anyway."
      fi
      close_deployment
    else
      echo "[trigger] AKASH_API_KEY not set or no active deployment — skipped deployment close."
    fi

    # Reset server_info.json to offline and clean up pending IP
    reset_server_info_offline

    consume_file halt
  fi
}
