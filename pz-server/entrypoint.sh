#!/bin/bash

# Configuration variables
SSH_PORT=${SSH_PORT:-2222}
RESTORE_POLL_INTERVAL_SEC=${RESTORE_POLL_INTERVAL_SEC:-10}
SERVER_NAME=${SERVER_NAME:-vsrania}
ADMIN_PASSWORD=${ADMIN_PASSWORD:-"Qwerty01234**"}
STORAGE_PASSWORD=${STORAGE_PASSWORD:-$ADMIN_PASSWORD}
CONTROLLER_URL=${CONTROLLER_URL:-}
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
if [ -z "$SSH_PRIVATE_KEY_BASE64" ]; then
    echo "ERROR: SSH_PRIVATE_KEY_BASE64 is not set! Git sync will fail."
fi
echo "  Decoding SSH private key..."
echo "$SSH_PRIVATE_KEY_BASE64" | tr -d ' "\r\n' | base64 -d > /home/steam/.ssh/id_rsa
chmod 600 /home/steam/.ssh/id_rsa
echo "  Validating SSH private key..."
if ! ssh-keygen -y -f /home/steam/.ssh/id_rsa > /home/steam/.ssh/id_rsa.pub 2>&1; then
    echo "ERROR: SSH private key is invalid or corrupted! Check SSH_PRIVATE_KEY_BASE64."
else
    echo "  SSH key OK."
fi
cat /home/steam/.ssh/id_rsa.pub > /home/steam/.ssh/authorized_keys
chmod 600 /home/steam/.ssh/authorized_keys
chown -R steam:steam /home/steam/.ssh

# 3. Setup git and clone repo (as steam)
# NOTE: no `chown -R /home/steam` here — the image already owns everything
# steam:steam (Dockerfile COPY --chown) and a recursive chown over the
# multi-GB game tree at boot costs minutes. The only root-created paths
# (.ssh, server.log) get explicit chowns below.
echo "=== Syncing with Git ==="
echo "  REPO_URL: ${REPO_URL:-NOT SET}"
echo "  GIT_USER_NAME: ${GIT_USER_NAME:-NOT SET}"
gosu steam bash -c "
export GIT_SSH_COMMAND=\"ssh -o StrictHostKeyChecking=no -v -i /home/steam/.ssh/id_rsa\"
git config --global user.name \"$GIT_USER_NAME\"
git config --global user.email \"$GIT_USER_EMAIL\"
if [ ! -d /home/steam/pz-saves ]; then
    echo \"  Cloning repo from $REPO_URL ...\"
    git clone \"$REPO_URL\" /home/steam/pz-saves || { echo \"ERROR: Git clone failed (exit \$?)\"; exit 1; }
    echo \"  Clone complete.\"
else
    echo \"  Pulling latest from existing repo...\"
    cd /home/steam/pz-saves && git pull && echo \"  Pull complete.\"
