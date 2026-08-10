#!/bin/bash

# Configuration variables
SSH_PORT=${SSH_PORT:-2222}
RESTORE_POLL_INTERVAL_SEC=${RESTORE_POLL_INTERVAL_SEC:-10}
SERVER_NAME=${SERVER_NAME:-vsrania}
ADMIN_PASSWORD=${ADMIN_PASSWORD:-"Qwerty01234**"}
SERVER_MEMORY_MAX=${SERVER_MEMORY_MAX:-8192m}
SERVER_MEMORY_MIN=${SERVER_MEMORY_MIN:-8192m}
WAIT_ON_CRASH_SEC=${WAIT_ON_CRASH_SEC:-1800}

# 1. Setup SSH Server
echo "=== Setting up SSH Server ==="
mkdir -p /run/sshd
echo "Port $SSH_PORT" >> /etc/ssh/sshd_config
/usr/sbin/sshd

# 2. Setup Steam SSH keys so Autosaver can SSH as steam
mkdir -p /home/steam/.ssh
chmod 700 /home/steam/.ssh
echo "$SSH_PRIVATE_KEY_BASE64" | tr -d ' "\r\n' | base64 -d > /home/steam/.ssh/id_rsa
chmod 600 /home/steam/.ssh/id_rsa
ssh-keygen -y -f /home/steam/.ssh/id_rsa > /home/steam/.ssh/id_rsa.pub
cat /home/steam/.ssh/id_rsa.pub > /home/steam/.ssh/authorized_keys
chmod 600 /home/steam/.ssh/authorized_keys
chown -R steam:steam /home/steam/.ssh

# 3. Setup git and clone repo (as steam)
echo "=== Syncing with Git ==="
chown -R steam:steam /home/steam
gosu steam bash -c "
export GIT_SSH_COMMAND=\"ssh -o StrictHostKeyChecking=no\"
git config --global user.name \"$GIT_USER_NAME\"
git config --global user.email \"$GIT_USER_EMAIL\"
if [ ! -d /home/steam/pz-saves ]; then
    git clone \"$REPO_URL\" /home/steam/pz-saves || { echo \"ERROR: Git clone failed\"; exit 1; }
else
    cd /home/steam/pz-saves && git pull
fi

push_with_retry() {
    for i in {1..5}; do
        git push && return 0
        git pull --rebase >/dev/null 2>&1
        sleep \$((RANDOM % 3 + 1))
    done
    echo \"WARNING: Git push failed after 5 retries\"
}
"

gosu steam bash -c "
cd /home/steam/pz-saves
# Define push_with_retry here as well for this isolated subshell
push_with_retry() {
    for i in {1..5}; do
        git push && return 0
        git pull --rebase >/dev/null 2>&1
        sleep \$((RANDOM % 3 + 1))
    done
}
echo \"{\\\"ip\\\": \\\"pending\\\", \\\"port\\\": $SSH_PORT, \\\"status\\\": \\\"booting\\\"}\" > server_info.json
git add server_info.json
git commit -m \"Server booting up, waiting for IP configuration\" || true
export GIT_SSH_COMMAND=\"ssh -o StrictHostKeyChecking=no\"
push_with_retry

