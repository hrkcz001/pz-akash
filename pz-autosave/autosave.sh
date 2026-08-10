#!/bin/bash
echo "=== Starting Zomboid Autosave Service ==="

# 1. Setup SSH key
mkdir -p /root/.ssh
chmod 700 /root/.ssh
echo "$SSH_PRIVATE_KEY_BASE64" | base64 -d > /root/.ssh/id_rsa
chmod 600 /root/.ssh/id_rsa
export GIT_SSH_COMMAND="ssh -o StrictHostKeyChecking=no"

git config --global user.name "${GIT_USER_NAME:-autosaver}"
git config --global user.email "${GIT_USER_EMAIL:-autosaver@localhost}"

# Configuration variables
HTTP_PORT=${HTTP_PORT:-80}
BACKUP_INTERVAL_SEC=${BACKUP_INTERVAL_SEC:-3600}
BACKUP_RETENTION_DAYS=${BACKUP_RETENTION_DAYS:-7}
RCON_PASSWORD=${RCON_PASSWORD:-"Qwerty0123**"}
RCON_PORT=${RCON_PORT:-27015}
AUTOSAVER_POLL_SEC=${AUTOSAVER_POLL_SEC:-60}
SSH_CONNECT_TIMEOUT=${SSH_CONNECT_TIMEOUT:-10}


if [ ! -d /root/pz-saves ]; then
    git clone "$REPO_URL" /root/pz-saves || { echo "ERROR: Git clone failed"; exit 1; }
fi

push_with_retry() {
    for i in {1..5}; do
        git push && return 0
        git pull --rebase >/dev/null 2>&1
        sleep $((RANDOM % 3 + 1))
    done
    echo "WARNING: Git push failed after 5 retries"
}

mkdir -p /data/backups
cd /data/backups
echo "=== Starting HTTP File & Upload Server on port $HTTP_PORT ==="
python3 -m uploadserver $HTTP_PORT &

cd /root/pz-saves
# Ensure files exist to avoid errors
touch backup_log restore_target backup_request server_info.json
git add .
git commit -m "Initialize state files" || true
push_with_retry

echo "=== Entering Autosaver Loop ==="
while true; do
    cd /root/pz-saves
    git pull >/dev/null 2>&1
    
    IS_PAUSED=false
    if [ -f pause_autosave ] && grep -iq "true" pause_autosave; then
        IS_PAUSED=true
        echo "Autosaver is paused (automatic backups suspended)."
    fi
    
    if [ -f server_info.json ]; then
        STATUS=$(jq -r '.status' server_info.json)
        SERVER_IP=$(jq -r '.ip' server_info.json)
        SERVER_PORT=$(jq -r '.port' server_info.json)
        
        if [ "$STATUS" = "booting" ] || [ "$STATUS" = "online" ]; then
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
                        
                        ssh -p $SERVER_PORT -o StrictHostKeyChecking=no steam@$SERVER_IP "cd /home/steam/Zomboid/Saves && rm -rf * && unzip -o /tmp/$TARGET && rm /tmp/$TARGET"
                        
                        git commit -m "Restore of $TARGET completed" || true
                        echo "Restore completed."
                    else
                        echo "ERROR: Backup $TARGET not found in /data/backups/"
                        git commit -m "Restore failed: $TARGET not found" || true
                    fi
                    echo "Clearing request_restore completely"
                    rm -f request_restore
                    git rm request_restore 2>/dev/null || true
                    push_with_retry
                fi
            fi
            
            # Check if manual backup requested OR time for periodic backup
            CURRENT_TIME=$(date +%s)
            LAST_BACKUP=$(cat /data/last_backup_time 2>/dev/null || echo "0")
            DIFF=$((CURRENT_TIME - LAST_BACKUP))
            
            if [ -s backup_request ] || { [ "$IS_PAUSED" = "false" ] && [ $DIFF -gt $BACKUP_INTERVAL_SEC ]; }; then
                echo "Starting safe manual/periodic backup from $SERVER_IP:$SERVER_PORT"
                TIMESTAMP=$(date +%Y%m%d_%H%M%S)
                BACKUP_NAME="backup_$TIMESTAMP.zip"
                
                # Send save command via RCON
                if ! python3 /usr/local/bin/rcon.py "$SERVER_IP" "$RCON_PORT" "$RCON_PASSWORD" "save"; then
                    echo "ERROR: RCON save command failed. Aborting backup to prevent corruption."
                    continue
                fi
                
                # Wait for disk flush
                sleep 5
                
                # Stream zip safely directly from server to local disk
                ssh -p $SERVER_PORT -o StrictHostKeyChecking=no steam@$SERVER_IP "cd /home/steam/Zomboid/Saves && zip -q -r - ." > /data/backups/$BACKUP_NAME
                
                echo $CURRENT_TIME > /data/last_backup_time
                echo $BACKUP_NAME >> backup_log
                
                # Auto-set the latest backup into restore_target
                echo $BACKUP_NAME > restore_target
                
                > backup_request
                git add backup_log backup_request restore_target
                git commit -m "Created safe backup $BACKUP_NAME and updated restore_target" || true
                push_with_retry
                
                echo "Backup $BACKUP_NAME finished and logged."
            fi
        fi
    fi
    
    # Cleanup old backups
    find /data/backups -name "backup_*.zip" -type f -mtime +$BACKUP_RETENTION_DAYS -delete

    sleep $AUTOSAVER_POLL_SEC
done
