#!/bin/bash

# 1. Setup SSH Server on Port 2222
echo "=== Setting up SSH Server ==="
mkdir -p /run/sshd
echo "Port 2222" >> /etc/ssh/sshd_config
/usr/sbin/sshd

# 2. Setup Steam SSH keys so Autosaver can SSH as steam
mkdir -p /home/steam/.ssh
chmod 700 /home/steam/.ssh
echo "$SSH_PRIVATE_KEY_BASE64" | base64 -d > /home/steam/.ssh/id_rsa
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
    git clone \"$REPO_URL\" /home/steam/pz-saves
else
    cd /home/steam/pz-saves && git pull
fi
"

# 4. Write IP and check restore
MY_IP=$(curl -s ifconfig.me)
gosu steam bash -c "
cd /home/steam/pz-saves
echo \"{\\\"ip\\\": \\\"$MY_IP\\\", \\\"port\\\": 2222, \\\"status\\\": \\\"booting\\\"}\" > server_info.json
git add server_info.json
git commit -m \"Server booting up at $MY_IP\" || true
export GIT_SSH_COMMAND=\"ssh -o StrictHostKeyChecking=no\"
git push || true
"

echo "=== Checking Restore Request ==="
if [ -f /home/steam/pz-saves/restore_target ]; then
    TARGET=$(cat /home/steam/pz-saves/restore_target)
    if [ -n "$TARGET" ]; then
        echo "WAITING FOR AUTOSAVER TO RESTORE: $TARGET"
        # Autosaver connects via SSH, pushes files, extracts, and empties restore_target
        while [ -s /home/steam/pz-saves/restore_target ]; do
            sleep 10
            gosu steam bash -c "cd /home/steam/pz-saves && export GIT_SSH_COMMAND=\"ssh -o StrictHostKeyChecking=no\" && git pull > /dev/null 2>&1"
        done
        echo "Restore complete! Proceeding with startup."
    fi
fi

# 5. Original logic for PZ setup (runs as steam)
echo "=== Setting up Directories ==="
gosu steam mkdir -p /home/steam/Zomboid/Server /home/steam/Zomboid/Saves /home/steam/Zomboid/db /home/steam/Zomboid/mods

echo "=== Linking Workshop Mods ==="
if [ -d /home/steam/pz-server/steamapps/workshop/content/380870 ]; then
    find /home/steam/pz-server/steamapps/workshop/content/380870 -maxdepth 2 -type d -name "mods" | while read -r mod_dir; do
        ln -sf "$mod_dir"/* /home/steam/Zomboid/mods/
    done
    echo "Workshop mods linked successfully."
fi
chown -R steam:steam /home/steam/Zomboid/mods

echo "=== Copying Configs ==="
gosu steam cp /home/steam/vsrania.ini /home/steam/Zomboid/Server/vsrania.ini
gosu steam cp /home/steam/vsrania_SandboxVars.lua /home/steam/Zomboid/Server/vsrania_SandboxVars.lua

echo "=== Auto-configuring Mods in vsrania.ini ==="
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
    if grep -q "^Mods=" /home/steam/Zomboid/Server/vsrania.ini; then
        sed -i "s/^Mods=/Mods=$MODS_LIST;/g" /home/steam/Zomboid/Server/vsrania.ini
    else
        echo "Mods=$MODS_LIST" >> /home/steam/Zomboid/Server/vsrania.ini
    fi
    echo "Added Mods to vsrania.ini: $MODS_LIST"
fi
chown steam:steam /home/steam/Zomboid/Server/vsrania.ini

graceful_shutdown() {
    echo "=== Termination signal received! Shutting down PZ server gracefully... ==="
    if kill -0 $PZ_PID 2>/dev/null; then
        kill -TERM $PZ_PID
        echo "Waiting for server to save local files and exit..."
        wait $PZ_PID
    fi
    # Also mark as offline in Git
    gosu steam bash -c "cd /home/steam/pz-saves && echo \"{\\\"status\\\": \\\"offline\\\"}\" > server_info.json && git add server_info.json && git commit -m \"Server offline\" && export GIT_SSH_COMMAND=\"ssh -o StrictHostKeyChecking=no\" && git push || true"
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
gosu steam "$PZ_PATH" -nosteam -servername vsrania -adminpassword "Qwerty01234**" -cachedir=/home/steam/Zomboid -Xmx8192m -Xms8192m &
PZ_PID=$!

# Let Autosaver know it's online
gosu steam bash -c "cd /home/steam/pz-saves && echo \"{\\\"ip\\\": \\\"$MY_IP\\\", \\\"port\\\": 2222, \\\"status\\\": \\\"online\\\"}\" > server_info.json && git add server_info.json && git commit -m \"Server online\" && export GIT_SSH_COMMAND=\"ssh -o StrictHostKeyChecking=no\" && git push || true"

wait $PZ_PID
EXIT_CODE=$?
echo "=== Project Zomboid Server exited with code $EXIT_CODE ==="

if [ $EXIT_CODE -ne 0 ]; then
    echo "ERROR: Server crashed unexpectedly! Sleeping for 30 minutes to preserve logs for debugging..."
    sleep 1800
fi