fi
" || echo "ERROR: Git sync subshell failed with exit code $?"

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
if [ -f server_info.json ]; then
    CURRENT_IP=\$(jq -r '.ip // \"pending\"' server_info.json)
    if [ \"\$CURRENT_IP\" = \"null\" ] || [ -z \"\$CURRENT_IP\" ]; then CURRENT_IP=\"pending\"; fi
    PREV_STATUS=\$(jq -r '.status // \"unknown\"' server_info.json)
    # A previous run that ended (stopped/error) means the recorded IP belongs to
    # the old Akash lease - it will not come back. Reset to pending so we wait
    # for the new deployment's IP instead of reusing a dead one.
    if [ \"\$PREV_STATUS\" = \"stopped\" ] || [ \"\$PREV_STATUS\" = \"error\" ]; then
        echo \"Previous run ended with status '\$PREV_STATUS' - discarding stale IP, waiting for new one.\"
        CURRENT_IP=\"pending\"
    fi
else
    CURRENT_IP=\"pending\"
fi
echo \"{\\\"ip\\\": \\\"\$CURRENT_IP\\\", \\\"port\\\": $SSH_PORT, \\\"status\\\": \\\"booting\\\"}\" > server_info.json
git add server_info.json
git commit -m \"Server booting up, status set to booting\" || true
export GIT_SSH_COMMAND=\"ssh -o StrictHostKeyChecking=no\"
push_with_retry

echo \"Checking IP configuration in server_info.json...\"
while true; do
    if [ \"\$CURRENT_IP\" != \"pending\" ] && [ \"\$CURRENT_IP\" != \"null\" ] && [ -n \"\$CURRENT_IP\" ]; then
        break
    fi
    echo \"Waiting for user to replace 'pending' with the real IP in server_info.json...\"
    sleep 10
    git pull >/dev/null 2>&1
    if [ -f server_info.json ]; then
        CURRENT_IP=\$(jq -r '.ip' server_info.json)
    fi
done
echo \"IP configured as \$CURRENT_IP. Continuing boot...\"
"

# 4. Setup Directories (runs as steam)
echo "=== Setting up Directories ==="
gosu steam mkdir -p /home/steam/Zomboid/Server /home/steam/Zomboid/Saves /home/steam/Zomboid/db /home/steam/Zomboid/mods

# The game reads mods from ~/Zomboid/mods (capital Z, hardcoded from $HOME),
# but builds some internal paths (checksum/animsets) in lowercase ~/zomboid
# regardless of -cachedir. Point both names at the SAME real directory
# (/home/steam/Zomboid). ext4 does not support directory hardlinks, so this is
# a symlink — the game follows it fine (the old lowercase aliases in earlier
# boots proved that).
if [ -e /home/steam/zomboid ] && [ ! -L /home/steam/zomboid ]; then
    echo "  Removing leftover real directory /home/steam/zomboid (replaced by symlink to Zomboid)"
    rm -rf /home/steam/zomboid
fi
ln -sfn /home/steam/Zomboid /home/steam/zomboid
echo "  Linked /home/steam/zomboid -> /home/steam/Zomboid"

echo "=== Checking for Existing Backup to Restore ==="
if [ -f /home/steam/pz-saves/restore_target ]; then
    TARGET=$(cat /home/steam/pz-saves/restore_target | tr -d '\n' | tr -d '\r')
    if [ -n "$TARGET" ]; then
        echo "Found existing backup target: $TARGET. Changing state to 'ready' to notify autosaver..."
        gosu steam bash -c "
cd /home/steam/pz-saves
push_with_retry() { for i in {1..5}; do git push && return 0; git pull --rebase >/dev/null 2>&1; sleep \$((RANDOM % 3 + 1)); done; }
echo \"ready\" > request_restore
export GIT_SSH_COMMAND=\"ssh -o StrictHostKeyChecking=no\"
git add request_restore
git commit -m \"Server ready for restore\" || true
push_with_retry
"
        echo "WAITING FOR AUTOSAVER TO RESTORE: $TARGET"
        while [ -f /home/steam/pz-saves/request_restore ]; do
            CURRENT_STATE=$(cat /home/steam/pz-saves/request_restore | tr -d '\n' | tr '[:upper:]' '[:lower:]' | tr -d '\r')
            if [ "$CURRENT_STATE" = "failed" ]; then
                echo "CRITICAL ERROR: Autosaver reported that the backup file was not found!"
                echo "Aborting startup to prevent overwriting your save data with a fresh world."
                gosu steam bash -c "
cd /home/steam/pz-saves
export GIT_SSH_COMMAND=\"ssh -o StrictHostKeyChecking=no\"
CURRENT_IP=\$(jq -r '.ip // \"pending\"' server_info.json)
CURRENT_PORT=\$(jq -r '.port // 0' server_info.json)
echo \"{\\\"ip\\\": \\\"\$CURRENT_IP\\\", \\\"port\\\": \$CURRENT_PORT, \\\"status\\\": \\\"error\\\"}\" > server_info.json
git add server_info.json
git commit -m \"Server startup aborted due to missing backup\" || true
push_with_retry
"
                echo "Sleeping for 1800 seconds (30 minutes) to preserve logs and prevent restart loops..."
                sleep 1800
                exit 1
            fi
            sleep $RESTORE_POLL_INTERVAL_SEC
            gosu steam bash -c "cd /home/steam/pz-saves && export GIT_SSH_COMMAND=\"ssh -o StrictHostKeyChecking=no\" && git pull > /dev/null 2>&1"
        done
        echo "Restore complete! Proceeding with startup."
    fi
fi

# 5. Install mods & sync configurations (runs as steam)
echo "=== Syncing Packages & Configs from Controller / Repo ==="

# 1. Resolve Controller URL: env var -> controller_info.json in pz-saves
RESOLVED_CONTROLLER_URL="${CONTROLLER_URL:-}"
if [ -z "$RESOLVED_CONTROLLER_URL" ] || [ "$RESOLVED_CONTROLLER_URL" = "http://controller:8000" ]; then
    if [ -f /home/steam/pz-saves/controller_info.json ]; then
        DISCOVERED_URL=$(jq -r '.storage_url // empty' /home/steam/pz-saves/controller_info.json 2>/dev/null)
        if [ -n "$DISCOVERED_URL" ] && [ "$DISCOVERED_URL" != "null" ]; then
            RESOLVED_CONTROLLER_URL="$DISCOVERED_URL"
            echo "  Auto-discovered controller URL from controller_info.json: $RESOLVED_CONTROLLER_URL"
        fi
    fi
fi

# 2. If controller URL is known, download common.zip & server.zip with retry and validation
if [ -n "$RESOLVED_CONTROLLER_URL" ]; then
    echo "  Connecting to Controller at $RESOLVED_CONTROLLER_URL ..."
    
    # Wait for controller /healthz to be ready (up to 30s)
    for i in {1..6}; do
        if curl -sf --max-time 5 "$RESOLVED_CONTROLLER_URL/healthz" >/dev/null 2>&1; then
            echo "  Controller health check OK."
            break
        fi
        echo "  Waiting for Controller health endpoint ($i/6)..."
        sleep 5
    done

    # Fetch common.zip
    echo "  Downloading common.zip..."
    for attempt in {1..3}; do
        if curl -sSL -f --max-time 180 "$RESOLVED_CONTROLLER_URL/common.zip" -o /tmp/common.zip 2>/dev/null; then
            if gosu steam unzip -t -q /tmp/common.zip >/dev/null 2>&1; then
                echo "  Extracting common.zip into /home/steam/Zomboid/..."
                gosu steam unzip -o -q /tmp/common.zip -d /home/steam/Zomboid/
                rm -f /tmp/common.zip
                echo "  common.zip installed successfully."
                break
            fi
        fi
        echo "  [warn] common.zip download attempt $attempt failed, retrying in 3s..."
        sleep 3
    done
    rm -f /tmp/common.zip

    # Fetch server.zip (authenticated)
    echo "  Downloading server.zip (authenticated)..."
    for attempt in {1..3}; do
        if curl -sSL -f --max-time 180 -H "Authorization: Bearer $STORAGE_PASSWORD" "$RESOLVED_CONTROLLER_URL/server.zip" -o /tmp/server.zip 2>/dev/null; then
            if gosu steam unzip -t -q /tmp/server.zip >/dev/null 2>&1; then
                echo "  Extracting server.zip into /home/steam/Zomboid/..."
                gosu steam unzip -o -q /tmp/server.zip -d /home/steam/Zomboid/
                rm -f /tmp/server.zip
                echo "  server.zip installed successfully."
                break
            fi
        fi
        echo "  [warn] server.zip download attempt $attempt failed (check STORAGE_PASSWORD or network), retrying in 3s..."
        sleep 3
    done
    rm -f /tmp/server.zip
else
    echo "  [info] No CONTROLLER_URL configured or discovered in controller_info.json. Relying on local repo files."
fi

# Also merge any files directly placed in pz-saves/common or pz-saves/server
if [ -d /home/steam/pz-saves/common ]; then
    echo "  Merging pz-saves/common into /home/steam/Zomboid/..."
    gosu steam cp -r /home/steam/pz-saves/common/. /home/steam/Zomboid/ 2>/dev/null || true
fi
if [ -d /home/steam/pz-saves/server ]; then
    echo "  Merging pz-saves/server into /home/steam/Zomboid/..."
    gosu steam cp -r /home/steam/pz-saves/server/. /home/steam/Zomboid/ 2>/dev/null || true
fi

# Ensure server .ini exists
INI_FILE="/home/steam/Zomboid/Server/${SERVER_NAME}.ini"
if [ ! -f "$INI_FILE" ]; then
    FALLBACK_INI=$(find /home/steam/Zomboid/Server -name "*.ini" 2>/dev/null | head -n 1)
    if [ -n "$FALLBACK_INI" ] && [ "$FALLBACK_INI" != "$INI_FILE" ]; then
        echo "  Copying $FALLBACK_INI -> $INI_FILE"
        gosu steam cp "$FALLBACK_INI" "$INI_FILE"
    elif [ -f /home/steam/vsrania.ini ]; then
        gosu steam cp /home/steam/vsrania.ini "$INI_FILE"
    fi
fi

SANDBOX_FILE="/home/steam/Zomboid/Server/${SERVER_NAME}_SandboxVars.lua"
if [ ! -f "$SANDBOX_FILE" ]; then
    FALLBACK_LUA=$(find /home/steam/Zomboid/Server -name "*SandboxVars.lua" 2>/dev/null | head -n 1)
    if [ -n "$FALLBACK_LUA" ] && [ "$FALLBACK_LUA" != "$SANDBOX_FILE" ]; then
        echo "  Copying $FALLBACK_LUA -> $SANDBOX_FILE"
        gosu steam cp "$FALLBACK_LUA" "$SANDBOX_FILE"
    elif [ -f /home/steam/vsrania_SandboxVars.lua ]; then
        gosu steam cp /home/steam/vsrania_SandboxVars.lua "$SANDBOX_FILE"
    fi
fi

# Mod and Map configuration (.ini) is fully generated and packaged by pz-controller.

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
    echo "Sleeping for 1800 seconds (30 minutes) to prevent immediate Akash restart loop..."
    sleep 1800
    exit 0
}

trap graceful_shutdown SIGTERM SIGINT

echo "=== Starting Project Zomboid Dedicated Server ==="
PZ_PATH=$(find /home/steam/pz-server -name "start-server.sh" -o -name "StartServer64.sh" 2>/dev/null | head -n 1)
if [ -z "$PZ_PATH" ]; then
    echo "ERROR: Server launch script not found!"
    exit 1
fi
PZ_DIR=$(dirname "$PZ_PATH")
chmod +x "$PZ_PATH" "$PZ_DIR"/ProjectZomboid64 "$PZ_DIR"/jre64/bin/java 2>/dev/null || true
# No `chown -R /home/steam/pz-server` — the image already owns it steam:steam
# (COPY --chown); a recursive chown over the whole game tree at boot costs
# minutes. The one file patched below (ProjectZomboid64.json) is re-chowned
# individually.

JSON_CONFIG="$PZ_DIR/ProjectZomboid64.json"
if [ -f "$JSON_CONFIG" ]; then
    echo "=== Patching ProjectZomboid64.json ==="
    # Disable Steam (required for -nosteam mode)
    sed -i 's/-Dzomboid.steam=1/-Dzomboid.steam=0/g' "$JSON_CONFIG"
    # Inject memory settings as proper JVM vmArgs.
    # NOTE: -Xmx/-Xms must NOT be passed as CLI args to the PZ launcher — pzexe
    # does not forward unknown CLI options to the JVM and logs "unknown option".
    # The JSON vmArgs array is the correct place for JVM flags.
    # Remove any existing Xmx/Xms entries first, then inject the configured values.
    if command -v python3 &>/dev/null; then
        python3 - "$JSON_CONFIG" "$SERVER_MEMORY_MAX" "$SERVER_MEMORY_MIN" <<'PYEOF'
import sys, json
path, xmx, xms = sys.argv[1], sys.argv[2], sys.argv[3]
with open(path) as f:
    cfg = json.load(f)
vmArgs = cfg.get("vmArgs", [])
# Strip any existing heap size flags
vmArgs = [a for a in vmArgs if not a.startswith("-Xmx") and not a.startswith("-Xms")]
vmArgs.extend(["-Xmx" + xmx, "-Xms" + xms])
cfg["vmArgs"] = vmArgs
with open(path, "w") as f:
    json.dump(cfg, f, indent=2)
print(f"Memory set: -Xmx{xmx} -Xms{xms} in {path}")
PYEOF
    else
        echo "WARNING: python3 not available, skipping memory vmArgs injection into JSON. Using JSON defaults."
    fi
    # The patch above ran as root — hand the file back to steam (single file,
    # cheap; avoids a full-tree chown).
    chown steam:steam "$JSON_CONFIG"
else
    echo "WARNING: ProjectZomboid64.json not found, relying on launch flags only."
fi

cd "$PZ_DIR"

# Ensure log file exists before launch (prevents tail race condition)
touch /home/steam/server.log
chown steam:steam /home/steam/server.log

# Launch without -Xmx/-Xms CLI flags — those are unknown to pzexe and ignored.
# Memory is now correctly set in ProjectZomboid64.json vmArgs above.
gosu steam "$PZ_PATH" -nosteam -servername "$SERVER_NAME" -adminpassword "$ADMIN_PASSWORD" -cachedir=/home/steam/Zomboid > /home/steam/server.log 2>&1 &
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
    echo "ERROR: Server crashed unexpectedly! Marking as stopped and sleeping for $WAIT_ON_CRASH_SEC seconds to preserve logs..."
    mark_server_stopped
    sleep $WAIT_ON_CRASH_SEC
else
    echo "Server exited cleanly. Marking as stopped..."
    mark_server_stopped
    echo "Sleeping for 1800 seconds (30 minutes) to prevent immediate Akash restart loop..."
    sleep 1800
fi
