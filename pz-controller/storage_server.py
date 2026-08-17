#!/usr/bin/env python3
"""
storage_server.py — Secure HTTP file & storage server for PZ Controller.

Serves:
  - Public web dashboard (server IP, status, mod list, client download links)
  - Dedicated Backups page (/backups) with password access, backup listing, and upload form
  - Filtered backup list (hiding .gitkeep and non-zip files)
  - Public downloads: /client.zip, /common.zip, /server_info.json, /healthz
  - Protected downloads: /server.zip, /backups/<filename> (requires password/token)
  - Protected uploads: POST /upload (for backup uploads)
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


def render_html_dashboard(server_info: dict, manifest: dict) -> str:
    status = server_info.get("status", "unknown").lower()
    ip = server_info.get("ip", "pending")
    port = server_info.get("port", 16261)
    
    badge_color = "#eab308"  # yellow
    if status == "online":
        badge_color = "#22c55e"  # green
    elif status in ("stopped", "error", "failed"):
        badge_color = "#ef4444"  # red

    client_info = manifest.get("client", {})
    common_info = manifest.get("common", {})
    client_size_mb = f"{client_info.get('size', 0) / (1024*1024):.1f} MB" if client_info.get('size') else "Ready"
    common_size_mb = f"{common_info.get('size', 0) / (1024*1024):.1f} MB" if common_info.get('size') else "Ready"

    return f"""<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>Project Zomboid Server & Mod Hub</title>
  <style>
    :root {{
      --bg: #0f172a;
      --card-bg: #1e293b;
      --text: #f8fafc;
      --text-muted: #94a3b8;
      --primary: #3b82f6;
      --primary-hover: #2563eb;
      --border: #334155;
    }}
    body {{
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
      background-color: var(--bg);
      color: var(--text);
      margin: 0;
      padding: 2rem 1rem;
      display: flex;
      justify-content: center;
    }}
    .container {{
      max-width: 760px;
      width: 100%;
    }}
    .card {{
      background-color: var(--card-bg);
      border: 1px solid var(--border);
      border-radius: 12px;
      padding: 1.75rem;
      margin-bottom: 1.5rem;
      box-shadow: 0 10px 15px -3px rgba(0,0,0,0.3);
    }}
    .nav-bar {{
      display: flex;
      gap: 0.75rem;
      margin-bottom: 1.25rem;
    }}
    .nav-link {{
      color: var(--text-muted);
      text-decoration: none;
      font-weight: 500;
      padding: 0.5rem 1rem;
      border-radius: 8px;
      border: 1px solid transparent;
      transition: all 0.15s;
    }}
    .nav-link.active, .nav-link:hover {{
      color: white;
      background-color: rgba(255, 255, 255, 0.05);
      border-color: var(--border);
    }}
    h1 {{
      font-size: 1.75rem;
      margin: 0;
      color: #f1f5f9;
    }}
    .status-badge {{
      display: inline-flex;
      align-items: center;
      gap: 0.5rem;
      padding: 0.35rem 0.85rem;
      border-radius: 9999px;
      font-weight: 600;
      font-size: 0.875rem;
      background: rgba(255, 255, 255, 0.05);
      border: 1px solid var(--border);
    }}
    .status-dot {{
      width: 10px;
      height: 10px;
      border-radius: 50%;
      background-color: {badge_color};
    }}
    .ip-box {{
      background: #090d16;
      border: 1px solid #1e293b;
      border-radius: 8px;
      padding: 0.75rem 1rem;
      font-family: monospace;
      font-size: 1.1rem;
      display: flex;
      justify-content: space-between;
      align-items: center;
      margin: 1rem 0;
    }}
    .btn {{
      display: inline-block;
      background-color: var(--primary);
      color: white;
      text-decoration: none;
      padding: 0.65rem 1.25rem;
      border-radius: 8px;
      font-weight: 500;
      cursor: pointer;
      border: none;
      transition: background-color 0.15s;
    }}
    .btn:hover {{
      background-color: var(--primary-hover);
    }}
    .btn-secondary {{
      background-color: #334155;
    }}
    .btn-secondary:hover {{
      background-color: #475569;
    }}
    .download-grid {{
      display: grid;
      grid-template-columns: 1fr 1fr;
      gap: 1rem;
      margin-top: 1rem;
    }}
    @media (max-width: 600px) {{
      .download-grid {{
        grid-template-columns: 1fr;
      }}
    }}
    .download-card {{
      border: 1px solid var(--border);
      border-radius: 8px;
      padding: 1rem;
      background: rgba(15, 23, 42, 0.6);
      text-align: center;
    }}
    .download-card h3 {{
      margin: 0 0 0.5rem 0;
      font-size: 1.1rem;
    }}
    .download-card p {{
      color: var(--text-muted);
      font-size: 0.85rem;
      margin-bottom: 1rem;
    }}
    .instructions {{
      font-size: 0.9rem;
      color: var(--text-muted);
      line-height: 1.5;
      margin-top: 1.5rem;
      border-top: 1px solid var(--border);
      padding-top: 1rem;
    }}
    code {{
      background: #090d16;
      padding: 0.2rem 0.4rem;
      border-radius: 4px;
      font-family: monospace;
      color: #38bdf8;
    }}
  </style>
