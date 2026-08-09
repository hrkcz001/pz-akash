#!/bin/bash

echo "=== Setting up Directories ==="
mkdir -p /home/steam/Zomboid/Server /home/steam/Zomboid/Saves /home/steam/Zomboid/db /home/steam/git-repo /home/steam/.ssh

echo "=== Copying Configs ==="
cp /home/steam/vsrania.ini /home/steam/Zomboid/Server/vsrania.ini
cp /home/steam/vsrania_SandboxVars.lua /home/steam/Zomboid/Server/vsrania_SandboxVars.lua

# SSH & Git Setup
if [ -n "$SSH_PRIVATE_KEY_BASE64" ]; then
    echo "=== Configuring SSH Key ==="
    echo "$SSH_PRIVATE_KEY_BASE64" | tr -d '\r\n "' | base64 -d -i > /home/steam/.ssh/id_ed25519
    echo "" >> /home/steam/.ssh/id_ed25519
    chmod 600 /home/steam/.ssh/id_ed25519
    ssh-keyscan -H github.com >> /home/steam/.ssh/known_hosts 2>/dev/null || true
    export GIT_SSH_COMMAND="ssh -i /home/steam/.ssh/id_ed25519 -o StrictHostKeyChecking=no"

    echo "=== Setting up Git Save Repository ==="
    git config --global user.name "${GIT_USER_NAME:-PZ AutoSave}"
    git config --global user.email "${GIT_USER_EMAIL:-pz-bot@users.noreply.github.com}"
    
    rm -rf /home/steam/git-repo
    if git clone "$REPO_URL" /home/steam/git-repo; then
        cd /home/steam/git-repo
        
        if [ -n "$SAVE_COMMIT_HASH" ] && [ "$SAVE_COMMIT_HASH" != "latest" ]; then
            git checkout "$SAVE_COMMIT_HASH" || echo "WARNING: Failed to checkout $SAVE_COMMIT_HASH"
        fi

        if [ -f /home/steam/git-repo/save.tar.gz ]; then
            echo "=== Restoring Save Archive from GitHub ==="
            tar -xzf /home/steam/git-repo/save.tar.gz -C /home/steam/Zomboid/ || true
            echo "Save archive unpacked successfully."
        fi
    else
        echo "WARNING: Git clone failed. Starting with fresh local save state."
    fi
fi

# Auto-Save & Git Push Function
save_and_push() {
    echo "=== Auto-saving and pushing save archive to GitHub ==="
    if [ -n "$SSH_PRIVATE_KEY_BASE64" ] && [ -d /home/steam/git-repo/.git ]; then
        export GIT_SSH_COMMAND="ssh -i /home/steam/.ssh/id_ed25519 -o StrictHostKeyChecking=no"
        cd /home/steam/git-repo || return 0
        
        CURRENT_BRANCH=$(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo "main")
        if [ "$CURRENT_BRANCH" = "HEAD" ] || [ -z "$CURRENT_BRANCH" ]; then
            git checkout -b main 2>/dev/null || true
            CURRENT_BRANCH="main"
        fi
        
        tar -czf /home/steam/git-repo/save.tar.gz -C /home/steam/Zomboid/ Saves db || true
        git add save.tar.gz || true
        git commit -m "Auto-save: $(date -u '+%Y-%m-%d %H:%M:%S UTC')" || true
        git push -u origin "$CURRENT_BRANCH" || git push origin "$CURRENT_BRANCH" || echo "Git push failed"
    fi
}

# Trap termination signals
trap 'echo "Termination signal received! Saving world..."; save_and_push; exit 0' SIGTERM SIGINT

# Periodic background auto-save loop (every 20 minutes)
(
    while true; do
        sleep 1200
        save_and_push
    done
) &

echo "=== Starting Project Zomboid Dedicated Server ==="
PZ_PATH=$(find /home/steam/pz-server / -name "start-server.sh" -o -name "StartServer64.sh" 2>/dev/null | head -n 1)
if [ -z "$PZ_PATH" ]; then
    echo "ERROR: Server launch script not found!"
    exit 1
fi
PZ_DIR=$(dirname "$PZ_PATH")
chmod +x "$PZ_PATH" "$PZ_DIR"/ProjectZomboid64 "$PZ_DIR"/jre64/bin/java 2>/dev/null || true

cd "$PZ_DIR"
"$PZ_PATH" -servername vsrania -adminpassword "Qwerty01234**" -cachedir=/home/steam/Zomboid -Xmx6144m -Xms6144m &

PZ_PID=$!
wait $PZ_PID || true
save_and_push
