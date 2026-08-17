#!/usr/bin/env python3
"""
storage_server.py — Secure HTTP file & storage server for PZ Controller.

Features:
  - Modern, responsive dark UI dashboard
  - 3 main action cards: Client Files, Common Files, and Server Files (faded with Lock)
  - Smaller faded Backups button with Lock at the bottom
  - In-browser password unlock modal (unlocks server files and backups seamlessly)
  - Dedicated /backups view with .gitkeep filtered out
  - Live Server IP & Status widget with 1-click copy
  - Public endpoints: /, /client.zip, /common.zip, /server_info.json, /healthz
  - Protected endpoints: /server.zip, /backups, /backups/<filename>, /upload
"""

import base64
import cgi
import datetime
import hmac
import json
import os
import secrets
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlparse

HTTP_PORT = int(os.environ.get("HTTP_PORT", "8000"))
STORAGE_PASSWORD = os.environ.get("STORAGE_PASSWORD") or os.environ.get("CONTROLLER_SECRET") or os.environ.get("ADMIN_PASSWORD", "")
PACKAGES_DIR = Path(os.environ.get("PACKAGES_DIR", "/data/packages"))
BACKUPS_DIR = Path(os.environ.get("BACKUPS_DIR", "/data/backups"))
SERVES_REPO = Path(os.environ.get("SERVES_REPO", "/root/pz-saves"))

CHUNK_SIZE = 256 * 1024  # 256 KB streaming chunk


def log(msg: str):
    print(f"[storage_server] {msg}", flush=True)


def check_auth(headers, query_params) -> bool:
    """Validate bearer token, basic auth, custom header or query token against STORAGE_PASSWORD."""
    if not STORAGE_PASSWORD:
        log("WARNING: STORAGE_PASSWORD is not set! Denying protected access.")
        return False

    # 1. Bearer token in Authorization header
    auth_header = headers.get("Authorization", "")
    if auth_header.startswith("Bearer "):
        token = auth_header[len("Bearer "):].strip()
        if hmac.compare_digest(token, STORAGE_PASSWORD):
            return True

    # 2. Basic Auth in Authorization header
    if auth_header.startswith("Basic "):
        try:
            raw = base64.b64decode(auth_header[len("Basic "):].strip()).decode("utf-8")
            if ":" in raw:
                _, password = raw.split(":", 1)
                if hmac.compare_digest(password, STORAGE_PASSWORD):
                    return True
        except Exception:
            pass

    # 3. X-Auth-Token or X-Storage-Secret header
    token_header = headers.get("X-Auth-Token") or headers.get("X-Storage-Secret") or headers.get("X-Controller-Secret")
    if token_header and hmac.compare_digest(token_header.strip(), STORAGE_PASSWORD):
        return True

    # 4. Query param (?token=... or ?key=... or ?password=...)
    query_token = query_params.get("token", [None])[0] or query_params.get("key", [None])[0] or query_params.get("password", [None])[0]
    if query_token and hmac.compare_digest(query_token.strip(), STORAGE_PASSWORD):
        return True

    return False


def get_server_info():
    """Read server_info.json from pz-saves repo."""
    info_path = SERVES_REPO / "server_info.json"
    if info_path.is_file():
        try:
            return json.loads(info_path.read_text(encoding="utf-8"))
        except Exception:
            pass
    return {"ip": "pending", "port": 16261, "status": "unknown"}


def get_manifest():
    """Read packages_manifest.json."""
    manifest_path = PACKAGES_DIR / "packages_manifest.json"
    if manifest_path.is_file():
        try:
            return json.loads(manifest_path.read_text(encoding="utf-8"))
        except Exception:
            pass
    return {}


def get_valid_backups():
    """Return sorted list of valid .zip backup files, hiding .gitkeep and non-zips."""
    backups = []
    if BACKUPS_DIR.is_dir():
        for f in BACKUPS_DIR.iterdir():
            # Exclude .gitkeep, hidden files, and non-zip files
            if f.is_file() and f.suffix.lower() == ".zip" and not f.name.startswith("."):
                backups.append({
                    "name": f.name,
                    "size": f.stat().st_size,
                    "size_str": f"{f.stat().st_size / (1024*1024):.2f} MB" if f.stat().st_size >= 1024*1024 else f"{f.stat().st_size / 1024:.1f} KB",
                    "mtime": int(f.stat().st_mtime),
                    "date_str": datetime.datetime.fromtimestamp(f.stat().st_mtime).strftime("%Y-%m-%d %H:%M:%S")
                })
    return sorted(backups, key=lambda b: b["mtime"], reverse=True)


