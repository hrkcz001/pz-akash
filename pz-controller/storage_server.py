#!/usr/bin/env python3
"""
storage_server.py — Secure HTTP file & storage server for PZ Controller.

Serves:
  - Public web dashboard (server IP, status, mod list, client download links)
  - Public downloads: /client.zip, /common.zip, /server_info.json, /healthz
  - Protected downloads: /server.zip, /backups/* (requires password/token)
  - Protected uploads: POST /upload (for backup uploads)
"""

import base64
import cgi
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
        # If no password configured, protect sensitive endpoints by default
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

    # 4. Query param (?token=... or ?key=...)
    query_token = query_params.get("token", [None])[0] or query_params.get("key", [None])[0]
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
    client_size_mb = f"{client_info.get('size', 0) / (1024*1024):.1f} MB" if client_info.get('size') else "Available"
    common_size_mb = f"{common_info.get('size', 0) / (1024*1024):.1f} MB" if common_info.get('size') else "Available"

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
      max-width: 720px;
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
    h1 {{
      font-size: 1.75rem;
      margin-top: 0;
      margin-bottom: 0.5rem;
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
        <span><strong>Server IP:</strong> {ip}:{port}</span>
        <button class="btn btn-secondary" onclick="navigator.clipboard.writeText('{ip}:{port}'); this.innerText='Copied!'; setTimeout(()=>this.innerText='Copy', 2000)">Copy</button>
      </div>
    </div>

    <div class="card">
      <h2>📦 Player Downloads</h2>
      <p style="color:var(--text-muted);">Download the pre-bundled mods and configs before joining the server:</p>

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
          <li>Extract both files directly into your Zomboid user folder:<br>
              Windows: <code>%USERPROFILE%\\Zomboid\\</code><br>
              Linux / Mac: <code>~/Zomboid/</code>
          </li>
          <li>Launch Project Zomboid and connect to <code>{ip}:{port}</code>!</li>
        </ol>
      </div>
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

        # 2. Web UI Dashboard
        if path in ("/", "/index.html"):
            server_info = get_server_info()
            manifest = get_manifest()
            html = render_html_dashboard(server_info, manifest).encode("utf-8")
            self._send_response_headers(200, "text/html; charset=utf-8", len(html))
            self.wfile.write(html)
            return

        # 3. Live server_info.json (Public)
        if path == "/server_info.json":
            info = get_server_info()
            body = json.dumps(info, indent=2).encode("utf-8")
            self._send_response_headers(200, "application/json", len(body))
            self.wfile.write(body)
            return

        # 4. Packages Manifest (Public)
        if path in ("/manifest", "/packages_manifest.json"):
            manifest = get_manifest()
            body = json.dumps(manifest, indent=2).encode("utf-8")
            self._send_response_headers(200, "application/json", len(body))
            self.wfile.write(body)
            return

        # 5. Public Downloads: client.zip & common.zip
        if path == "/client.zip":
            self._stream_file(PACKAGES_DIR / "client.zip", "client.zip", as_attachment=True)
            return

        if path == "/common.zip":
            self._stream_file(PACKAGES_DIR / "common.zip", "common.zip", as_attachment=True)
            return

        # 6. PROTECTED DOWNLOAD: server.zip (Game Server Only)
        if path == "/server.zip":
            if not check_auth(self.headers, query):
                self._send_error(401, "Unauthorized: Valid password/token required for server.zip")
                return
            log("Authenticated download of server.zip accepted.")
            self._stream_file(PACKAGES_DIR / "server.zip", "server.zip", as_attachment=True)
            return

        # 7. PROTECTED DOWNLOAD: /backups/<filename>
        if path.startswith("/backups/"):
            if not check_auth(self.headers, query):
                self._send_error(401, "Unauthorized: Valid password/token required to access backups")
                return
            filename = os.path.basename(path[len("/backups/"):])
            backup_file = BACKUPS_DIR / filename
            if backup_file.is_file() and backup_file.suffix == ".zip":
                log(f"Authenticated backup download: {filename}")
                self._stream_file(backup_file, filename, as_attachment=True)
            else:
                self._send_error(404, "Backup not found")
            return

        # 8. PROTECTED: /backups (List backups)
        if path in ("/backups", "/backups/"):
            if not check_auth(self.headers, query):
                self._send_error(401, "Unauthorized: Valid password/token required")
                return
            backups = []
            if BACKUPS_DIR.is_dir():
                for f in sorted(BACKUPS_DIR.glob("*.zip"), reverse=True):
                    backups.append({
                        "name": f.name,
                        "size": f.stat().st_size,
                        "mtime": int(f.stat().st_mtime)
                    })
            body = json.dumps({"backups": backups}, indent=2).encode("utf-8")
            self._send_response_headers(200, "application/json", len(body))
            self.wfile.write(body)
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
                    # Handle multiple files or single file
                    for data in files_data:
                        if isinstance(data, bytes):
                            filename = f"upload_{int(time.time())}_{secrets.token_hex(4)}.zip"
                            save_path = BACKUPS_DIR / filename
                            save_path.write_bytes(data)
                            uploaded_files.append(filename)

                log(f"Authenticated upload completed: {uploaded_files}")
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
