#!/bin/bash

# Configuration variables
SSH_PORT=${SSH_PORT:-2222}
RESTORE_POLL_INTERVAL_SEC=${RESTORE_POLL_INTERVAL_SEC:-10}
SERVER_NAME=${SERVER_NAME:-vsrania}
ADMIN_PASSWORD=${ADMIN_PASSWORD:-"Qwerty01234**"}
SERVER_MEMORY_MAX=${SERVER_MEMORY_MAX:-8192m}
SERVER_MEMORY_MIN=${SERVER_MEMORY_MIN:-8192m}
WAIT_ON_CRASH_SEC=${WAIT_ON_CRASH_SEC:-1800}
AUTO_CONFIGURE_MODS=${AUTO_CONFIGURE_MODS:-true}
AUTO_CONFIGURE_MAPS=${AUTO_CONFIGURE_MAPS:-true}

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
echo "=== Syncing with Git ==="
chown -R steam:steam /home/steam
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

# 5. Original logic for PZ setup (runs as steam)
echo "=== Linking Workshop Mods ==="
WORKSHOP_ROOT="/home/steam/pz-server/steamapps/workshop/content/108600"
MOD_LINK_COUNT=0
if [ -d "$WORKSHOP_ROOT" ]; then
    # Search up to depth 4 to handle varied workshop mod structures:
    #   <item_id>/mods/<ModName>/           (standard)
    #   <item_id>/Contents/mods/<ModName>/  (some mods wrap in Contents/)
    # Using relative symlinks (-r) avoids double-path bugs where
    # ScriptManager.Load prefixes the mods dir onto an already-absolute path.
    while IFS= read -r mod_dir; do
        while IFS= read -r mod_content; do
            link_name="/home/steam/Zomboid/mods/$(basename "$mod_content")"
            if [ ! -e "$link_name" ]; then
                ln -srf "$mod_content" "$link_name"
                MOD_LINK_COUNT=$((MOD_LINK_COUNT + 1))
            else
                echo "  [skip] $(basename "$mod_content") already linked"
            fi
        done < <(find "$mod_dir" -maxdepth 1 -mindepth 1 -type d 2>/dev/null)
    done < <(find "$WORKSHOP_ROOT" -maxdepth 4 -type d -name "mods" 2>/dev/null)
    echo "Workshop mods linked: $MOD_LINK_COUNT mod folder(s)."
else
    echo "WARNING: Workshop content directory not found at $WORKSHOP_ROOT — no mods will be loaded."
fi
chown -R steam:steam /home/steam/Zomboid/mods

# Patch: damnlib ships its content under 42.17/ but the game (42.20) expects
# the mod's active content under 42.20/. Rename the real directory (the mods
# dir entry is a symlink into the workshop content folder, so realpath first).
# Scoped to damnlib only — no other mod is installed.
echo "=== Patching damnlib version directory (42.17 -> 42.20) ==="
DAMNLIB_REAL=$(realpath /home/steam/Zomboid/mods/damnlib 2>/dev/null || true)
if [ -n "$DAMNLIB_REAL" ] && [ -d "$DAMNLIB_REAL/42.17" ]; then
    mv "$DAMNLIB_REAL/42.17" "$DAMNLIB_REAL/42.20"
    echo "  Renamed $DAMNLIB_REAL/42.17 -> 42.20"
elif [ -n "$DAMNLIB_REAL" ] && [ -d "$DAMNLIB_REAL/42.20" ]; then
    echo "  damnlib/42.20 already exists, no patch needed."
else
    echo "  WARNING: damnlib mod not found or 42.17 dir missing — skipping patch."
fi

echo "=== Copying Configs ==="
gosu steam cp /home/steam/vsrania.ini /home/steam/Zomboid/Server/${SERVER_NAME}.ini
gosu steam cp /home/steam/vsrania_SandboxVars.lua /home/steam/Zomboid/Server/${SERVER_NAME}_SandboxVars.lua

