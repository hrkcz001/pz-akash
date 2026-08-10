#!/bin/bash
echo "=== Starting Zomboid Autosave Service ==="

# 1. Setup SSH key
mkdir -p /root/.ssh
chmod 700 /root/.ssh
echo "$SSH_PRIVATE_KEY_BASE64" | base64 -d > /root/.ssh/id_rsa
chmod 600 /root/.ssh/id_rsa
export GIT_SSH_COMMAND="ssh -o StrictHostKeyChecking=no"

git config --global user.name "$GIT_USER_NAME"
git config --global user.email "$GIT_USER_EMAIL"

if [ ! -d /root/pz-saves ]; then
    git clone "$REPO_URL" /root/pz-saves
fi

mkdir -p /data/backups
cd /data/backups
echo "=== Starting HTTP File Server on port 80 ==="
python3 -m http.server 80 &

cd /root/pz-saves
# Ensure files exist to avoid errors
touch backup_log restore_target backup_request server_info.json
git add .
git commit -m "Initialize state files" || true
git push || true

echo "=== Entering Autosaver Loop ==="
while true; do
    cd /root/pz-saves
    git pull >/dev/null 2>&1
    
    if [ -f server_info.json ]; then
        STATUS=$(jq -r '.status' server_info.json)
        SERVER_IP=$(jq -r '.ip' server_info.json)
        SERVER_PORT=$(jq -r '.port' server_info.json)
        
        if [ "$STATUS" = "booting" ] || [ "$STATUS" = "online" ]; then
            # Check if restore is requested
            if [ -s restore_target ]; then
                TARGET=$(cat restore_target)
                echo "Restore requested: $TARGET to $SERVER_IP:$SERVER_PORT"
                if [ -f "/data/backups/$TARGET" ]; then
                    echo "Uploading and extracting backup..."
                    scp -P $SERVER_PORT -o StrictHostKeyChecking=no /data/backups/$TARGET steam@$SERVER_IP:/home/steam/Zomboid/Saves/
                    ssh -p $SERVER_PORT -o StrictHostKeyChecking=no steam@$SERVER_IP "cd /home/steam/Zomboid/Saves && unzip -o $TARGET && rm $TARGET"
                    
                    echo "Clearing restore_target"
                    > restore_target
                    git add restore_target
                    git commit -m "Restore of $TARGET completed"
                    git push
                    echo "Restore completed."
                else
                    echo "ERROR: Backup $TARGET not found in /data/backups/"
                fi
            fi
            
            # Check if manual backup requested OR time for periodic backup (every 6 hours)
            CURRENT_TIME=$(date +%s)
            LAST_BACKUP=$(cat /data/last_backup_time 2>/dev/null || echo "0")
            DIFF=$((CURRENT_TIME - LAST_BACKUP))
            
            if [ $DIFF -gt 21600 ] || [ -s backup_request ]; then
                echo "Starting backup from $SERVER_IP:$SERVER_PORT"
                TIMESTAMP=$(date +%Y%m%d_%H%M%S)
                BACKUP_NAME="backup_$TIMESTAMP.zip"
                
                # Zip saves
                ssh -p $SERVER_PORT -o StrictHostKeyChecking=no steam@$SERVER_IP "cd /home/steam/Zomboid/Saves && zip -q -r /tmp/$BACKUP_NAME ."
                
                # Download zip
                scp -P $SERVER_PORT -o StrictHostKeyChecking=no steam@$SERVER_IP:/tmp/$BACKUP_NAME /data/backups/$BACKUP_NAME
                
                # Cleanup server zip
                ssh -p $SERVER_PORT -o StrictHostKeyChecking=no steam@$SERVER_IP "rm /tmp/$BACKUP_NAME"
                
                echo $CURRENT_TIME > /data/last_backup_time
                echo $BACKUP_NAME >> backup_log
                > backup_request
                git add backup_log backup_request
                git commit -m "Created backup $BACKUP_NAME"
                git push
                
                echo "Backup $BACKUP_NAME finished and logged."
            fi
        fi
    fi
    
    sleep 60
done
