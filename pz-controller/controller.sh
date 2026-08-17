#!/bin/bash
echo "=== Starting Project Zomboid Controller Service ==="

# Shared helpers (git state, consume-on-trigger files, safe backup)
source /usr/local/bin/state.sh

# 1. Setup SSH key
mkdir -p /root/.ssh
chmod 700 /root/.ssh
echo "$SSH_PRIVATE_KEY_BASE64" | tr -d ' "\r\n' | base64 -d > /root/.ssh/id_rsa
chmod 600 /root/.ssh/id_rsa
export GIT_SSH_COMMAND="ssh -o StrictHostKeyChecking=no"

git config --global user.name "${GIT_USER_NAME:-pz-controller}"
git config --global user.email "${GIT_USER_EMAIL:-pz-controller@localhost}"

# Configuration variables
HTTP_PORT=${HTTP_PORT:-8000}   # file/storage server — MUST differ from WEBHOOK_PORT
BACKUP_INTERVAL_SEC=${BACKUP_INTERVAL_SEC:-3600}
BACKUP_RETENTION_DAYS=${BACKUP_RETENTION_DAYS:-7}
# RCON_PASSWORD/RCON_PORT are resolved by state.sh from the server SDL in
# pz-saves — the controller deployment carries no server info.
AUTOSAVER_POLL_SEC=${AUTOSAVER_POLL_SEC:-60}
SSH_CONNECT_TIMEOUT=${SSH_CONNECT_TIMEOUT:-10}
# Webhook mode: WEBHOOK_ENABLED starts the listener; WEBHOOK_MODE=true makes
# the loop rely on webhooks and only poll as a slow safety net.
WEBHOOK_ENABLED=${WEBHOOK_ENABLED:-true}
WEBHOOK_MODE=${WEBHOOK_MODE:-false}
WEBHOOK_POLL_SEC=${WEBHOOK_POLL_SEC:-300}
WEBHOOK_PORT=${WEBHOOK_PORT:-8080}
WEBHOOK_PID_FILE=${WEBHOOK_PID_FILE:-/data/webhook.pid}
WEBHOOK_HEARTBEAT_SEC=${WEBHOOK_HEARTBEAT_SEC:-600}
STORAGE_SERVER_PID_FILE=${STORAGE_SERVER_PID_FILE:-/data/storage_server.pid}

if [ ! -d /root/pz-saves ]; then
    git clone "$REPO_URL" /root/pz-saves || { echo "ERROR: Git clone failed"; exit 1; }
fi

if [ "$HTTP_PORT" = "$WEBHOOK_PORT" ]; then
    echo "ERROR: HTTP_PORT ($HTTP_PORT) and WEBHOOK_PORT ($WEBHOOK_PORT) must be different — the storage server and the webhook listener cannot share a port. Fix the env and restart."
    exit 1
fi

mkdir -p /data/backups /data/packages

echo "=== Starting HTTP Storage & Dashboard Server on port $HTTP_PORT ==="
python3 /usr/local/bin/storage_server.py 2>&1 | tee -a /data/storage_server.log &
echo $! > "$STORAGE_SERVER_PID_FILE"

if [ "$WEBHOOK_ENABLED" = "true" ]; then
    echo "=== Starting GitHub webhook listener on port $WEBHOOK_PORT ==="
    nohup python3 /usr/local/bin/webhook.py 2>&1 | tee -a /data/webhook.log &
    echo "webhook listener started (pid $!) — logs follow in the console and /data/webhook.log"
fi