if [ "$AUTO_CONFIGURE_MODS" = "true" ]; then
    echo "=== Auto-configuring Mods in ${SERVER_NAME}.ini ==="
    # 1. Build a set of all installed mod IDs by scanning mod directories
    declare -A INSTALLED_MODS
    for d in /home/steam/Zomboid/mods/*; do
        if [ -d "$d" ]; then
            # Read the canonical mod ID from mod.info
            # PZ mod.info uses "id=" as the field name (not "modId=")
            # Build 42 mods often place their mod.info in a version subfolder
            # (42/ or 42.0/). The game prioritizes these, so we should too.
            MOD_ID=""
            MOD_INFO_FILE=""
            # Find the highest 42.x version subfolder that contains a mod.info.
            # Handles 42, 42.0, 42.1, 42.20, etc. — picks the highest via version sort.
            # NOTE: We avoid find, ls -d, and even glob expansion on "$d"/42.*
            # because directory names with shell glob characters (e.g. "[B42] Mod
            # Manager") cause the brackets to be interpreted as character classes
            # during pathname expansion. Instead, we loop over ALL children of $d
            # and filter by basename regex — no globbing touches the parent path.
            BEST_VERSION_DIR=""
            if [ -d "$d" ]; then
                BEST_VERSION_DIR=$(
                    for entry in "$d"/*/; do
                        [ -d "$entry" ] || continue
                        bname=$(basename "$entry")
                        case "$bname" in
                            42|42.*) echo "$entry" ;;
                        esac
                    done | sort -V -r | while IFS= read -r vdir; do
                        if [ -f "$vdir/mod.info" ]; then
                            printf '%s' "$vdir"
                            break
                        fi
                    done
                )
            fi
            if [ -n "$BEST_VERSION_DIR" ]; then
                MOD_INFO_FILE="$BEST_VERSION_DIR/mod.info"
            fi
            # Fall back to root mod.info (legacy/B41 style)
            if [ -z "$MOD_INFO_FILE" ] && [ -f "$d/mod.info" ]; then
                MOD_INFO_FILE="$d/mod.info"
            fi
            # Fall back to common/mod.info as last resort (non-standard location,
            # but some mods like CommonSense, better-auto-mechanics use it)
            if [ -z "$MOD_INFO_FILE" ] && [ -f "$d/common/mod.info" ]; then
                MOD_INFO_FILE="$d/common/mod.info"
            fi
            if [ -n "$MOD_INFO_FILE" ]; then
                # Try "id=" first (standard PZ format), then "modId=" (legacy)
                # Strip UTF-8 BOM (\xEF\xBB\xBF) and tolerate leading whitespace
                MOD_ID=$(sed 's/^\xEF\xBB\xBF//' "$MOD_INFO_FILE" | grep -i "^\s*id=" | head -n1 | sed 's/^[[:space:]]*[iI][dD]=//' | tr -d '\r\n ')
                if [ -z "$MOD_ID" ]; then
                    MOD_ID=$(sed 's/^\xEF\xBB\xBF//' "$MOD_INFO_FILE" | grep -i "^\s*modId=" | head -n1 | sed 's/^[[:space:]]*[mM][oO][dD][iI][dD]=//' | tr -d '\r\n ')
                fi
            fi
            # Fallback to folder name if no mod.info found or has no id line
            if [ -z "$MOD_ID" ]; then
                MOD_ID=$(basename "$d")
                echo "  [warn] No id= in mod.info for $d, using folder name: $MOD_ID"
            fi
            INSTALLED_MODS["$MOD_ID"]=1
        fi
    done
    echo "  Discovered ${#INSTALLED_MODS[@]} installed mod(s)"

    # 2. Reconcile with the declared Mods= line: preserve order, strip missing, append new
    INI_FILE="/home/steam/Zomboid/Server/${SERVER_NAME}.ini"
    MODS_LINE_EXISTS=$(grep -c "^Mods=" "$INI_FILE")
    EXISTING_MODS_LINE=$(grep "^Mods=" "$INI_FILE" | head -n1 | cut -d= -f2-)

    FINAL_LIST=""
    declare -A SEEN_MODS

    if [ -n "$EXISTING_MODS_LINE" ]; then
        # Walk the declared list: keep entries that are installed, drop the rest
        REMOVED=""
        IFS=';' read -ra DECLARED <<< "$EXISTING_MODS_LINE"
        for MOD in "${DECLARED[@]}"; do
            MOD=$(echo "$MOD" | xargs)  # trim whitespace
            [ -z "$MOD" ] && continue
            if [ -n "${INSTALLED_MODS[$MOD]+x}" ]; then
                if [ -z "$FINAL_LIST" ]; then FINAL_LIST="$MOD"; else FINAL_LIST="$FINAL_LIST;$MOD"; fi
                SEEN_MODS["$MOD"]=1
            else
                REMOVED="$REMOVED $MOD"
            fi
        done
        if [ -n "$REMOVED" ]; then
            echo "  [removed] Not installed:$REMOVED"
        fi
    fi

    # Append any installed mods not already in the list
    APPENDED=""
    for MOD in "${!INSTALLED_MODS[@]}"; do
        if [ -z "${SEEN_MODS[$MOD]+x}" ]; then
            if [ -z "$FINAL_LIST" ]; then FINAL_LIST="$MOD"; else FINAL_LIST="$FINAL_LIST;$MOD"; fi
            APPENDED="$APPENDED $MOD"
        fi
    done
    if [ -n "$APPENDED" ]; then
        echo "  [appended] New mods:$APPENDED"
    fi

    # Write the final Mods= line (replace in-place if it exists, otherwise append)
    INI_FILE="/home/steam/Zomboid/Server/${SERVER_NAME}.ini"
    MODS_LINE_EXISTS=$(grep -c "^Mods=" "$INI_FILE")
    if [ "$MODS_LINE_EXISTS" -gt 0 ] 2>/dev/null; then
        sed -i "s/^Mods=.*/Mods=$FINAL_LIST/" "$INI_FILE"
    else
        echo "Mods=$FINAL_LIST" >> "$INI_FILE"
    fi
    echo "  Mods= finalized ($(echo "$FINAL_LIST" | tr ';' '\n' | wc -l) mods)"
    chown steam:steam "$INI_FILE"