def render_html_dashboard(server_info: dict, manifest: dict, token: str = "") -> str:
    status = server_info.get("status", "unknown").lower()
    ip = server_info.get("ip", "pending")
    port = server_info.get("port", 16261)
    
    badge_color = "#eab308"
    status_text = "BOOTING"
    if status == "online":
        badge_color = "#10b981"
        status_text = "SERVER ONLINE"
    elif status in ("stopped", "error", "failed"):
        badge_color = "#ef4444"
        status_text = status.upper()

    client_info = manifest.get("client", {})
    common_info = manifest.get("common", {})
    server_pkg_info = manifest.get("server", {})

    client_size_mb = f"{client_info.get('size', 0) / (1024*1024):.1f} MB" if client_info.get('size') else "Ready"
    common_size_mb = f"{common_info.get('size', 0) / (1024*1024):.1f} MB" if common_info.get('size') else "Ready"
    server_size_mb = f"{server_pkg_info.get('size', 0) / (1024*1024):.1f} MB" if server_pkg_info.get('size') else "Ready"

    client_mods_count = client_info.get('mods_count', 0)
    common_mods_count = common_info.get('mods_count', 0)
    server_mods_count = server_pkg_info.get('mods_count', 0)

    return f"""<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>Project Zomboid • Controller & Hub</title>
  <style>
    :root {{
      --bg: #0b0f19;
      --card-bg: rgba(17, 24, 39, 0.85);
      --card-border: rgba(255, 255, 255, 0.08);
      --text: #f3f4f6;
      --text-muted: #9ca3af;
      --primary: #3b82f6;
      --primary-hover: #2563eb;
      --primary-glow: rgba(59, 130, 246, 0.35);
      --accent: #10b981;
      --accent-glow: rgba(16, 185, 129, 0.3);
      --amber: #f59e0b;
      --amber-glow: rgba(245, 158, 11, 0.25);
    }}
    * {{
      box-sizing: border-box;
    }}
    body {{
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Oxygen, Ubuntu, Cantarell, sans-serif;
      background: radial-gradient(circle at top center, #1e1b4b 0%, #0b0f19 55%, #030712 100%);
      color: var(--text);
      margin: 0;
      padding: 2.5rem 1rem;
      min-height: 100vh;
      display: flex;
      justify-content: center;
      align-items: flex-start;
    }}
    .container {{
      max-width: 880px;
      width: 100%;
    }}
    .header-card {{
      background: var(--card-bg);
      backdrop-filter: blur(16px);
      border: 1px solid var(--card-border);
      border-radius: 18px;
      padding: 2rem;
      margin-bottom: 2rem;
      box-shadow: 0 20px 25px -5px rgba(0, 0, 0, 0.5), 0 8px 10px -6px rgba(0, 0, 0, 0.5);
    }}
    .title-row {{
      display: flex;
      justify-content: space-between;
      align-items: center;
      flex-wrap: wrap;
      gap: 1rem;
    }}
    h1 {{
      font-size: 2rem;
      font-weight: 800;
      letter-spacing: -0.025em;
      margin: 0;
      display: flex;
      align-items: center;
      gap: 0.75rem;
      background: linear-gradient(135deg, #ffffff 0%, #cbd5e1 100%);
      -webkit-background-clip: text;
      -webkit-text-fill-color: transparent;
    }}
    .status-badge {{
      display: inline-flex;
      align-items: center;
      gap: 0.5rem;
      padding: 0.4rem 0.9rem;
      border-radius: 9999px;
      font-weight: 700;
      font-size: 0.8rem;
      letter-spacing: 0.05em;
      background: rgba(0, 0, 0, 0.3);
      border: 1px solid var(--card-border);
    }}
    .status-dot {{
      width: 10px;
      height: 10px;
      border-radius: 50%;
      background-color: {badge_color};
      box-shadow: 0 0 12px {badge_color};
    }}
    .ip-box {{
      background: rgba(0, 0, 0, 0.4);
      border: 1px solid rgba(255, 255, 255, 0.06);
      border-radius: 12px;
      padding: 1rem 1.25rem;
      margin-top: 1.5rem;
      display: flex;
      justify-content: space-between;
      align-items: center;
      flex-wrap: wrap;
      gap: 1rem;
    }}
    .ip-details {{
      display: flex;
      flex-direction: column;
      gap: 0.25rem;
    }}
    .ip-label {{
      font-size: 0.75rem;
      text-transform: uppercase;
      letter-spacing: 0.08em;
      color: var(--text-muted);
      font-weight: 600;
    }}
    .ip-address {{
      font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
      font-size: 1.35rem;
      font-weight: 700;
      color: #38bdf8;
      letter-spacing: 0.02em;
    }}
    .btn {{
      display: inline-flex;
      align-items: center;
      justify-content: center;
      gap: 0.5rem;
      font-weight: 600;
      border-radius: 10px;
      padding: 0.75rem 1.25rem;
      font-size: 0.95rem;
      text-decoration: none;
      cursor: pointer;
      border: 1px solid transparent;
      transition: all 0.2s cubic-bezier(0.4, 0, 0.2, 1);
    }}
    .btn-primary {{
      background: linear-gradient(135deg, #3b82f6 0%, #2563eb 100%);
      color: white;
      box-shadow: 0 4px 14px var(--primary-glow);
    }}
    .btn-primary:hover {{
      transform: translateY(-2px);
      box-shadow: 0 6px 20px var(--primary-glow);
    }}
    .btn-emerald {{
      background: linear-gradient(135deg, #10b981 0%, #059669 100%);
      color: white;
      box-shadow: 0 4px 14px var(--accent-glow);
    }}
    .btn-emerald:hover {{
      transform: translateY(-2px);
      box-shadow: 0 6px 20px var(--accent-glow);
    }}
    .btn-secondary {{
      background: rgba(255, 255, 255, 0.06);
      color: var(--text);
      border-color: rgba(255, 255, 255, 0.1);
    }}
    .btn-secondary:hover {{
      background: rgba(255, 255, 255, 0.12);
    }}
    .cards-grid {{
      display: grid;
      grid-template-columns: repeat(3, 1fr);
      gap: 1.25rem;
      margin-bottom: 2rem;
    }}
    @media (max-width: 768px) {{
      .cards-grid {{
        grid-template-columns: 1fr;
      }}
    }}
    .action-card {{
      background: var(--card-bg);
      backdrop-filter: blur(12px);
      border: 1px solid var(--card-border);
      border-radius: 16px;
      padding: 1.5rem;
      display: flex;
      flex-direction: column;
      justify-content: space-between;
      position: relative;
      overflow: hidden;
      transition: all 0.25s ease;
      box-shadow: 0 10px 15px -3px rgba(0, 0, 0, 0.3);
    }}
    .action-card:hover {{
      transform: translateY(-4px);
      border-color: rgba(255, 255, 255, 0.2);
    }}
    .action-card.locked {{
      opacity: 0.55;
      filter: saturate(0.6);
      background: rgba(17, 24, 39, 0.5);
      border-style: dashed;
    }}
    .action-card.locked:hover {{
      opacity: 0.85;
      filter: saturate(0.9);
      transform: translateY(-2px);
    }}
    .card-badge {{
      display: inline-flex;
      align-items: center;
      gap: 0.35rem;
      font-size: 0.75rem;
      font-weight: 700;
      text-transform: uppercase;
      letter-spacing: 0.05em;
      padding: 0.25rem 0.6rem;
      border-radius: 6px;
      margin-bottom: 0.75rem;
      width: fit-content;
    }}
    .badge-blue {{
      background: rgba(59, 130, 246, 0.15);
      color: #60a5fa;
      border: 1px solid rgba(59, 130, 246, 0.3);
    }}
    .badge-green {{
      background: rgba(16, 185, 129, 0.15);
      color: #34d399;
      border: 1px solid rgba(16, 185, 129, 0.3);
    }}
    .badge-amber {{
      background: rgba(245, 158, 11, 0.15);
      color: #fbbf24;
      border: 1px solid rgba(245, 158, 11, 0.3);
    }}
    .card-title {{
      font-size: 1.25rem;
      font-weight: 700;
      margin: 0 0 0.5rem 0;
      display: flex;
      align-items: center;
      gap: 0.5rem;
    }}
    .card-desc {{
      color: var(--text-muted);
      font-size: 0.875rem;
      line-height: 1.45;
      margin: 0 0 1.25rem 0;
      flex-grow: 1;
    }}
    .card-stats {{
      display: flex;
      gap: 0.75rem;
      font-size: 0.8rem;
      color: var(--text-muted);
      margin-bottom: 1.25rem;
      padding-top: 0.75rem;
      border-top: 1px solid rgba(255, 255, 255, 0.05);
    }}
    .bottom-bar {{
      display: flex;
      justify-content: center;
      margin-top: 1rem;
    }}
    .backups-btn {{
      display: inline-flex;
      align-items: center;
      gap: 0.6rem;
      padding: 0.65rem 1.25rem;
      border-radius: 12px;
      font-size: 0.875rem;
      font-weight: 600;
      color: var(--text-muted);
      background: rgba(17, 24, 39, 0.6);
      border: 1px dashed rgba(255, 255, 255, 0.15);
      text-decoration: none;
      transition: all 0.2s ease;
      opacity: 0.75;
    }}
    .backups-btn:hover {{
      opacity: 1;
      color: white;
      border-color: rgba(255, 255, 255, 0.35);
      background: rgba(30, 41, 59, 0.8);
      transform: translateY(-2px);
    }}
    .guide-card {{
      background: var(--card-bg);
      backdrop-filter: blur(12px);
      border: 1px solid var(--card-border);
      border-radius: 16px;
      padding: 1.5rem;
      margin-top: 2rem;
    }}
    .guide-title {{
      font-size: 1rem;
      font-weight: 700;
      color: #cbd5e1;
      margin-top: 0;
      display: flex;
      align-items: center;
      gap: 0.5rem;
    }}
    ol {{
      margin: 0;
      padding-left: 1.25rem;
      color: var(--text-muted);
      font-size: 0.9rem;
      line-height: 1.6;
    }}
    code {{
      background: rgba(0, 0, 0, 0.4);
      padding: 0.2rem 0.4rem;
      border-radius: 6px;
      font-family: ui-monospace, monospace;
      color: #38bdf8;
      border: 1px solid rgba(255, 255, 255, 0.05);
    }}
    /* Modal Styles */
    .modal-overlay {{
      display: none;
      position: fixed;
      inset: 0;
      background: rgba(0, 0, 0, 0.75);
      backdrop-filter: blur(8px);
      z-index: 100;
      align-items: center;
      justify-content: center;
      padding: 1rem;
    }}
    .modal-card {{
      background: #111827;
      border: 1px solid rgba(255, 255, 255, 0.15);
      border-radius: 16px;
      max-width: 440px;
      width: 100%;
      padding: 2rem;
      box-shadow: 0 25px 50px -12px rgba(0, 0, 0, 0.7);
      animation: modalIn 0.2s ease-out;
    }}
    @keyframes modalIn {{
      from {{ opacity: 0; transform: scale(0.95); }}
      to {{ opacity: 1; transform: scale(1); }}
    }}
    .input-field {{
      width: 100%;
      padding: 0.75rem 1rem;
      border-radius: 10px;
      background: #090d16;
      border: 1px solid rgba(255, 255, 255, 0.15);
      color: white;
      font-size: 1rem;
      margin: 1rem 0 1.5rem 0;
      outline: none;
      transition: border-color 0.15s;
    }}
    .input-field:focus {{
      border-color: #3b82f6;
    }}
  </style>
</head>
<body>
  <div class="container">
    
    <!-- Header with Live Server Info -->
    <div class="header-card">
      <div class="title-row">
        <h1>🧟 Project Zomboid</h1>
        <div class="status-badge">
          <div class="status-dot"></div>
          <span>{status_text}</span>
        </div>
      </div>
      
      <p style="color:var(--text-muted); margin:0.5rem 0 0 0; font-size:0.95rem;">
        Game Server mod distribution and automated management hub.
      </p>

      <div class="ip-box">
        <div class="ip-details">
          <span class="ip-label">Dedicated Server Address</span>
          <span class="ip-address">{ip}:{port}</span>
        </div>
        <button class="btn btn-secondary" onclick="copyIp('{ip}:{port}', this)">
          📋 Copy Address
        </button>
      </div>
    </div>

    <!-- 3 Main Action Cards -->
    <div class="cards-grid">
      
      <!-- 1. Client Files -->
      <div class="action-card">
        <div>
          <div class="card-badge badge-blue">🎮 Player Package</div>
          <h3 class="card-title">Client Files</h3>
          <p class="card-desc">Client-side mods, UI tweaks, lua scripts and player settings.</p>
        </div>
        <div>
          <div class="card-stats">
            <span>📦 {client_mods_count} mod(s)</span>
            <span>💾 {client_size_mb}</span>
          </div>
          <a href="/client.zip" class="btn btn-primary" style="width:100%;" download>
            ⬇️ Download Client
          </a>
        </div>
      </div>

      <!-- 2. Common Files -->
      <div class="action-card">
        <div>
          <div class="card-badge badge-green">🌐 Shared Package</div>
          <h3 class="card-title">Common Files</h3>
          <p class="card-desc">Shared workshop mods, map files, and assets common to both client and server.</p>
        </div>
        <div>
          <div class="card-stats">
            <span>📦 {common_mods_count} mod(s)</span>
            <span>💾 {common_size_mb}</span>
          </div>
          <a href="/common.zip" class="btn btn-emerald" style="width:100%;" download>
            ⬇️ Download Common
          </a>
        </div>
      </div>

      <!-- 3. Server Files (Faded with Lock) -->
      <div class="action-card locked" id="serverCard">
        <div>
          <div class="card-badge badge-amber" id="serverBadge">🔒 Protected</div>
          <h3 class="card-title">Server Files</h3>
          <p class="card-desc">Server-only configurations (<code>.ini</code>, <code>SandboxVars.lua</code>, spawn regions, server mods).</p>
        </div>
        <div>
          <div class="card-stats">
            <span>📦 {server_mods_count} mod(s)</span>
            <span>💾 {server_size_mb}</span>
          </div>
          <button class="btn btn-secondary" id="serverBtn" style="width:100%;" onclick="handleProtectedDownload('server.zip')">
            🔒 Unlock Server Files
          </button>
        </div>
      </div>

    </div>

    <!-- Bottom Faded Backups Button with Lock -->
    <div class="bottom-bar">
      <a href="javascript:void(0)" class="backups-btn" id="backupsLink" onclick="handleBackupsNav()">
        🔒 🗄️ Server Backups & Save Management
      </a>
    </div>

    <!-- Quick Installation Accordion -->
    <div class="guide-card">
      <h4 class="guide-title">📖 Player Installation Guide</h4>
      <ol>
        <li>Download both <strong><code>common.zip</code></strong> and <strong><code>client.zip</code></strong> above.</li>
        <li>Extract both archives directly into your local Zomboid folder:
          <br>• Windows: <code>%USERPROFILE%\\Zomboid\\</code> (e.g. <code>C:\\Users\\&lt;Name&gt;\\Zomboid\\</code>)
          <br>• Linux / macOS: <code>~/Zomboid/</code>
        </li>
        <li>Launch Project Zomboid and direct connect to <code>{ip}:{port}</code>!</li>
      </ol>
    </div>

  </div>

  <!-- Password Unlock Modal -->
  <div class="modal-overlay" id="passwordModal">
    <div class="modal-card">
      <div style="display:flex; justify-content:space-between; align-items:center; margin-bottom:1rem;">
        <h3 style="margin:0; font-size:1.3rem;">🔒 Protected Area</h3>
        <span style="cursor:pointer; color:var(--text-muted); font-size:1.5rem;" onclick="closeModal()">&times;</span>
      </div>
      <p style="color:var(--text-muted); font-size:0.9rem; margin-top:0;">
        Enter the Controller storage password to unlock server files and backup archives:
      </p>
      <input type="password" id="modalPassword" class="input-field" placeholder="Enter STORAGE_PASSWORD" onkeydown="if(event.key==='Enter') submitPassword()" />
      <div style="display:flex; gap:0.75rem; justify-content:flex-end;">
        <button class="btn btn-secondary" onclick="closeModal()">Cancel</button>
        <button class="btn btn-primary" onclick="submitPassword()">Unlock</button>
      </div>
    </div>
  </div>

  <script>
    let pendingAction = null;

    function copyIp(text, btn) {{
      navigator.clipboard.writeText(text);
      const original = btn.innerText;
      btn.innerText = '✅ Copied!';
      setTimeout(() => btn.innerText = original, 2000);
    }}

    function getSavedToken() {{
      return sessionStorage.getItem('pz_token') || new URLSearchParams(window.location.search).get('token') || '';
    }}

    function setSavedToken(token) {{
      if (token) {{
        sessionStorage.setItem('pz_token', token);
        unlockUI(token);
      }}
    }}

    function unlockUI(token) {{
      const serverCard = document.getElementById('serverCard');
      const serverBadge = document.getElementById('serverBadge');
      const serverBtn = document.getElementById('serverBtn');
      const backupsLink = document.getElementById('backupsLink');

      if (serverCard) {{
        serverCard.classList.remove('locked');
        serverBadge.innerHTML = '🔓 Unlocked';
        serverBadge.className = 'card-badge badge-green';
        serverBtn.innerHTML = '⬇️ Download Server Files';
        serverBtn.className = 'btn btn-primary';
        serverBtn.onclick = () => window.location.href = '/server.zip?token=' + encodeURIComponent(token);
      }}

      if (backupsLink) {{
        backupsLink.style.opacity = '1';
        backupsLink.style.borderColor = 'rgba(16, 185, 129, 0.4)';
        backupsLink.innerHTML = '🔓 🗄️ Server Backups & Save Management';
        backupsLink.onclick = () => window.location.href = '/backups?token=' + encodeURIComponent(token);
      }}
    }}

    function handleProtectedDownload(target) {{
      const token = getSavedToken();
      if (token) {{
        window.location.href = '/' + target + '?token=' + encodeURIComponent(token);
      }} else {{
        pendingAction = () => window.location.href = '/' + target + '?token=' + encodeURIComponent(getSavedToken());
        openModal();
      }}
    }}

    function handleBackupsNav() {{
      const token = getSavedToken();
      if (token) {{
        window.location.href = '/backups?token=' + encodeURIComponent(token);
      }} else {{
        pendingAction = () => window.location.href = '/backups?token=' + encodeURIComponent(getSavedToken());
        openModal();
      }}
    }}

    function openModal() {{
      document.getElementById('passwordModal').style.display = 'flex';
      document.getElementById('modalPassword').value = '';
      setTimeout(() => document.getElementById('modalPassword').focus(), 100);
    }}

    function closeModal() {{
      document.getElementById('passwordModal').style.display = 'none';
    }}

    function submitPassword() {{
      const val = document.getElementById('modalPassword').value.trim();
      if (val) {{
        setSavedToken(val);
        closeModal();
        if (pendingAction) {{
          pendingAction();
          pendingAction = null;
        }}
      }}
    }}

    // Check token on load
    window.addEventListener('DOMContentLoaded', () => {{
      const token = getSavedToken();
      if (token) {{
        unlockUI(token);
      }}
    }});
  </script>
</body>
</html>
"""


