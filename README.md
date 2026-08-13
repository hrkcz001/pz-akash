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
To force an immediate backup at any time, push a file named `backup` (any content) to `pz-saves`. The Autosaver consumes it (removes + pushes), runs a safe backup (RCON save → zip → updates `restore_target`), and is done.

### Halting the Server (Graceful Shutdown)
Push a file named `halt` to `pz-saves`. The Autosaver consumes it, performs a safe backup, and issues a `quit` command to the server. The server gracefully shuts down and updates `server_info.json` to `"stopped"`.

### Scheduled Stop + Auto Top-up
Push a file named `stop_at` with a stop time — epoch seconds or `YYYY-MM-DD HH:MM[:SS]` (UTC):
- When the time is reached, the Autosaver does a final safe backup, gracefully stops the server, and **closes the Akash deployment** (billing stops). The `stop_at` file is consumed.
- Until then, the Autosaver checks the deployment escrow every 10 minutes and **tops it up** so funds always cover the remaining time (with margin). Edit `stop_at` to a later time and it will simply top up more; if the time already passed, the next check stops the server immediately.
- If the server is already shut down (deployment closed), pushing `start` again deploys it fresh.

### Webhooks (instead of polling)
The Autosaver listens for GitHub webhooks on port `8080` (`WEBHOOK_PORT`). Setup:
1. **Deploy the Autosaver with port 8080 exposed** — use `pz-autosave/deployment.example.yaml` as a starting point. It uses **shared endpoints** (no IP lease — IP leases are rare), so the provider assigns the external ports and the URL is only known after deploy.
2. **Find the webhook URL** — the autosaver resolves and logs it into `/data/webhook.log` a few minutes after boot (the log also goes to the container logs in the Akash Console). You can also run `/usr/local/bin/webhook_url.sh` inside the container via the Console shell, or read the lease status in the Akash Console. It handles both shared endpoints (forwarded port) and IP leases.
3. **Create the webhook in GitHub**: `pz-saves` repo → Settings → Webhooks → Add webhook → Payload URL: `http://<public-ip>:8080/webhook`, Content type: `application/json`, Secret: the same value as `WEBHOOK_SECRET` → Add webhook (default events are fine — pushes).
4. **Set in the Autosaver deployment**: `WEBHOOK_SECRET` (same value) and `WEBHOOK_MODE=true`. The loop then only polls as a slow safety net (`WEBHOOK_POLL_SEC`, default 300s).
5. Every push (trigger files, `stop_at`, `server_info.json` from the server) is processed within seconds instead of a polling interval.

### Pricing
- **`MAX_PRICE_USD` (default `3.0`)** — the bid ceiling in USD per day. An 8 vCPU / 16Gi / 30Gi server with an IP lease typically goes for ~$1–3/day on Akash; this cap only sets the ceiling, the winning bid lands below it. `pz-server/deployment.yaml` (manual deploys) uses `amount: 400` uakt/block ≈ $2.9/day at AKT ~$0.5.
- **`DEPLOY_DAYS` (default `7`)** — the escrow deposit covers this many days at the max price; unspent funds return when the deployment is closed. `stop_at` schedules a stop and top-ups funds automatically (see above).

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

---

## 🚀 Automated Deployment (Console managed-wallet API)

Push a file named `start` to the `pz-saves` repo and the Autosaver will:

1. **Remove** the `start` file (consumed trigger).
2. **Deploy** the game server on Akash via the Console API (if it isn't already online): picks the best **EU** provider — cheapest wins, but any bid within `PRICE_TOLERANCE` (20%) of the cheapest makes the **closest** one win; creates the deployment (escrow funded for `DEPLOY_DAYS` at max price), accepts the lease, waits for the public IP.
3. **Writes the IP** into `server_info.json` (`status: booting`) so the server boots without manual IP configuration.
4. **Provider failures**: closes the deployment, remembers the provider (skip list), and retries with a fresh deployment. Failed providers are forgotten as soon as a deploy succeeds.

**Server SDL**: the deploy script uses the server's `deployment.yaml` from the **pz-saves** repo (falls back to the template bundled in the image). That file is **self-sufficient** — image tag, ports, env (incl. the SSH deploy key) all live there; only the `__MAX_PRICE_UAKT__` token is filled at deploy time from the autosaver's `MAX_PRICE_USD`. Bump the image tag in it after rebuilding `pz-server`. Any other `__TOKEN__` left unresolved aborts the deploy.

**Required env in the Autosaver deployment** (no server info — the autosaver reads the server SDL from pz-saves; RCON credentials are extracted from `ADMIN_PASSWORD` there):
- `AKASH_API_KEY` — Console managed-wallet API key (Console → Settings → API Keys)
- `SSH_PRIVATE_KEY_BASE64` — deploy key for pz-saves
- `REPO_URL`, `GIT_USER_NAME`, `GIT_USER_EMAIL`
- `WEBHOOK_SECRET` — for the GitHub webhook (see the webhook section)

**Tuning env (defaults in brackets):**
- `DEPLOY_DAYS [7]` — escrow covers this many days at max price
- `MIN_PRICE_USD [0.001]` / `MAX_PRICE_USD [3.0]` — bid filter, USD per day
- `PRICE_TOLERANCE [0.20]` — within this % of the cheapest bid the closest provider wins
- `REF_LAT [52.2297]` / `REF_LON [21.0122]` — reference point for proximity (default Warsaw, central/east EU)
- `EU_COUNTRY_CODES` — default EU27 + GB CH NO UA
- `SKIP_TTL_SEC [86400]` — how long failed providers are remembered
- `SERVER_ONLINE_VERIFY [true]` — wait for `server_info` "online" before declaring success

Deploy logs: `/data/deploy.log` inside the Autosaver container.
