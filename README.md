# Project Zomboid - Akash Deployment & Controller

A fully automated, self-healing Project Zomboid dedicated server and mod management system for the Akash Network.

---

## 🏗 Architecture Overview

The system consists of two primary services communicating via the private `pz-saves` Git repository and an authenticated/public HTTP storage interface:

1. **`pz-controller`**:
   - **Build-Time Mod Packaging**: Downloads Steam Workshop mods during image build using SteamCMD for `common/`, `client/`, and `server/` configs.
   - **Auto-Configures Configs**: Automatically discovers mod IDs and custom maps, injecting `Mods=` and `Map=` into server `.ini` files.
   - **Generates 3 Archives**:
     - `common.zip`: Shared mods & files (Public).
     - `client.zip`: Client-specific mods & configs (Public).
     - `server.zip`: Server-specific mods, `.ini`s, and `.lua` files (🔒 Protected).
   - **HTTP Hub & Dashboard (`:8000`)**:
     - Web UI showing live server IP & Port (with 1-click copy buttons), live status badges, clean game client `.torrent` download, and package statistics (mod count, config file count, archive size).
     - Dynamically renders player guide from `README.md` in `pz-saves`.
     - Public endpoints: `/`, `/client.zip`, `/common.zip`, `/game.torrent`, `/server_info.json`, `/manifest`, `/healthz`.
     - Protected endpoints: `/server.zip` (requires `SERVER_FILES_PASSWORD` or `STORAGE_PASSWORD`), `/upload` & `/backups/*` (requires `BACKUPS_PASSWORD` or `STORAGE_PASSWORD`).
   - **Akash Orchestrator & Auto-Backups**: Triggers server deployments on `start`, safe RCON backups on schedule or `backup`, and graceful halts on `halt` or `stop_at`.

2. **`pz-server` (Dedicated Game Server)**:
   - Lightweight image containing only core Project Zomboid binaries.
   - On boot, dynamically downloads `common.zip` and `server.zip` from `pz-controller` using authenticated credentials, unzips/merges them into `/home/steam/Zomboid/`, and launches the dedicated server.

---

## 📁 `pz-saves` Repository Structure

The `pz-saves` repository maintains your world state and configurations:

```text
pz-saves/
├── README.md                 <-- Player connection & mod installation guide (rendered on web hub)
├── game.torrent              <-- Torrent file for clean game installation (downloadable from hub)
├── deployment.yaml           <-- Akash deployment configuration for dedicated server
├── common/                   <-- Shared between client & server
│   └── mods.json             <-- List of Workshop IDs common to both
├── client/                   <-- Client-only files & configs
│   ├── mods.json             <-- Client-only Workshop IDs
│   └── Lua/                  <-- Client-side scripts/configs
├── server/                   <-- Server-only files (PROTECTED)
│   ├── mods.json             <-- Server-only Workshop IDs
│   └── Server/
│       ├── vsrania.ini
│       ├── vsrania_SandboxVars.lua
│       └── vsrania_spawnregions.lua
├── server_info.json          <-- Live server IP, port, and status (auto-managed)
├── controller_info.json      <-- Auto-discovered controller ingress URL (auto-managed)
└── restore_target            <-- Target backup file for auto-restore
```

---

## 🎮 Player Experience & Connecting

1. Open `http://<controller-ip>:8000/` (or your custom domain with Cloudflare SSL `https://...`) in any browser.
2. Check the live server status badge (**ONLINE**, **STARTING UP**, or **OFFLINE**).
3. Download the clean game client using the **`game.torrent`** button (recommended to prevent version mismatches).
4. Download **`common.zip`** and **`client.zip`** from the web hub.
5. Extract both `.zip` archives directly into your local Zomboid folder with file replacement / overwrite:
   - **Windows**: `%USERPROFILE%\Zomboid\` (e.g. `C:\Users\<Name>\Zomboid\`)
   - **Linux / macOS**: `~/Zomboid/`
6. When the status is **ONLINE**, copy the **Server IP** and **Port** using the 1-click copy buttons and connect in-game!

---

## 💾 Backups, Restores & Lifecycle

### Starting the Dedicated Server (Deploy to Akash)
Push a file named `start` to `pz-saves`. The Controller consumes it (`git rm start`), reads `deployment.yaml` from `pz-saves`, creates an Akash deployment via the Akash Console API, waits for provider lease bids and IP assignment, writes the server IP to `server_info.json`, and boots the dedicated game server.

### Manual Backups
Push a file named `backup` to `pz-saves`. The Controller consumes it, runs a safe backup (`RCON save` → `zip` stream → updates `restore_target`), and pushes to git.

### Halting the Server (Graceful Shutdown)
Push a file named `halt` to `pz-saves`. The Controller consumes it, runs a safe backup, and issues `quit` via RCON. Once stopped, the Controller **closes the Akash deployment** to stop billing and refund unspent escrow.

### Scheduled Stop + Auto Top-up
Push a file named `stop_at` with an epoch timestamp or `YYYY-MM-DD HH:MM[:SS]` (UTC):
- At the scheduled time, the Controller backs up and halts the server, closing the Akash deployment.
- Until then, the Controller automatically checks and tops up the lease escrow.

### Self-Healing & Auto-Restart on Crash
If the Project Zomboid dedicated server crashes or the Akash provider deployment unexpectedly closes before `halt` is sent or `stop_at` is reached:
- The server process automatically restarts in-container (up to 3 retry attempts).
- If local restarts are exhausted or the Akash deployment is terminated unexpectedly, the Controller automatically provisions a fresh redeployment on Akash to maintain uptime.

### Restoring a Backup
1. Write the backup filename into `restore_target` in `pz-saves` (e.g. `backup_20260810_120000.zip`).
2. Write `requested` into `request_restore`.
3. Commit and push.
4. When the server boots, it downloads the backup and applies it cleanly before starting the world.

---

## 🔒 Storage Server Security & Password Management

The Controller HTTP service (`:8000`) supports granular password protection:

| Environment Variable | Description & Scope | Default Fallback |
| :--- | :--- | :--- |
| **`SERVER_FILES_PASSWORD`** | Protects `/server.zip` and the **Server Files** card unlock in the web dashboard. | `STORAGE_PASSWORD` |
| **`BACKUPS_PASSWORD`** | Protects `/backups` list, `/backups/<file>` downloads, `/upload`, and the **Backups** view. | `STORAGE_PASSWORD` |
| **`STORAGE_PASSWORD`** | Master administrator password that serves as a global fallback for all protected endpoints. | `ADMIN_PASSWORD` |

### Supported Authentication Formats:
- **Authorization Header**: `Authorization: Bearer <PASSWORD>` or `Authorization: Basic <base64(user:password)>`
- **Custom Headers**: `X-Auth-Token: <PASSWORD>`, `X-Server-Files-Password: <PASSWORD>`, `X-Backups-Password: <PASSWORD>`
- **Query Parameters**: `?token=<PASSWORD>`, `?server_token=<PASSWORD>`, `?backup_token=<PASSWORD>`
- **Interactive UI**: Password unlock modals right from the web dashboard and `/backups` page with session persistence.