else
    echo "=== Skipping Mods auto-configuration (AUTO_CONFIGURE_MODS=false) ==="
fi

if [ "$AUTO_CONFIGURE_MAPS" = "true" ]; then
    echo "=== Auto-configuring Maps in ${SERVER_NAME}.ini ==="
    # Discover maps provided by installed mods by scanning <mod>/common/media/maps/
    # Prioritize the highest 42.x versioned subfolder if present.
    declare -A INSTALLED_MAPS
    for d in /home/steam/Zomboid/mods/*; do
        [ -d "$d" ] || continue
        MEDIA_MAPS_DIR=""
        # Check highest 42.x versioned subfolder first
        # NOTE: Same bracket-safe approach as the mods section above.
        BEST_VERSION_DIR=""
        BEST_VERSION_DIR=$(
            for entry in "$d"/*/; do
                [ -d "$entry" ] || continue
                bname=$(basename "$entry")
                case "$bname" in
                    42|42.*) echo "$entry" ;;
                esac
            done | sort -V -r | head -n1
        )
        if [ -n "$BEST_VERSION_DIR" ] && [ -d "$BEST_VERSION_DIR/common/media/maps" ]; then
            MEDIA_MAPS_DIR="$BEST_VERSION_DIR/common/media/maps"
        elif [ -d "$d/common/media/maps" ]; then
            MEDIA_MAPS_DIR="$d/common/media/maps"
        fi
        if [ -n "$MEDIA_MAPS_DIR" ]; then
            for map_entry in "$MEDIA_MAPS_DIR"/*/; do
                [ -d "$map_entry" ] || continue
                MAP_NAME=$(basename "$map_entry")
                if [ -z "${INSTALLED_MAPS[$MAP_NAME]+x}" ]; then
                    INSTALLED_MAPS["$MAP_NAME"]=1
                    echo "  [found] Map '$MAP_NAME' -> $map_entry"
                fi
            done
        fi
    done
    echo "  Discovered ${#INSTALLED_MAPS[@]} mod map(s)"

    # Reconcile with the declared Map= line: preserve order, strip missing, append new
    # "Muldraugh, KY" is the vanilla base map and is always appended LAST.
    # PZ loads maps left-to-right; mod maps must come first for proper priority.
    INI_FILE="/home/steam/Zomboid/Server/${SERVER_NAME}.ini"
    MAP_LINE_EXISTS=$(grep -ci "^Map=" "$INI_FILE")
    EXISTING_MAP_LINE=$(grep -i "^Map=" "$INI_FILE" | head -n1 | cut -d= -f2-)

    MAP_FINAL=""
    declare -A SEEN_MAPS

    if [ -n "$EXISTING_MAP_LINE" ]; then
        # Walk the existing list: keep entries that still exist, drop the rest
        MAP_FINAL=""
        MAP_REMOVED=""
        IFS=';' read -ra DECLARED_MAPS <<< "$EXISTING_MAP_LINE"
        for MAP in "${DECLARED_MAPS[@]}"; do
            MAP=$(echo "$MAP" | xargs)  # trim whitespace
            [ -z "$MAP" ] && continue
            # Skip vanilla map here; it will be appended at the end
            [ "$MAP" = "Muldraugh, KY" ] && continue
            if [ -n "${INSTALLED_MAPS[$MAP]+x}" ]; then
                if [ -z "$MAP_FINAL" ]; then MAP_FINAL="$MAP"; else MAP_FINAL="$MAP_FINAL;$MAP"; fi
                SEEN_MAPS["$MAP"]=1
            else
                MAP_REMOVED="$MAP_REMOVED $MAP"
            fi
        done

        if [ -n "$MAP_REMOVED" ]; then
            echo "  [removed] Maps not found:$MAP_REMOVED"
        fi
    fi

    # Append any discovered maps not already in the list
    MAP_APPENDED=""
    for MAP in "${!INSTALLED_MAPS[@]}"; do
        if [ -z "${SEEN_MAPS[$MAP]+x}" ]; then
            if [ -z "$MAP_FINAL" ]; then MAP_FINAL="$MAP"; else MAP_FINAL="$MAP_FINAL;$MAP"; fi
            MAP_APPENDED="$MAP_APPENDED $MAP"
        fi
    done
    if [ -n "$MAP_APPENDED" ]; then
        echo "  [appended] New maps:$MAP_APPENDED"
    fi

    # Always append vanilla base map last
    if [ -z "$MAP_FINAL" ]; then MAP_FINAL="Muldraugh, KY"; else MAP_FINAL="$MAP_FINAL;Muldraugh, KY"; fi

    # Write the final Map= line (replace in-place if it exists, otherwise append)
    if [ "$MAP_LINE_EXISTS" -gt 0 ] 2>/dev/null; then
        sed -i "s/^Map=.*/Map=$MAP_FINAL/" "$INI_FILE"
    else
        echo "Map=$MAP_FINAL" >> "$INI_FILE"
    fi
    echo "  Map= finalized ($(echo "$MAP_FINAL" | tr ';' '\n' | wc -l) entries)"