# Best-effort background URL resolver: resolves public endpoints for Storage (:8000)
# and Webhook (:8080), logging them and updating controller_info.json in pz-saves.
(
    for i in 1 2 3 4 5 6 7 8; do
        sleep 25
        STORAGE_URL=$(/usr/local/bin/controller_url.sh "$HTTP_PORT" 2>/dev/null || true)
        WH_URL=$(/usr/local/bin/controller_url.sh "$WEBHOOK_PORT" 2>/dev/null || true)
        if [ -n "$STORAGE_URL" ]; then
            echo "================================================================="
            echo "  PUBLIC CONTROLLER STORAGE URL: $STORAGE_URL"
            [ -n "$WH_URL" ] && echo "  PUBLIC CONTROLLER WEBHOOK URL: $WH_URL/webhook"
            echo "================================================================="
            
            # Automatically update Cloudflare dynamic redirect if token is configured
            if [ -n "${CLOUDFLARE_API_TOKEN:-}" ]; then
                python3 /usr/local/bin/update_cloudflare.py "$STORAGE_URL" || true
            fi

            # Publish to pz-saves so game server and tools automatically discover controller
            cd /root/pz-saves
            git pull >/dev/null 2>&1 || true
            echo "{\"storage_url\": \"$STORAGE_URL\", \"webhook_url\": \"${WH_URL:-}\", \"updated_at\": $(date +%s)}" > controller_info.json
            git add controller_info.json
            git commit -m "Update controller_info.json with live storage URL: $STORAGE_URL" || true
            push_with_retry
            exit 0
        fi
    done
    echo "CONTROLLER URL: not resolved via Akash Console API yet — check the Akash Console lease status."
) 2>&1 | tee -a /data/webhook.log &

# webhook_watchdog — keep the GitHub webhook listener alive and flag URL drift.
# server_watchdogs — keep storage server & webhook listener alive and flag URL drift.
server_watchdogs() {
    # 1. Storage server watchdog
    if ! curl -sf --max-time 5 "http://127.0.0.1:$HTTP_PORT/healthz" >/dev/null 2>&1; then
        echo "[controller] WARNING: storage server not answering on :$HTTP_PORT — restarting."
        if [ -f "$STORAGE_SERVER_PID_FILE" ]; then
            kill "$(cat "$STORAGE_SERVER_PID_FILE")" 2>/dev/null || true
            rm -f "$STORAGE_SERVER_PID_FILE"
        fi
        nohup python3 /usr/local/bin/storage_server.py 2>&1 | tee -a /data/storage_server.log &
        echo $! > "$STORAGE_SERVER_PID_FILE"
        echo "[controller] storage server restarted (pid $!)."
    fi

    [ "$WEBHOOK_ENABLED" = "true" ] || return 0

    # 2. Webhook listener watchdog
    if ! curl -sf --max-time 5 "http://127.0.0.1:$WEBHOOK_PORT/healthz" >/dev/null 2>&1; then
        echo "[controller] WARNING: webhook listener not answering on :$WEBHOOK_PORT — restarting."
        if [ -f "$WEBHOOK_PID_FILE" ]; then
            kill "$(cat "$WEBHOOK_PID_FILE")" 2>/dev/null || true
            rm -f "$WEBHOOK_PID_FILE"
        fi
        nohup python3 /usr/local/bin/webhook.py 2>&1 | tee -a /data/webhook.log &
        echo "[controller] webhook listener restarted (pid $!)."
    fi

    # 3. URL drift: shared-endpoint external ports can be re-mapped by the
    #    provider; GitHub deliveries then fail until the webhook URL is
    #    updated. Re-resolve the URL and warn on change (at most every 10 min).
    [ -n "${AKASH_API_KEY:-}" ] || return 0
    local last_check now url
    last_check=$(cat /data/webhook_url.check 2>/dev/null || echo 0)
    now=$(date +%s)
    [ $((now - last_check)) -ge 600 ] || return 0
    echo "$now" > /data/webhook_url.check
    url=$(/usr/local/bin/webhook_url.sh 2>/dev/null || true)
    [ -n "$url" ] || return 0
    if [ -f /data/webhook_url ] && [ "$(cat /data/webhook_url)" != "$url" ]; then
        echo "[controller] WARNING: webhook URL changed to $url — update it in GitHub -> pz-saves -> Settings -> Webhooks, or deliveries keep failing."
    fi
    echo "$url" > /data/webhook_url
}

cd /root/pz-saves
# Ensure files exist to avoid errors
touch backup_log restore_target server_info.json
git add .
git commit -m "Initialize state files" || true
push_with_retry