</head>
<body>
  <div class="container">
    <div class="nav-bar">
      <a href="/" class="nav-link active">📦 Player Packages</a>
      <a href="/backups" class="nav-link">🗄️ Server Backups</a>
    </div>

    <div class="card">
      <div style="display:flex; justify-content:space-between; align-items:center; flex-wrap:wrap; gap:1rem;">
        <h1>🧟 Project Zomboid Server</h1>
        <div class="status-badge">
          <div class="status-dot"></div>
          <span style="text-transform: uppercase;">{status}</span>
        </div>
      </div>
      
      <p style="color:var(--text-muted); margin-top:0.25rem;">Live server status and player mod packages.</p>

      <div class="ip-box">
        <span><strong>Server Address:</strong> {ip}:{port}</span>
        <button class="btn btn-secondary" onclick="navigator.clipboard.writeText('{ip}:{port}'); this.innerText='Copied!'; setTimeout(()=>this.innerText='Copy', 2000)">Copy</button>
      </div>
    </div>

    <div class="card">
      <h2>📦 Download Game Packages</h2>
      <p style="color:var(--text-muted);">Download the pre-bundled mods and configs before connecting:</p>

      <div class="download-grid">
        <div class="download-card">
          <h3>Common Package</h3>
          <p>Shared server & client mods ({common_size_mb})</p>
          <a href="/common.zip" class="btn" download>Download common.zip</a>
        </div>
        <div class="download-card">
          <h3>Client Package</h3>
          <p>Client mods & configurations ({client_size_mb})</p>
          <a href="/client.zip" class="btn" download>Download client.zip</a>
        </div>
      </div>

      <div class="instructions">
        <strong>How to install:</strong>
        <ol style="padding-left: 1.25rem; margin-top: 0.5rem;">
          <li>Download both <code>common.zip</code> and <code>client.zip</code>.</li>
          <li>Extract both archives directly into your local Zomboid directory:<br>
              Windows: <code>%USERPROFILE%\\Zomboid\\</code><br>
              Linux / macOS: <code>~/Zomboid/</code>
          </li>
          <li>Launch Project Zomboid and connect to <code>{ip}:{port}</code>!</li>
        </ol>
      </div>
    </div>
  </div>