def render_html_backups(server_info: dict, backups: list, authenticated: bool, token: str = "") -> str:
    status = server_info.get("status", "unknown").lower()
    ip = server_info.get("ip", "pending")
    port = server_info.get("port", 16261)
    
    badge_color = "#eab308"
    status_text = "BOOTING"
    if status == "online":
        badge_color = "#10b981"
        status_text = "SERVER ONLINE"
    elif status in ("stopped", "error", "failed"):
        badge_color = "#ef4444"
        status_text = status.upper()

    if backups:
        rows = []
        for b in backups:
            token_param = f"?token={token}" if token else ""
            dl_url = f"/backups/{b['name']}{token_param}"
            rows.append(f"""
            <tr>
              <td style="font-family:ui-monospace, monospace; font-weight:600; color:#38bdf8;">{b['name']}</td>
              <td style="color:var(--text-muted);">{b['date_str']}</td>
              <td style="color:var(--text-muted);">{b['size_str']}</td>
              <td>
                <a href="{dl_url}" class="btn btn-secondary" style="padding:0.35rem 0.85rem; font-size:0.85rem;" download>⬇️ Download</a>
              </td>
            </tr>
            """)
        backups_table = f"""
        <table style="width:100%; border-collapse:collapse; text-align:left; margin-top:1rem;">
          <thead>
            <tr style="border-bottom: 1px solid var(--card-border); color: var(--text-muted); font-size:0.8rem; text-transform:uppercase; letter-spacing:0.05em;">
              <th style="padding: 0.75rem 0.5rem;">Archive File</th>
              <th style="padding: 0.75rem 0.5rem;">Created</th>
              <th style="padding: 0.75rem 0.5rem;">Size</th>
              <th style="padding: 0.75rem 0.5rem;">Action</th>
            </tr>
          </thead>
          <tbody>
            {''.join(rows)}
          </tbody>
        </table>
        """
    else:
        backups_table = """<p style="color:var(--text-muted); margin-top:1rem; font-style:italic;">No backup archives (.zip) found in /data/backups/.</p>"""

    auth_warning = ""
    if not authenticated:
        auth_warning = """
        <div class="card" style="border-color:#f59e0b; background: rgba(245, 158, 11, 0.08); margin-bottom:1.5rem;">
          <h3 style="margin-top:0; color:#fbbf24; font-size:1.15rem;">🔒 Password Required</h3>
          <p style="color:var(--text-muted); font-size:0.9rem; margin-bottom:1rem;">Enter the Controller password to view and download world backups:</p>
          <form method="GET" action="/backups" style="display:flex; gap:0.5rem; max-width:420px;">
            <input type="password" name="token" placeholder="Enter STORAGE_PASSWORD" style="flex:1; padding:0.65rem 0.85rem; border-radius:8px; background:#090d16; border:1px solid rgba(255,255,255,0.15); color:white;" required />
            <button type="submit" class="btn btn-primary">Unlock</button>
          </form>
        </div>
        """

    return f"""<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>Project Zomboid • Server Backups</title>
  <style>
    :root {{
      --bg: #0b0f19;
      --card-bg: rgba(17, 24, 39, 0.85);
      --card-border: rgba(255, 255, 255, 0.08);
      --text: #f3f4f6;
      --text-muted: #9ca3af;
      --primary: #3b82f6;
      --primary-hover: #2563eb;
    }}
    * {{ box-sizing: border-box; }}
    body {{
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
      background: radial-gradient(circle at top center, #1e1b4b 0%, #0b0f19 55%, #030712 100%);
      color: var(--text);
      margin: 0;
      padding: 2.5rem 1rem;
      min-height: 100vh;
      display: flex;
      justify-content: center;
    }}
    .container {{ max-width: 880px; width: 100%; }}
    .card {{
      background: var(--card-bg);
      backdrop-filter: blur(16px);
      border: 1px solid var(--card-border);
      border-radius: 18px;
      padding: 1.75rem;
      margin-bottom: 1.5rem;
      box-shadow: 0 20px 25px -5px rgba(0, 0, 0, 0.5);
    }}
    .nav-bar {{
      display: flex;
      gap: 0.75rem;
      margin-bottom: 1.5rem;
    }}
    .nav-link {{
      color: var(--text-muted);
      text-decoration: none;
      font-weight: 600;
      font-size: 0.9rem;
      padding: 0.5rem 1rem;
      border-radius: 10px;
      border: 1px solid transparent;
      transition: all 0.15s;
    }}
    .nav-link.active, .nav-link:hover {{
      color: white;
      background-color: rgba(255, 255, 255, 0.06);
      border-color: var(--card-border);
    }}
    h1 {{
      font-size: 1.85rem;
      margin: 0;
      font-weight: 800;
      color: #f1f5f9;
    }}
    .status-badge {{
      display: inline-flex;
      align-items: center;
      gap: 0.5rem;
      padding: 0.4rem 0.9rem;
      border-radius: 9999px;
      font-weight: 700;
      font-size: 0.8rem;
      background: rgba(0, 0, 0, 0.3);
      border: 1px solid var(--card-border);
    }}
    .status-dot {{
      width: 10px;
      height: 10px;
      border-radius: 50%;
      background-color: {badge_color};
    }}
    .ip-box {{
      background: rgba(0, 0, 0, 0.4);
      border: 1px solid rgba(255, 255, 255, 0.06);
      border-radius: 12px;
      padding: 0.85rem 1.25rem;
      margin-top: 1.25rem;
      display: flex;
      justify-content: space-between;
      align-items: center;
    }}
    .btn {{
      display: inline-flex;
      align-items: center;
      justify-content: center;
      gap: 0.5rem;
      font-weight: 600;
      border-radius: 10px;
      padding: 0.65rem 1.25rem;
      font-size: 0.95rem;
      text-decoration: none;
      cursor: pointer;
      border: none;
      transition: all 0.15s;
    }}
    .btn-primary {{
      background: linear-gradient(135deg, #3b82f6 0%, #2563eb 100%);
      color: white;
    }}
    .btn-secondary {{
      background: rgba(255, 255, 255, 0.06);
      color: var(--text);
      border: 1px solid rgba(255, 255, 255, 0.1);
    }}
    .btn-secondary:hover {{
      background: rgba(255, 255, 255, 0.12);
    }}
    table td {{
      padding: 0.75rem 0.5rem;
      border-bottom: 1px solid rgba(255, 255, 255, 0.05);
      font-size: 0.9rem;
    }}
  </style>
</head>
<body>
  <div class="container">
    <div class="nav-bar">
      <a href="/{f'?token={token}' if token else ''}" class="nav-link">📦 Packages</a>
      <a href="/backups{f'?token={token}' if token else ''}" class="nav-link active">🗄️ Backups</a>
    </div>

    <div class="card">
      <div style="display:flex; justify-content:space-between; align-items:center; flex-wrap:wrap; gap:1rem;">
        <h1>🗄️ Server Backups</h1>
        <div class="status-badge">
          <div class="status-dot"></div>
          <span>{status_text}</span>
        </div>
      </div>
      
      <p style="color:var(--text-muted); margin:0.35rem 0 0 0; font-size:0.95rem;">
        Automated and manual server save archives (.zip).
      </p>

      <div class="ip-box">
        <div>
          <span style="font-size:0.75rem; text-transform:uppercase; color:var(--text-muted); display:block;">Server Address</span>
          <span style="font-family:ui-monospace, monospace; font-size:1.2rem; font-weight:700; color:#38bdf8;">{ip}:{port}</span>
        </div>
        <button class="btn btn-secondary" onclick="navigator.clipboard.writeText('{ip}:{port}'); this.innerText='Copied!'; setTimeout(()=>this.innerText='Copy', 2000)">Copy</button>
      </div>
    </div>

    {auth_warning}

    <div class="card">
      <div style="display:flex; justify-content:space-between; align-items:center; flex-wrap:wrap;">
        <h2 style="margin:0; font-size:1.3rem;">Available Backups</h2>
        <span style="color:var(--text-muted); font-size:0.85rem;">{len(backups)} archive(s)</span>
      </div>
      {backups_table}
    </div>

    <div class="card">
      <h2 style="margin:0 0 0.5rem 0; font-size:1.3rem;">⬆️ Upload Backup Archive</h2>
      <p style="color:var(--text-muted); font-size:0.9rem; margin-top:0;">Upload a save <code>.zip</code> into the Controller for restore:</p>
      <form action="/upload{f'?token={token}' if token else ''}" method="POST" enctype="multipart/form-data" style="margin-top:1rem;">
        <input type="file" name="file" accept=".zip" style="color:var(--text-muted); margin-bottom:1rem; display:block;" required />
        <button type="submit" class="btn btn-primary">Upload Archive</button>
      </form>
    </div>
  </div>
</body>
</html>
"""