echo "=== Entering Controller Loop ==="
while true; do
    cd /root/pz-saves

    # Sync state + consume trigger files (start/backup/halt). In webhook mode
    # this still runs as a slow safety net; webhooks make it near-instant.
    git_pull_state
    process_triggers

    # Scheduled stop + escrow top-up (no-ops unless stop_at / active deployment)
    /usr/local/bin/schedule.sh 2>&1 | tee -a /data/schedule.log

    # Keep storage server & webhook listener alive + flag URL drift
    server_watchdogs

    IS_PAUSED=false
    if [ -f pause_autosave ] && grep -iq "true" pause_autosave; then
        IS_PAUSED=true
        echo "Controller autosave is paused (automatic backups suspended)."
    fi
    
    if [ -f server_info.json ]; then
        STATUS=$(jq -r '.status' server_info.json)
        SERVER_IP=$(jq -r '.ip' server_info.json)
        SERVER_PORT=$(jq -r '.port' server_info.json)
        
        if [ "$SERVER_IP" = "pending" ]; then
            echo "Server IP is pending configuration. Waiting..."
        elif [ "$STATUS" = "booting" ] || [ "$STATUS" = "online" ]; then
            # Check if restore is requested and server is ready
            if [ -f request_restore ] && grep -iq "ready" request_restore; then
                if [ -f restore_target ]; then
                    TARGET=$(cat restore_target)
                    echo "Server is ready. Restoring: $TARGET to $SERVER_IP:$SERVER_PORT"
                    if [ -f "/data/backups/$TARGET" ]; then
                        echo "Uploading and extracting backup..."
                        # Server is ready, so SCP should connect immediately.
                        for i in {1..5}; do
                            scp -P $SERVER_PORT -o StrictHostKeyChecking=no -o ConnectTimeout=$SSH_CONNECT_TIMEOUT /data/backups/$TARGET steam@$SERVER_IP:/tmp/ && break || sleep 5
                        done
                        
                        ssh -p $SERVER_PORT -o StrictHostKeyChecking=no steam@$SERVER_IP "mkdir -p /home/steam/Zomboid/Saves && cd /home/steam/Zomboid/Saves && rm -rf * && unzip -o /tmp/$TARGET && rm /tmp/$TARGET"
                        
                        echo "Restore completed."
                        echo "Clearing request_restore completely"
                        rm -f request_restore
                        git rm request_restore 2>/dev/null || true
                        
                        git commit -m "Restore of $TARGET completed and request_restore cleared" || true
                    else
                        echo "ERROR: Backup $TARGET not found in /data/backups/"
                        
                        echo "failed" > request_restore
                        git add request_restore
                        
                        git commit -m "Restore failed: $TARGET not found" || true
                    fi
                    push_with_retry
                fi
            fi
            
            # Periodic backup only — manual backups (`backup`) and halts (`halt`)
            # are handled by process_triggers.
            if [ "$STATUS" = "online" ]; then
                CURRENT_TIME=$(date +%s)
                LAST_BACKUP=$(cat /data/last_backup_time 2>/dev/null || echo "0")
                DIFF=$((CURRENT_TIME - LAST_BACKUP))
                
                if [ "$IS_PAUSED" = "false" ] && [ $DIFF -gt $BACKUP_INTERVAL_SEC ]; then
                    echo "Time for periodic backup."
                    run_backup 0
                fi
            fi
        fi
    fi
    
    # Cleanup old backups
    find /data/backups -name "backup_*.zip" -type f -mtime +$BACKUP_RETENTION_DAYS -delete

    # Actualize backup_log to only contain existing zip files
    ls -1 /data/backups | grep '\.zip$' | sort -r > backup_log.tmp
    if ! cmp -s backup_log backup_log.tmp; then
        mv backup_log.tmp backup_log
        git add backup_log
        git commit -m "Update backup_log to match available backups" || true
        push_with_retry
    else
        rm -f backup_log.tmp
    fi

    if [ "$WEBHOOK_MODE" = "true" ]; then
        # Webhook-driven: poll only as a slow safety net.
        sleep $WEBHOOK_POLL_SEC
    else
        sleep $AUTOSAVER_POLL_SEC
    fi
done