</body>
</html>
"""


def render_html_backups(server_info: dict, backups: list, authenticated: bool, token: str = "") -> str:
    status = server_info.get("status", "unknown").lower()
    ip = server_info.get("ip", "pending")
    port = server_info.get("port", 16261)
    
    badge_color = "#eab308"
    if status == "online":
        badge_color = "#22c55e"
    elif status in ("stopped", "error", "failed"):
        badge_color = "#ef4444"

    # Render backup rows
    if backups:
        rows = []
        for b in backups:
            token_param = f"?token={token}" if token else ""
            dl_url = f"/backups/{b['name']}{token_param}"
            rows.append(f"""
            <tr>
              <td style="font-family:monospace; font-weight:600;">{b['name']}</td>
              <td style="color:var(--text-muted);">{b['date_str']}</td>
              <td style="color:var(--text-muted);">{b['size_str']}</td>
              <td>
                <a href="{dl_url}" class="btn btn-secondary" style="padding:0.35rem 0.75rem; font-size:0.85rem;" download>Download</a>
              </td>
            </tr>
            """)
        backups_table = f"""
        <table style="width:100%; border-collapse:collapse; text-align:left; margin-top:1rem;">
          <thead>
            <tr style="border-bottom: 1px solid var(--border); color: var(--text-muted); font-size:0.85rem;">
              <th style="padding: 0.6rem 0.5rem;">Backup File</th>
              <th style="padding: 0.6rem 0.5rem;">Date Created</th>
              <th style="padding: 0.6rem 0.5rem;">Size</th>
              <th style="padding: 0.6rem 0.5rem;">Action</th>
            </tr>
          </thead>
          <tbody>
            {''.join(rows)}
          </tbody>
        </table>
        """
    else:
        backups_table = """<p style="color:var(--text-muted); margin-top:1rem; font-style:italic;">No backup archives (.zip) found in /data/backups/.</p>"""

    auth_section = ""
    if not authenticated:
        auth_section = """
        <div class="card" style="border-color:#f59e0b; background: rgba(245, 158, 11, 0.05);">
          <h3 style="margin-top:0; color:#f59e0b;">🔒 Password Required</h3>
          <p style="color:var(--text-muted); font-size:0.9rem;">Server backups and uploads are protected. Enter the storage password below:</p>
          <form method="GET" action="/backups" style="display:flex; gap:0.5rem; max-width:400px;">
            <input type="password" name="token" placeholder="Enter STORAGE_PASSWORD" style="flex:1; padding:0.6rem 0.75rem; border-radius:6px; background:#090d16; border:1px solid var(--border); color:white;" required />
            <button type="submit" class="btn">Unlock</button>
          </form>
        </div>
        """

    return f"""<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>Project Zomboid Server Backups</title>
  <style>
    :root {{
      --bg: #0f172a;
      --card-bg: #1e293b;
      --text: #f8fafc;
      --text-muted: #94a3b8;
      --primary: #3b82f6;
      --primary-hover: #2563eb;
      --border: #334155;
    }}
    body {{
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
      background-color: var(--bg);
      color: var(--text);
      margin: 0;
      padding: 2rem 1rem;
      display: flex;
      justify-content: center;
    }}
    .container {{
      max-width: 760px;
      width: 100%;
    }}
    .card {{
      background-color: var(--card-bg);
      border: 1px solid var(--border);
      border-radius: 12px;
      padding: 1.75rem;
      margin-bottom: 1.5rem;
      box-shadow: 0 10px 15px -3px rgba(0,0,0,0.3);
    }}
    .nav-bar {{
      display: flex;
      gap: 0.75rem;
      margin-bottom: 1.25rem;
    }}
    .nav-link {{
      color: var(--text-muted);
      text-decoration: none;
      font-weight: 500;
      padding: 0.5rem 1rem;
      border-radius: 8px;
      border: 1px solid transparent;
      transition: all 0.15s;
    }}
    .nav-link.active, .nav-link:hover {{
      color: white;
      background-color: rgba(255, 255, 255, 0.05);
      border-color: var(--border);
    }}
    h1 {{
      font-size: 1.75rem;
      margin: 0;
      color: #f1f5f9;
    }}
    .status-badge {{
      display: inline-flex;
      align-items: center;
      gap: 0.5rem;
      padding: 0.35rem 0.85rem;
      border-radius: 9999px;
      font-weight: 600;
      font-size: 0.875rem;
      background: rgba(255, 255, 255, 0.05);
      border: 1px solid var(--border);
    }}
    .status-dot {{
      width: 10px;
      height: 10px;
      border-radius: 50%;
      background-color: {badge_color};
    }}
    .ip-box {{
      background: #090d16;
      border: 1px solid #1e293b;
      border-radius: 8px;
      padding: 0.75rem 1rem;
      font-family: monospace;
      font-size: 1.1rem;
      display: flex;
      justify-content: space-between;
      align-items: center;
      margin: 1rem 0;
    }}
    .btn {{
      display: inline-block;
      background-color: var(--primary);
      color: white;
      text-decoration: none;
      padding: 0.65rem 1.25rem;
      border-radius: 8px;
      font-weight: 500;
      cursor: pointer;
      border: none;
      transition: background-color 0.15s;
    }}
    .btn:hover {{
      background-color: var(--primary-hover);
    }}
    .btn-secondary {{
      background-color: #334155;
    }}
    .btn-secondary:hover {{
      background-color: #475569;
    }}
    table td {{
      padding: 0.65rem 0.5rem;
      border-bottom: 1px solid rgba(51, 65, 85, 0.5);
      font-size: 0.9rem;
    }}
  </style>
</head>
<body>
  <div class="container">
    <div class="nav-bar">
      <a href="/" class="nav-link">📦 Player Packages</a>
      <a href="/backups{f'?token={token}' if token else ''}" class="nav-link active">🗄️ Server Backups</a>
    </div>

    <div class="card">
      <div style="display:flex; justify-content:space-between; align-items:center; flex-wrap:wrap; gap:1rem;">
        <h1>🗄️ Server Backups</h1>
        <div class="status-badge">
          <div class="status-dot"></div>
          <span style="text-transform: uppercase;">{status}</span>
        </div>
      </div>
      
      <p style="color:var(--text-muted); margin-top:0.25rem;">Server world backups (.zip) and upload management.</p>

      <div class="ip-box">
        <span><strong>Server Address:</strong> {ip}:{port}</span>
        <button class="btn btn-secondary" onclick="navigator.clipboard.writeText('{ip}:{port}'); this.innerText='Copied!'; setTimeout(()=>this.innerText='Copy', 2000)">Copy</button>
      </div>
    </div>

    {auth_section}

    <div class="card">
      <div style="display:flex; justify-content:space-between; align-items:center; flex-wrap:wrap;">
        <h2 style="margin:0;">Available Backups</h2>
        <span style="color:var(--text-muted); font-size:0.85rem;">{len(backups)} archive(s)</span>
      </div>
      {backups_table}
    </div>

    <div class="card">
      <h2>⬆️ Upload Backup Archive</h2>
      <p style="color:var(--text-muted); font-size:0.9rem;">Upload an existing world save <code>.zip</code> into the Controller:</p>
      <form action="/upload{f'?token={token}' if token else ''}" method="POST" enctype="multipart/form-data" style="margin-top:1rem;">
        <input type="file" name="file" accept=".zip" style="color:var(--text-muted); margin-bottom:1rem; display:block;" required />
        <button type="submit" class="btn">Upload Backup .zip</button>
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

        # 2. Main Dashboard (Player Downloads + Server IP)
        if path in ("/", "/index.html"):
            server_info = get_server_info()
            manifest = get_manifest()
            html = render_html_dashboard(server_info, manifest).encode("utf-8")
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