else
    echo "=== Skipping Maps auto-configuration (AUTO_CONFIGURE_MAPS=false) ==="
fi

# === Fix B42 Linux mod case-sensitivity (DamnLib et al.) ===
# Since B42.13 PZ lowercases mod file paths internally (scripts, checksums,
# animsets). Mods like DamnLib ship uppercase names (airBrake/, AnimSets/,
# DAMN_Client.lua, ...) which then can't be found on Linux's case-sensitive
# filesystem. Create lowercase symlink aliases for every file and directory
# in each installed mod. Safe to re-run (skips existing symlinks).
echo "=== Fixing mod case-sensitivity for Linux (B42 regression) ==="

create_lowercase_aliases() {
    local root="$1"
    [ -d "$root" ] || return 0
    local entry parent name lower link_path
    while IFS= read -r -d '' entry; do
        [ -L "$entry" ] && continue
        parent=$(dirname "$entry")
        name=$(basename "$entry")
        lower=$(printf '%s' "$name" | tr '[:upper:]' '[:lower:]')
        [ "$name" = "$lower" ] && continue
        link_path="$parent/$lower"
        # Don't clobber a real (non-symlink) entry that already exists
        if [ -e "$link_path" ] && [ ! -L "$link_path" ]; then
            continue
        fi
        ln -sfn "$name" "$link_path" 2>/dev/null || true
    done < <(find "$root" -not -type l -print0 2>/dev/null | sort -z)
}

ALIASED=0
for d in /home/steam/Zomboid/mods/*/; do
    [ -d "$d" ] || continue
    create_lowercase_aliases "$d"
    ALIASED=$((ALIASED + 1))
done
echo "  Applied lowercase aliases to $ALIASED mod folder(s)."

# PZ also lowercases the absolute mods-dir path; make the lowercased form
# resolve back to the real directory (this path contains 'Zomboid').
ln -sfn /home/steam/Zomboid /home/steam/zomboid 2>/dev/null || true

# Sanity check: the exact DamnLib paths the game crashed on should now resolve:
# 1. the ScriptManager script path (case-sensitive dirs)
# 2. the AdvancedAnimator animset path its checksum build failed on (version dir)
if find -L /home/steam/Zomboid/mods/damnlib -type f -path "*/media/scripts/airbrake/template_airbrake.txt" 2>/dev/null | grep -q . \
   && find -L /home/steam/Zomboid/mods/damnlib -type f -path "*/media/animsets/player-vehicle/enter/damn_enter.xml" 2>/dev/null | grep -q .; then
    echo "  [ok] DamnLib script + animset paths resolve"
else
    echo "  [warn] DamnLib script/animset paths still NOT found (check mod layout)"
fi

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
chown -R steam:steam /home/steam/pz-server

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