class StorageHandler(BaseHTTPRequestHandler):
    def log_message(self, format, *args):
        # Silenced request logging to prevent log spam
        pass

    def _send_response_headers(self, code: int, content_type: str, length: int = None, extra_headers: dict = None):
        self.send_response(code)
        self.send_header("Content-Type", content_type)
        if length is not None:
            self.send_header("Content-Length", str(length))
        if extra_headers:
            for k, v in extra_headers.items():
                self.send_header(k, v)
        self.end_headers()

    def _send_error(self, code: int, message: str):
        body = json.dumps({"error": message, "code": code}).encode("utf-8")
        self._send_response_headers(code, "application/json", len(body))
        self.wfile.write(body)

    def _stream_file(self, file_path: Path, filename: str = None, as_attachment: bool = False):
        if not file_path.is_file():
            self._send_error(404, "File not found")
            return

        size = file_path.stat().st_size
        extra = {}
        if as_attachment:
            dl_name = filename or file_path.name
            extra["Content-Disposition"] = f'attachment; filename="{dl_name}"'

        self._send_response_headers(200, "application/octet-stream", size, extra)
        try:
            with open(file_path, "rb") as f:
                while chunk := f.read(CHUNK_SIZE):
                    self.wfile.write(chunk)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def do_GET(self):
        parsed = urlparse(self.path)
        path = parsed.path
        query = parse_qs(parsed.query)

        # 1. Healthz
        if path == "/healthz":
            body = b"ok"
            self._send_response_headers(200, "text/plain", len(body))
            self.wfile.write(body)
            return

        # 2. Main Dashboard (Player Downloads + Server IP + 3 Cards)
        if path in ("/", "/index.html"):
            server_info = get_server_info()
            manifest = get_manifest()
            token = query.get("token", [None])[0] or query.get("key", [None])[0] or query.get("password", [None])[0] or ""
            html = render_html_dashboard(server_info, manifest, token).encode("utf-8")
            self._send_response_headers(200, "text/html; charset=utf-8", len(html))
            self.wfile.write(html)
            return

        # 3. Dedicated Backups Folder / Page
        if path in ("/backups", "/backups/"):
            server_info = get_server_info()
            is_authed = check_auth(self.headers, query)
            token = query.get("token", [None])[0] or query.get("key", [None])[0] or query.get("password", [None])[0] or ""

            # Check if JSON API requested
            accept_header = self.headers.get("Accept", "")
            if "application/json" in accept_header:
                if not is_authed:
                    self._send_error(401, "Unauthorized")
                    return
                backups = get_valid_backups()
                body = json.dumps({"backups": backups}, indent=2).encode("utf-8")
                self._send_response_headers(200, "application/json", len(body))
                self.wfile.write(body)
                return

            # Render HTML Backups page
            backups = get_valid_backups() if is_authed else []
            html = render_html_backups(server_info, backups, is_authed, token).encode("utf-8")
            self._send_response_headers(200, "text/html; charset=utf-8", len(html))
            self.wfile.write(html)
            return

        # 4. Live server_info.json (Public)
        if path == "/server_info.json":
            info = get_server_info()
            body = json.dumps(info, indent=2).encode("utf-8")
            self._send_response_headers(200, "application/json", len(body))
            self.wfile.write(body)
            return

        # 5. Packages Manifest (Public)
        if path in ("/manifest", "/packages_manifest.json"):
            manifest = get_manifest()
            body = json.dumps(manifest, indent=2).encode("utf-8")
            self._send_response_headers(200, "application/json", len(body))
            self.wfile.write(body)
            return

        # 6. Public Downloads: client.zip & common.zip
        if path == "/client.zip":
            self._stream_file(PACKAGES_DIR / "client.zip", "client.zip", as_attachment=True)
            return

        if path == "/common.zip":
            self._stream_file(PACKAGES_DIR / "common.zip", "common.zip", as_attachment=True)
            return

        # 7. PROTECTED DOWNLOAD: server.zip (Game Server Only)
        if path == "/server.zip":
            if not check_auth(self.headers, query):
                self._send_error(401, "Unauthorized: Valid password/token required for server.zip")
                return
            log("Authenticated download of server.zip accepted.")
            self._stream_file(PACKAGES_DIR / "server.zip", "server.zip", as_attachment=True)
            return

        # 8. PROTECTED DOWNLOAD: /backups/<filename>
        if path.startswith("/backups/"):
            if not check_auth(self.headers, query):
                self._send_error(401, "Unauthorized: Valid password/token required to access backups")
                return
            filename = os.path.basename(path[len("/backups/"):])
            # Security: ensure file is .zip and not hidden/.gitkeep
            if filename.startswith(".") or not filename.lower().endswith(".zip"):
                self._send_error(404, "File not found")
                return
            backup_file = BACKUPS_DIR / filename
            if backup_file.is_file():
                log(f"Authenticated backup download: {filename}")
                self._stream_file(backup_file, filename, as_attachment=True)
            else:
                self._send_error(404, "Backup not found")
            return

        self._send_error(404, "Not Found")

    def do_POST(self):
        parsed = urlparse(self.path)
        path = parsed.path
        query = parse_qs(parsed.query)

        # 1. PROTECTED UPLOAD: POST /upload
        if path in ("/upload", "/upload/"):
            if not check_auth(self.headers, query):
                self._send_error(401, "Unauthorized: Valid password/token required for upload")
                return

            ctype, pdict = cgi.parse_header(self.headers.get("Content-Type", ""))
            if ctype != "multipart/form-data":
                self._send_error(400, "Content-Type must be multipart/form-data")
                return

            try:
                pdict["boundary"] = bytes(pdict["boundary"], "utf-8")
                pdict["CONTENT-LENGTH"] = int(self.headers.get("Content-Length", 0))
                fields = cgi.parse_multipart(self.rfile, pdict)

                uploaded_files = []
                BACKUPS_DIR.mkdir(parents=True, exist_ok=True)

                for field_name, files_data in fields.items():
                    for data in files_data:
                        if isinstance(data, bytes) and data:
                            filename = f"backup_upload_{datetime.datetime.now().strftime('%Y%m%d_%H%M%S')}_{secrets.token_hex(2)}.zip"
                            save_path = BACKUPS_DIR / filename
                            save_path.write_bytes(data)
                            uploaded_files.append(filename)

                log(f"Authenticated upload completed: {uploaded_files}")
                token = query.get("token", [None])[0] or query.get("key", [None])[0] or ""
                token_param = f"?token={token}" if token else ""
                
                # If uploaded via browser form, redirect back to /backups
                accept_header = self.headers.get("Accept", "")
                if "text/html" in accept_header or not "application/json" in accept_header:
                    self.send_response(303)
                    self.send_header("Location", f"/backups{token_param}")
                    self.end_headers()
                    return

                body = json.dumps({"ok": True, "files": uploaded_files}).encode("utf-8")
                self._send_response_headers(200, "application/json", len(body))
                self.wfile.write(body)
            except Exception as e:
                log(f"Upload error: {e}")
                self._send_error(500, f"Upload processing failed: {e}")
            return

        self._send_error(404, "Not Found")


def run_server():
    PACKAGES_DIR.mkdir(parents=True, exist_ok=True)
    BACKUPS_DIR.mkdir(parents=True, exist_ok=True)

    server = ThreadingHTTPServer(("0.0.0.0", HTTP_PORT), StorageHandler)
    log(f"PZ Controller Storage Server listening on port {HTTP_PORT}")
    log(f"Password protection: {'ACTIVE' if STORAGE_PASSWORD else 'NO PASSWORD SET (WARNING)'}")
    server.serve_forever()


if __name__ == "__main__":
    run_server()
