# Project Zomboid - Akash Deployment

This repository contains the setup for a fully automated, self-healing Project Zomboid dedicated server on the Akash Network. It is split into two containers: a **Game Server** and an **Autosaver**.

## 🚀 How It Works
- **Game Server (`pz-server`)**: Runs Project Zomboid. It automatically syncs its state with a private GitHub repository you configure. If it crashes, it requests a restore.
- **Autosaver (`pz-autosave`)**: Runs continuously, safely triggering game saves via RCON and streaming `.zip` backups directly from the server. It handles restores automatically when the game server requests them.

## ⚙️ Configuration
All configurations are exposed as environment variables in your `deployment.yaml` and `docker-compose.yml`. You can change settings like:
- `SERVER_NAME`, `ADMIN_PASSWORD`
- `BACKUP_INTERVAL_SEC` (default: 3600 / 1 hour)
- `BACKUP_RETENTION_DAYS` (default: 7 days)
- `SERVER_MEMORY_MAX` (default: 8192m)

## 💾 Backups & Restores

### Downloading Backups
The Autosaver exposes a web server on port `80` (or whatever `HTTP_PORT` you configured). 
1. Open your browser and navigate to `http://<autosaver-ip>:80/`
2. You will see a directory of all your `.zip` backup files. Click any to download.

### Uploading Your Own Backups
You can upload an existing `.zip` backup directly to the running Autosaver container using the built-in web interface!
1. Open your browser and navigate to `http://<autosaver-ip>:80/upload`
2. Select your `.zip` file and hit upload.
3. Once uploaded, you can trigger a restore manually by modifying the `restore_target` file in your Git repository.

### Pre-loading Backups During Build
If you don't want to use the web uploader, you can bake backups directly into your Docker image:
1. Place your `.zip` backup files into the `pz-autosave/local_backups/` directory on your computer. (They are ignored by Git, so they won't bloat your repo!)
2. Run your `docker build` command. The files will automatically be placed into `/data/backups/` inside the container.

### Triggering a Restore Manually
To force the server to load a specific backup:
1. Edit the `restore_target` text file in your Git repository and paste the exact filename of your backup (e.g., `backup_20260810_120000.zip`).
2. Edit the `request_restore` text file and type `requested`.
3. Commit and push the changes. The autosaver will detect it, wipe the current world, and unzip your backup!