echo \"Waiting for user to replace 'pending' with the real IP in server_info.json...\"
while true; do
    git pull >/dev/null 2>&1
    if [ -f server_info.json ]; then
        CURRENT_IP=\$(jq -r '.ip' server_info.json)
        if [ \"\$CURRENT_IP\" != \"pending\" ] && [ \"\$CURRENT_IP\" != \"null\" ] && [ -n \"\$CURRENT_IP\" ]; then
            break
        fi
    fi
    sleep 10
done
echo \"IP configured as \$CURRENT_IP. Continuing boot...\"
"

echo "=== Checking Restore Request ==="
if [ -f /home/steam/pz-saves/request_restore ]; then
    RESTORE_STATE=$(cat /home/steam/pz-saves/request_restore | tr -d '\n' | tr '[:upper:]' '[:lower:]')
    
    if [ "$RESTORE_STATE" = "requested" ] || [ "$RESTORE_STATE" = "true" ]; then
        echo "Restore requested. Changing state to 'ready' to notify autosaver..."
        gosu steam bash -c "
cd /home/steam/pz-saves
push_with_retry() { for i in {1..5}; do git push && return 0; git pull --rebase >/dev/null 2>&1; sleep \$((RANDOM % 3 + 1)); done; }
echo \"ready\" > request_restore
export GIT_SSH_COMMAND=\"ssh -o StrictHostKeyChecking=no\"
git add request_restore
git commit -m \"Server ready for restore\" || true
push_with_retry
"
        RESTORE_STATE="ready"
    fi
    
    if [ "$RESTORE_STATE" = "ready" ]; then
        if [ -f /home/steam/pz-saves/restore_target ]; then
            TARGET=$(cat /home/steam/pz-saves/restore_target)
            if [ -n "$TARGET" ]; then
                echo "WAITING FOR AUTOSAVER TO RESTORE: $TARGET"
                while [ -f /home/steam/pz-saves/request_restore ]; do
                    CURRENT_STATE=$(cat /home/steam/pz-saves/request_restore | tr -d '\n' | tr '[:upper:]' '[:lower:]')
                    if [ "$CURRENT_STATE" != "ready" ]; then
                        break
                    fi
                    sleep $RESTORE_POLL_INTERVAL_SEC
                    gosu steam bash -c "cd /home/steam/pz-saves && export GIT_SSH_COMMAND=\"ssh -o StrictHostKeyChecking=no\" && git pull > /dev/null 2>&1"
                done
                echo "Restore complete! Proceeding with startup."
            fi
        fi
    fi
fi

# 5. Original logic for PZ setup (runs as steam)
echo "=== Setting up Directories ==="
gosu steam mkdir -p /home/steam/Zomboid/Server /home/steam/Zomboid/Saves /home/steam/Zomboid/db /home/steam/Zomboid/mods

echo "=== Linking Workshop Mods ==="
if [ -d /home/steam/pz-server/steamapps/workshop/content/108600 ]; then
    find /home/steam/pz-server/steamapps/workshop/content/108600 -maxdepth 2 -type d -name "mods" | while read -r mod_dir; do
        ln -sf "$mod_dir"/* /home/steam/Zomboid/mods/
    done
    echo "Workshop mods linked successfully."
fi
chown -R steam:steam /home/steam/Zomboid/mods

echo "=== Copying Configs ==="
gosu steam cp /home/steam/vsrania.ini /home/steam/Zomboid/Server/${SERVER_NAME}.ini
gosu steam cp /home/steam/vsrania_SandboxVars.lua /home/steam/Zomboid/Server/${SERVER_NAME}_SandboxVars.lua

echo "=== Auto-configuring Mods in ${SERVER_NAME}.ini ==="
MODS_LIST=""
for d in /home/steam/Zomboid/mods/*; do
    if [ -d "$d" ]; then
        MOD_ID=$(basename "$d")
        if [ -z "$MODS_LIST" ]; then
            MODS_LIST="$MOD_ID"
        else
            MODS_LIST="$MODS_LIST;$MOD_ID"
        fi
    fi
done

if [ -n "$MODS_LIST" ]; then
    if grep -q "^Mods=" /home/steam/Zomboid/Server/${SERVER_NAME}.ini; then
        sed -i "s/^Mods=/Mods=$MODS_LIST;/g" /home/steam/Zomboid/Server/${SERVER_NAME}.ini
    else
        echo "Mods=$MODS_LIST" >> /home/steam/Zomboid/Server/${SERVER_NAME}.ini
    fi
    echo "Added Mods to ${SERVER_NAME}.ini: $MODS_LIST"
fi
chown steam:steam /home/steam/Zomboid/Server/${SERVER_NAME}.ini

mark_server_stopped() {
    # Mark as stopped and request restore on next boot
    gosu steam bash -c "
cd /home/steam/pz-saves
export GIT_SSH_COMMAND=\"ssh -o StrictHostKeyChecking=no\"
push_with_retry() { for i in {1..5}; do git push && return 0; git pull --rebase >/dev/null 2>&1; sleep \$((RANDOM % 3 + 1)); done; }
if [ -f server_info.json ]; then
    CURRENT_IP=\$(jq -r '.ip // \"pending\"' server_info.json)
    CURRENT_PORT=\$(jq -r '.port // 0' server_info.json)
else
    CURRENT_IP=\"pending\"
    CURRENT_PORT=0
fi
echo \"{\\\"ip\\\": \\\"\$CURRENT_IP\\\", \\\"port\\\": \$CURRENT_PORT, \\\"status\\\": \\\"stopped\\\"}\" > server_info.json
echo \"requested\" > request_restore
git add server_info.json request_restore
git commit -m \"Server stopped, auto-restore requested\" || true
push_with_retry
"
}

graceful_shutdown() {
    echo "=== Termination signal received! Shutting down PZ server gracefully... ==="
    if kill -0 $PZ_PID 2>/dev/null; then
        kill -TERM $PZ_PID
        echo "Waiting for server to save local files and exit..."
        wait $PZ_PID
    fi
    mark_server_stopped
    exit 0
}

trap graceful_shutdown SIGTERM SIGINT

echo "=== Starting Project Zomboid Dedicated Server ==="
PZ_PATH=$(find /home/steam/pz-server / -name "start-server.sh" -o -name "StartServer64.sh" 2>/dev/null | head -n 1)
if [ -z "$PZ_PATH" ]; then
    echo "ERROR: Server launch script not found!"
    exit 1
fi
PZ_DIR=$(dirname "$PZ_PATH")
chmod +x "$PZ_PATH" "$PZ_DIR"/ProjectZomboid64 "$PZ_DIR"/jre64/bin/java 2>/dev/null || true
chown -R steam:steam /home/steam/pz-server

JSON_CONFIG="$PZ_DIR/ProjectZomboid64.json"
if [ -f "$JSON_CONFIG" ]; then
    echo "=== Disabling Steam in ProjectZomboid64.json ==="
    sed -i 's/-Dzomboid.steam=1/-Dzomboid.steam=0/g' "$JSON_CONFIG"
else
    echo "WARNING: ProjectZomboid64.json not found, relying on launch flags only."
fi

cd "$PZ_DIR"


gosu steam "$PZ_PATH" -nosteam -servername "$SERVER_NAME" -adminpassword "$ADMIN_PASSWORD" -cachedir=/home/steam/Zomboid -Xmx$SERVER_MEMORY_MAX -Xms$SERVER_MEMORY_MIN > /home/steam/server.log 2>&1 &
PZ_PID=$!

tail -f /home/steam/server.log &
TAIL_PID=$!

echo "Waiting for server to be fully started..."
while ! grep -q "\*\*\* SERVER STARTED \*\*\*" /home/steam/server.log; do
    if ! kill -0 $PZ_PID 2>/dev/null; then
        echo "Server process died during startup!"
        break
    fi
    sleep 2
done

if kill -0 $PZ_PID 2>/dev/null; then
    echo "Server is fully started. Marking as online..."
    # Let Autosaver know it's online
    gosu steam bash -c "
cd /home/steam/pz-saves
export GIT_SSH_COMMAND=\"ssh -o StrictHostKeyChecking=no\"
CURRENT_IP=\$(jq -r '.ip' server_info.json)

echo \"Marking server as online at \$CURRENT_IP!\"
push_with_retry() { for i in {1..5}; do git push && return 0; git pull --rebase >/dev/null 2>&1; sleep \$((RANDOM % 3 + 1)); done; }
echo \"{\\\"ip\\\": \\\"\$CURRENT_IP\\\", \\\"port\\\": $SSH_PORT, \\\"status\\\": \\\"online\\\"}\" > server_info.json
git add server_info.json
git commit -m \"Server online with IP \$CURRENT_IP\" || true
push_with_retry
"
fi

wait $PZ_PID
EXIT_CODE=$?
kill $TAIL_PID 2>/dev/null || true
echo "=== Project Zomboid Server exited with code $EXIT_CODE ==="

if [ $EXIT_CODE -ne 0 ]; then
    echo "ERROR: Server crashed unexpectedly! Sleeping for $WAIT_ON_CRASH_SEC seconds to preserve logs for debugging..."
    sleep $WAIT_ON_CRASH_SEC
else
    echo "Server exited cleanly. Marking as stopped..."
    mark_server_stopped
fi
