# Project Zomboid - Akash Deployment

A fully automated, self-healing Project Zomboid dedicated server for the Akash Network.

## 🚀 Getting Started

The deployment uses a private Git repository (e.g., `pz-saves`) to store state and communicate between the **Game Server** and the **Autosaver**. 

### 1. Initial IP Configuration
When you first deploy, the Game Server will boot up. Because it might not correctly fetch its public IP on Akash, it enters a pending state:
1. Clone your `pz-saves` git repository locally.
2. Wait for the server to finish booting (it takes a few minutes). Once ready, the server will push `{"ip": "pending", "status": "booting", "port": 2222}` to `server_info.json`.
3. Edit `server_info.json` and change `"pending"` to your server's actual public IP address.
4. Commit and push the change.
5. The server will detect your IP, mark itself as `"online"`, and the Autosaver will begin normal operations!

---

## 💾 Backups & Restores

The Autosaver securely streams `.zip` backups directly from the game server via SSH, so the game doesn't pause.

### Manual Backups
To force an immediate backup at any time:
1. In your `pz-saves` repository, create or edit the file named `backup_request` and type anything inside (e.g., `true`).
2. Commit and push the file. 
3. The Autosaver will immediately run a backup and then automatically empty the `backup_request` file when finished.

### Pausing Automatic Backups
By default, the Autosaver backs up the server every hour.
1. To pause periodic backups, create or edit the `pause_autosave` file and write `true`.
2. Commit and push. The Autosaver will skip periodic backups until you remove the word `true`. (Manual backups will still work).

### Downloading & Viewing Backups
The Autosaver runs a web server on port `80`.
1. Open `http://<autosaver-ip>:80/` in your browser.
2. You will see a list of all your `.zip` backup files. Click any file to download it.

### Uploading Backups
You can upload old or custom `.zip` backups directly into the Autosaver system.
1. Open `http://<autosaver-ip>:80/upload` in your browser.
2. Select your `.zip` file and click upload.
3. The file will instantly be available for restoring!

### Restoring a Backup
To restore a specific backup to the Game Server:
1. Edit the `restore_target` file in your `pz-saves` repository and paste the exact filename of your backup (e.g., `backup_20260810_120000.zip`).
2. Edit the `request_restore` file and write `requested`.
3. Commit and push both files. 
4. Restart your game server container (or wait for it to reboot). Upon starting, it will detect the restore request, download your backup from the Autosaver, and wipe the current world to apply the backup!
