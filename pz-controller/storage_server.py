#!/usr/bin/env python3
"""
storage_server.py — Secure HTTP file & storage server for PZ Controller.

Features:
  - Modern, high-end responsive dark UI dashboard
  - Prominent "Download Game Client (.torrent)" banner above package cards
  - 3 main action cards: Client Files, Common Files, and Server Files (faded with Lock)
  - Detailed stats: Mod count, non-mod config files count, and package size
  - Dedicated /backups view with .gitkeep filtered out
  - Split Server IP and Port into 2 fancy standalone fields with 1-click copy
  - Hidden IP/Port when server is booting or stopped (with custom status cards)
  - Dynamic Readme plate rendered from pz-saves/README.md or pz-saves/README
  - In-browser password unlock modal (unlocks server files and backups seamlessly)
  - Public endpoints: /, /client.zip, /common.zip, /game.torrent, /server_info.json, /healthz
  - Protected endpoints: /server.zip, /backups, /backups/<filename>, /upload
"""

import base64
import datetime
import email
from email.parser import BytesParser
import hmac
import json
import os
import re
import secrets
import subprocess
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlparse

HTTP_PORT = int(os.environ.get("HTTP_PORT", "8000"))
STORAGE_PASSWORD = os.environ.get("STORAGE_PASSWORD") or os.environ.get("CONTROLLER_SECRET") or os.environ.get("ADMIN_PASSWORD", "")
SERVER_FILES_PASSWORD = os.environ.get("SERVER_FILES_PASSWORD") or os.environ.get("SERVERFILES_PASSWORD") or STORAGE_PASSWORD
BACKUPS_PASSWORD = os.environ.get("BACKUPS_PASSWORD") or os.environ.get("BACKUP_PASSWORD") or os.environ.get("BACKUPS_SECRET") or STORAGE_PASSWORD
PACKAGES_DIR = Path(os.environ.get("PACKAGES_DIR", "/data/packages"))
BACKUPS_DIR = Path(os.environ.get("BACKUPS_DIR", "/data/backups"))
SERVES_REPO = Path(os.environ.get("SERVES_REPO", "/root/pz-saves"))

CHUNK_SIZE = 256 * 1024  # 256 KB streaming chunk


def log(msg: str):
    print(f"[storage_server] {msg}", flush=True)


def extract_credentials(headers, query_params) -> list:
    """Extract candidate tokens/passwords from Bearer auth, Basic auth, custom headers, and query parameters."""
    tokens = []
    # 1. Bearer token in Authorization header
    auth_header = headers.get("Authorization", "")
    if auth_header.startswith("Bearer "):
        t = auth_header[len("Bearer "):].strip()
        if t: tokens.append(t)

    # 2. Basic Auth in Authorization header
    if auth_header.startswith("Basic "):
        try:
            raw = base64.b64decode(auth_header[len("Basic "):].strip()).decode("utf-8")
            if ":" in raw:
                _, password = raw.split(":", 1)
                if password: tokens.append(password)
        except Exception:
            pass

    # 3. Custom headers
    for h in ("X-Auth-Token", "X-Storage-Secret", "X-Controller-Secret", "X-Server-Files-Password", "X-Backups-Password"):
        val = headers.get(h)
        if val and val.strip():
            tokens.append(val.strip())

    # 4. Query parameters
    for q in ("token", "key", "password", "server_token", "backup_token", "server_password", "backup_password"):
        vals = query_params.get(q, [])
        if vals and vals[0] and vals[0].strip():
            tokens.append(vals[0].strip())

    return tokens


def check_server_files_auth(headers, query_params) -> bool:
    """Validate credentials against SERVER_FILES_PASSWORD (or master STORAGE_PASSWORD)."""
    allowed = [p for p in (SERVER_FILES_PASSWORD, STORAGE_PASSWORD) if p]
    if not allowed:
        log("WARNING: Neither SERVER_FILES_PASSWORD nor STORAGE_PASSWORD is set! Denying access.")
        return False

    creds = extract_credentials(headers, query_params)
    for c in creds:
        for expected in allowed:
            if hmac.compare_digest(c, expected):
                return True
    return False


def check_backups_auth(headers, query_params) -> bool:
    """Validate credentials against BACKUPS_PASSWORD (or master STORAGE_PASSWORD)."""
    allowed = [p for p in (BACKUPS_PASSWORD, STORAGE_PASSWORD) if p]
    if not allowed:
        log("WARNING: Neither BACKUPS_PASSWORD nor STORAGE_PASSWORD is set! Denying access.")
        return False

    creds = extract_credentials(headers, query_params)
    for c in creds:
        for expected in allowed:
            if hmac.compare_digest(c, expected):
                return True
    return False


_last_git_refresh = 0.0


def refresh_saves_repo():
    """Ensure pz-saves local repo is synced with origin/main."""
    global _last_git_refresh
    now = time.time()
    if now - _last_git_refresh < 3.0:
        return
    _last_git_refresh = now
    try:
        lock_dir = Path("/data/git_repo.lock")
        if lock_dir.exists():
            return
        subprocess.run(
            ["git", "-C", str(SERVES_REPO), "fetch", "origin", "main"],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=4
        )
        subprocess.run(
            ["git", "-C", str(SERVES_REPO), "checkout", "-B", "main", "origin/main"],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=3
        )
    except Exception:
        pass


def get_server_info():
    """Read server_info.json from pz-saves repo with git sync. Default to stopped status and empty IP."""
    refresh_saves_repo()
    info_path = SERVES_REPO / "server_info.json"
    if info_path.is_file():
        try:
            content = info_path.read_text(encoding="utf-8")
            if "<<<<<<<" in content:
                # Auto-heal conflicted file
                subprocess.run(
                    ["git", "-C", str(SERVES_REPO), "checkout", "-B", "main", "origin/main"],
                    stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=3
                )
                content = info_path.read_text(encoding="utf-8")
            data = json.loads(content)
            st = str(data.get("status", "stopped")).lower()
            return {
                "ip": data.get("ip", "") if st == "online" else "",
                "raw_ip": data.get("ip", ""),
                "port": int(data.get("port", 16261)),
                "status": st
            }
        except Exception:
            pass
    return {"ip": "", "raw_ip": "", "port": 16261, "status": "stopped"}


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


def markdown_to_html(md_text: str) -> str:
    """Clean markdown to semantic HTML parser."""
    if not md_text or not md_text.strip():
        return "<p>No installation instructions provided.</p>"
    
    lines = md_text.replace("\r\n", "\n").split("\n")
    html_out = []
    in_code_block = False
    in_ul = False
    in_ol = False
    
    def process_inline(text: str) -> str:
        text = text.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")
        text = re.sub(r"`([^`]+)`", r"<code>\1</code>", text)
        text = re.sub(r"\*\*([^*]+)\*\*", r"<strong>\1</strong>", text)
        text = re.sub(r"__([^_]+)__", r"<strong>\1</strong>", text)
        text = re.sub(r"\*([^*]+)\*", r"<em>\1</em>", text)
        text = re.sub(r"_([^_]+)_", r"<em>\1</em>", text)
        text = re.sub(r"\[([^\]]+)\]\(([^)]+)\)", r'<a href="\2" target="_blank" rel="noopener">\1</a>', text)
        return text

    for raw_line in lines:
        line = raw_line.rstrip()
        stripped = line.strip()
        
        # Code block toggle (handles indented and non-indented ```)
        if stripped.startswith("```"):
            if in_code_block:
                html_out.append("</code></pre>")
                in_code_block = False
            else:
                if in_ul: html_out.append("</ul>"); in_ul = False
                if in_ol: html_out.append("</ol>"); in_ol = False
                lang = stripped[3:].strip()
                html_out.append(f'<pre class="code-block {lang}"><code>')
                in_code_block = True
            continue
            
        if in_code_block:
            escaped = (line.replace("&", "&amp;")
                           .replace("<", "&lt;")
                           .replace(">", "&gt;"))
            html_out.append(escaped)
            continue

        # Blank line
        if not stripped:
            if in_ul: html_out.append("</ul>"); in_ul = False
            if in_ol: html_out.append("</ol>"); in_ol = False
            continue

        # Headings
        if stripped.startswith("### "):
            if in_ul: html_out.append("</ul>"); in_ul = False
            if in_ol: html_out.append("</ol>"); in_ol = False
            html_out.append(f"<h3>{process_inline(stripped[4:])}</h3>")
            continue
        if stripped.startswith("## "):
            if in_ul: html_out.append("</ul>"); in_ul = False
            if in_ol: html_out.append("</ol>"); in_ol = False
            html_out.append(f"<h2>{process_inline(stripped[3:])}</h2>")
            continue
        if stripped.startswith("# "):
            if in_ul: html_out.append("</ul>"); in_ul = False
            if in_ol: html_out.append("</ol>"); in_ol = False
            html_out.append(f"<h1>{process_inline(stripped[2:])}</h1>")
            continue

        # Blockquote / Notes
        if stripped.startswith("> "):
            if in_ul: html_out.append("</ul>"); in_ul = False
            if in_ol: html_out.append("</ol>"); in_ol = False
            html_out.append(f"<blockquote>{process_inline(stripped[2:])}</blockquote>")
            continue

        # Horizontal rule
        if re.match(r"^(\-{3,}|\*{3,}|_{3,})$", stripped):
            if in_ul: html_out.append("</ul>"); in_ul = False
            if in_ol: html_out.append("</ol>"); in_ol = False
            html_out.append("<hr>")
            continue

        # Unordered list
        if stripped.startswith(("- ", "* ", "+ ")):
            if in_ol: html_out.append("</ol>"); in_ol = False
            if not in_ul:
                html_out.append("<ul>")
                in_ul = True
            html_out.append(f"<li>{process_inline(stripped[2:])}</li>")
            continue

        # Ordered list
        m_ol = re.match(r"^(\d+)\.\s+(.*)$", stripped)
        if m_ol:
            if in_ul: html_out.append("</ul>"); in_ul = False
            if not in_ol:
                html_out.append("<ol>")
                in_ol = True
            html_out.append(f"<li>{process_inline(m_ol.group(2))}</li>")
            continue

        # Paragraph
        if in_ul: html_out.append("</ul>"); in_ul = False
        if in_ol: html_out.append("</ol>"); in_ol = False
        html_out.append(f"<p>{process_inline(stripped)}</p>")

    if in_code_block: html_out.append("</code></pre>")
    if in_ul: html_out.append("</ul>")
    if in_ol: html_out.append("</ol>")

    return "\n".join(html_out)


def get_readme_html() -> str:
    """Read README.md from pz-saves repository and render as HTML."""
    candidates = [
        SERVES_REPO / "README.md",
        Path("/data/README.md")
    ]
    for c in candidates:
        if c.is_file():
            try:
                content = c.read_text(encoding="utf-8")
                if content.strip():
                    return markdown_to_html(content)
            except Exception as e:
                log(f"Error reading {c}: {e}")

    # Fallback instructions
    fallback_md = """# ☣️ Quick Join Guide

1. **Clean Client**: Download via **`game.torrent`** above (or delete existing mods in `Zomboid/mods`).
2. **Download Mods**: Get both **`common.zip`** and **`client.zip`** above.
3. **Extract & Overwrite**: Unzip both into your `Zomboid` folder:
   * **Windows**: `%USERPROFILE%\\Zomboid\\`
   * **Linux / Deck**: `~/Zomboid/`
4. **Connect**: Launch PZ → **Join** → Enter **IP**, Port `16261`, and Server Password `1488`.
"""
    return markdown_to_html(fallback_md)


def format_pkg_stats(info: dict) -> str:
    """Format package subtitle stats (mods count, files count, size)."""
    mods = info.get("mods_count", 0)
    files = info.get("files_count", 0)
    size_bytes = info.get("size", 0)
    
    size_str = f"{size_bytes / (1024*1024):.1f} MB" if size_bytes >= 1024*1024 else (f"{size_bytes / 1024:.1f} KB" if size_bytes > 0 else "Ready")
    
    parts = []
    if mods > 0:
        parts.append(f"{mods} mod{'s' if mods != 1 else ''}")
    if files > 0:
        parts.append(f"{files} file{'s' if files != 1 else ''}")
    parts.append(size_str)
    
    return " • ".join(parts)


def render_html_dashboard(server_info: dict, manifest: dict, server_token: str = "", backup_token: str = "") -> str:
    status = server_info.get("status", "stopped").lower()
    ip = server_info.get("raw_ip", "") if status == "online" else ""
    port = server_info.get("port", 16261)
    
    is_online = (status == "online" and bool(ip) and ip != "pending")
    is_booting = (status == "booting")
    
    # Status Badge
    if is_online:
        badge_class = "badge-online"
        badge_text = "ONLINE"
        dot_html = '<span class="status-dot online"></span>'
    elif is_booting:
        badge_class = "badge-booting"
        badge_text = "STARTING UP"
        dot_html = '<span class="status-dot booting"></span>'
    else:
        badge_class = "badge-offline"
        badge_text = "OFFLINE"
        dot_html = '<span class="status-dot offline"></span>'

    # Connection Address / Status Widget (IP, Port, Server Password)
    if is_online:
        address_widget_html = f"""
        <div class="address-grid">
          <div class="address-card">
            <div class="address-label">SERVER IP</div>
            <div class="address-value-row">
              <span class="address-text" id="ip-val">{ip}</span>
              <button type="button" class="copy-btn" onclick="copyValue('{ip}', this)">
                <svg class="copy-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="9" y="9" width="13" height="13" rx="2" ry="2"></rect><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"></path></svg>
                <span>Copy</span>
              </button>
            </div>
          </div>
          <div class="address-card">
            <div class="address-label">PORT</div>
            <div class="address-value-row">
              <span class="address-text" id="port-val">{port}</span>
              <button type="button" class="copy-btn" onclick="copyValue('{port}', this)">
                <svg class="copy-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="9" y="9" width="13" height="13" rx="2" ry="2"></rect><path d="M5 15H4a2 2 0 0 1-2-2v-4"></path></svg>
                <span>Copy</span>
              </button>
            </div>
          </div>
          <div class="address-card">
            <div class="address-label">SERVER PASSWORD</div>
            <div class="address-value-row">
              <span class="address-text" id="pwd-val" style="color:#fbbf24;">1488</span>
              <button type="button" class="copy-btn" onclick="copyValue('1488', this)">
                <svg class="copy-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="9" y="9" width="13" height="13" rx="2" ry="2"></rect><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"></path></svg>
                <span>Copy</span>
              </button>
            </div>
          </div>
        </div>
        """
    elif is_booting:
        address_widget_html = """
        <div class="status-banner booting-banner">
          <div class="status-banner-icon">🚀</div>
          <div class="status-banner-text">
            <div class="status-banner-title">Vsrania is Starting Up</div>
            <div class="status-banner-desc">Initializing instance on Akash. IP and Port will appear here automatically when ready.</div>
          </div>
        </div>
        """
    else:
        address_widget_html = """
        <div class="status-banner offline-banner">
          <div class="status-banner-icon">⏸️</div>
          <div class="status-banner-text">
            <div class="status-banner-title">Vsrania is Offline</div>
            <div class="status-banner-desc">Server is stopped. You can download mods below in preparation for the session.</div>
          </div>
        </div>
        """

    client_info = manifest.get("client", {})
    common_info = manifest.get("common", {})
    server_pkg_info = manifest.get("server", {})

    client_stats_str = format_pkg_stats(client_info)
    common_stats_str = format_pkg_stats(common_info)
    server_stats_str = format_pkg_stats(server_pkg_info)

    readme_content_html = get_readme_html()

    return f"""<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>Vsrania • Server Hub</title>
  <style>
    :root {{
      --bg: #07090e;
      --card-bg: rgba(15, 23, 42, 0.75);
      --card-border: rgba(255, 255, 255, 0.08);
      --text: #f8fafc;
      --text-muted: #94a3b8;
      --primary: #3b82f6;
      --primary-hover: #2563eb;
      --primary-glow: rgba(59, 130, 246, 0.3);
      --accent: #10b981;
      --accent-glow: rgba(16, 185, 129, 0.3);
      --purple: #8b5cf6;
      --purple-glow: rgba(139, 92, 246, 0.3);
    }}
    * {{ box-sizing: border-box; }}
    body {{
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
      background: radial-gradient(circle at 50% 0%, #1e1b4b 0%, #0b0f19 50%, #030712 100%);
      color: var(--text);
      margin: 0;
      padding: 2rem 1rem 4rem;
      min-height: 100vh;
      display: flex;
      justify-content: center;
    }}
    .container {{
      max-width: 820px;
      width: 100%;
    }}

    /* Top Nav */
    .nav-bar {{
      display: flex;
      justify-content: space-between;
      align-items: center;
      margin-bottom: 2rem;
      flex-wrap: wrap;
      gap: 1rem;
    }}
    .brand-title {{
      display: flex;
      align-items: center;
      gap: 0.75rem;
      font-size: 1.35rem;
      font-weight: 700;
      letter-spacing: -0.02em;
    }}
    .brand-icon {{
      font-size: 1.6rem;
    }}
    .nav-links {{
      display: flex;
      gap: 0.5rem;
    }}
    .nav-item {{
      color: var(--text-muted);
      text-decoration: none;
      font-weight: 500;
      font-size: 0.9rem;
      padding: 0.5rem 1rem;
      border-radius: 8px;
      border: 1px solid transparent;
      transition: all 0.15s;
    }}
    .nav-item.active, .nav-item:hover {{
      color: white;
      background: rgba(255, 255, 255, 0.05);
      border-color: var(--card-border);
    }}

    /* Header & Status */
    .header-card {{
      background: var(--card-bg);
      backdrop-filter: blur(16px);
      border: 1px solid var(--card-border);
      border-radius: 16px;
      padding: 1.75rem;
      margin-bottom: 1.5rem;
      box-shadow: 0 20px 25px -5px rgba(0,0,0,0.5);
    }}
    .header-top {{
      display: flex;
      justify-content: space-between;
      align-items: center;
      flex-wrap: wrap;
      gap: 1rem;
      margin-bottom: 1.25rem;
    }}
    .server-title-block h1 {{
      font-size: 1.5rem;
      margin: 0 0 0.25rem 0;
      letter-spacing: -0.02em;
    }}
    .server-title-block p {{
      margin: 0;
      font-size: 0.85rem;
      color: var(--text-muted);
    }}
    .status-badge {{
      display: inline-flex;
      align-items: center;
      gap: 0.5rem;
      padding: 0.4rem 0.9rem;
      border-radius: 9999px;
      font-size: 0.8rem;
      font-weight: 700;
      letter-spacing: 0.05em;
      border: 1px solid transparent;
    }}
    .badge-online {{
      background: rgba(16, 185, 129, 0.15);
      color: #34d399;
      border-color: rgba(16, 185, 129, 0.3);
      box-shadow: 0 0 15px rgba(16, 185, 129, 0.2);
    }}
    .badge-booting {{
      background: rgba(245, 158, 11, 0.15);
      color: #fbbf24;
      border-color: rgba(245, 158, 11, 0.3);
      box-shadow: 0 0 15px rgba(245, 158, 11, 0.2);
    }}
    .badge-offline {{
      background: rgba(239, 68, 68, 0.15);
      color: #f87171;
      border-color: rgba(239, 68, 68, 0.3);
    }}
    .status-dot {{
      width: 8px;
      height: 8px;
      border-radius: 50%;
    }}
    .status-dot.online {{
      background: #10b981;
      box-shadow: 0 0 8px #10b981;
    }}
    .status-dot.booting {{
      background: #f59e0b;
      animation: pulse 1.5s infinite;
    }}
    .status-dot.offline {{
      background: #ef4444;
    }}
    @keyframes pulse {{
      0%, 100% {{ opacity: 1; transform: scale(1); }}
      50% {{ opacity: 0.4; transform: scale(0.85); }}
    }}

    /* Address Grid (IP, Port, Password) */
    .address-grid {{
      display: grid;
      grid-template-columns: 1.8fr 1fr 1.3fr;
      gap: 0.75rem;
    }}
    @media (max-width: 650px) {{
      .address-grid {{ grid-template-columns: 1fr; }}
    }}
    .address-card {{
      background: rgba(0, 0, 0, 0.45);
      border: 1px solid rgba(255, 255, 255, 0.08);
      border-radius: 12px;
      padding: 0.9rem 1.25rem;
    }}
    .address-label {{
      font-size: 0.7rem;
      font-weight: 700;
      color: var(--text-muted);
      letter-spacing: 0.08em;
      margin-bottom: 0.35rem;
    }}
    .address-value-row {{
      display: flex;
      justify-content: space-between;
      align-items: center;
      gap: 0.75rem;
    }}
    .address-text {{
      font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
      font-size: 1.15rem;
      font-weight: 700;
      color: #38bdf8;
      letter-spacing: 0.02em;
    }}
    .copy-btn {{
      background: rgba(255, 255, 255, 0.08);
      color: var(--text);
      border: 1px solid rgba(255, 255, 255, 0.12);
      border-radius: 6px;
      padding: 0.35rem 0.75rem;
      font-size: 0.75rem;
      font-weight: 600;
      cursor: pointer;
      display: inline-flex;
      align-items: center;
      gap: 0.35rem;
      transition: all 0.15s;
    }}
    .copy-btn:hover {{
      background: rgba(255, 255, 255, 0.15);
      border-color: rgba(255, 255, 255, 0.25);
    }}
    .copy-icon {{
      width: 13px;
      height: 13px;
    }}

    /* Status Banners */
    .status-banner {{
      display: flex;
      align-items: center;
      gap: 1.25rem;
      padding: 1rem 1.25rem;
      border-radius: 12px;
    }}
    .booting-banner {{
      background: rgba(245, 158, 11, 0.08);
      border: 1px solid rgba(245, 158, 11, 0.2);
    }}
    .offline-banner {{
      background: rgba(239, 68, 68, 0.06);
      border: 1px solid rgba(239, 68, 68, 0.15);
    }}
    .status-banner-icon {{
      font-size: 2rem;
    }}
    .status-banner-title {{
      font-weight: 700;
      font-size: 0.95rem;
      margin-bottom: 0.2rem;
    }}
    .status-banner-desc {{
      font-size: 0.8rem;
      color: var(--text-muted);
      line-height: 1.4;
    }}

    /* Torrent Card Banner */
    .torrent-card {{
      background: linear-gradient(135deg, rgba(139, 92, 246, 0.15) 0%, rgba(59, 130, 246, 0.1) 100%);
      border: 1px solid rgba(139, 92, 246, 0.3);
      border-radius: 16px;
      padding: 1.5rem 1.75rem;
      margin-bottom: 1.5rem;
      display: flex;
      justify-content: space-between;
      align-items: center;
      flex-wrap: wrap;
      gap: 1.25rem;
      box-shadow: 0 10px 25px -5px var(--purple-glow);
    }}
    .torrent-info {{
      max-width: 500px;
    }}
    .torrent-badge {{
      display: inline-block;
      background: rgba(139, 92, 246, 0.25);
      color: #c4b5fd;
      font-size: 0.7rem;
      font-weight: 700;
      padding: 0.2rem 0.55rem;
      border-radius: 4px;
      letter-spacing: 0.06em;
      margin-bottom: 0.4rem;
    }}
    .torrent-title {{
      font-size: 1.15rem;
      font-weight: 700;
      color: white;
      margin-bottom: 0.25rem;
    }}
    .torrent-desc {{
      font-size: 0.82rem;
      color: var(--text-muted);
      line-height: 1.45;
    }}
    .torrent-btn {{
      background: linear-gradient(135deg, #8b5cf6 0%, #6d28d9 100%);
      color: white;
      text-decoration: none;
      padding: 0.75rem 1.35rem;
      border-radius: 10px;
      font-weight: 600;
      font-size: 0.9rem;
      display: inline-flex;
      align-items: center;
      gap: 0.5rem;
      box-shadow: 0 4px 15px rgba(139, 92, 246, 0.4);
      transition: all 0.15s;
    }}
    .torrent-btn:hover {{
      transform: translateY(-2px);
      box-shadow: 0 6px 20px rgba(139, 92, 246, 0.55);
    }}

    /* Action Cards Grid */
    .cards-grid {{
      display: grid;
      grid-template-columns: repeat(3, 1fr);
      gap: 1.25rem;
      margin-bottom: 1.5rem;
    }}
    @media (max-width: 768px) {{
      .cards-grid {{ grid-template-columns: 1fr; }}
    }}
    .action-card {{
      background: var(--card-bg);
      backdrop-filter: blur(16px);
      border: 1px solid var(--card-border);
      border-radius: 16px;
      padding: 1.5rem;
      display: flex;
      flex-direction: column;
      justify-content: space-between;
      position: relative;
      overflow: hidden;
      transition: transform 0.2s, box-shadow 0.2s;
    }}
    .action-card:hover {{
      transform: translateY(-3px);
    }}
    .card-client {{
      border-top: 3px solid var(--primary);
    }}
    .card-client:hover {{
      box-shadow: 0 10px 25px -5px var(--primary-glow);
    }}
    .card-common {{
      border-top: 3px solid var(--accent);
    }}
    .card-common:hover {{
      box-shadow: 0 10px 25px -5px var(--accent-glow);
    }}
    .card-server {{
      border-top: 3px solid #64748b;
      opacity: 0.65;
    }}
    .card-server.unlocked {{
      opacity: 1;
      border-top-color: #f59e0b;
      box-shadow: 0 10px 25px -5px rgba(245, 158, 11, 0.25);
    }}
    .card-header {{
      display: flex;
      align-items: center;
      gap: 0.75rem;
      margin-bottom: 0.75rem;
    }}
    .card-icon-box {{
      width: 40px;
      height: 40px;
      border-radius: 10px;
      display: flex;
      align-items: center;
      justify-content: center;
      font-size: 1.25rem;
    }}
    .client-icon-box {{
      background: rgba(59, 130, 246, 0.15);
    }}
    .common-icon-box {{
      background: rgba(16, 185, 129, 0.15);
    }}
    .server-icon-box {{
      background: rgba(100, 116, 139, 0.2);
    }}
    .card-title {{
      font-size: 1.1rem;
      font-weight: 700;
      margin: 0;
    }}
    .card-stats {{
      font-size: 0.8rem;
      color: var(--text-muted);
      margin-bottom: 1.25rem;
      line-height: 1.4;
    }}
    .card-btn {{
      display: flex;
      align-items: center;
      justify-content: center;
      gap: 0.5rem;
      width: 100%;
      padding: 0.7rem;
      border-radius: 10px;
      font-weight: 600;
      font-size: 0.875rem;
      text-decoration: none;
      cursor: pointer;
      border: none;
      transition: all 0.15s;
    }}
    .btn-client {{
      background: var(--primary);
      color: white;
    }}
    .btn-client:hover {{
      background: var(--primary-hover);
    }}
    .btn-common {{
      background: var(--accent);
      color: #022c22;
      font-weight: 700;
    }}
    .btn-common:hover {{
      background: #059669;
      color: white;
    }}
    .btn-locked {{
      background: rgba(255, 255, 255, 0.08);
      color: var(--text-muted);
      border: 1px solid var(--card-border);
    }}
    .btn-locked:hover {{
      background: rgba(255, 255, 255, 0.15);
      color: white;
    }}
    .btn-unlocked {{
      background: #f59e0b;
      color: #451a03;
      font-weight: 700;
    }}

    /* Backups Section Button (Bottom Faded) */
    .backups-footer {{
      display: flex;
      justify-content: center;
      margin-bottom: 2rem;
    }}
    .backups-faded-btn {{
      display: inline-flex;
      align-items: center;
      gap: 0.6rem;
      background: rgba(15, 23, 42, 0.6);
      border: 1px solid rgba(255, 255, 255, 0.08);
      color: var(--text-muted);
      padding: 0.6rem 1.25rem;
      border-radius: 12px;
      font-size: 0.85rem;
      font-weight: 600;
      text-decoration: none;
      cursor: pointer;
      transition: all 0.15s;
    }}
    .backups-faded-btn:hover {{
      background: rgba(255, 255, 255, 0.08);
      color: white;
      border-color: rgba(255, 255, 255, 0.18);
    }}

    /* Readme Plate */
    .readme-card {{
      background: var(--card-bg);
      backdrop-filter: blur(16px);
      border: 1px solid var(--card-border);
      border-radius: 16px;
      padding: 1.75rem;
      margin-bottom: 1.5rem;
    }}
    .readme-header {{
      display: flex;
      align-items: center;
      gap: 0.6rem;
      border-bottom: 1px solid var(--card-border);
      padding-bottom: 0.85rem;
      margin-bottom: 1.25rem;
    }}
    .readme-header h2 {{
      font-size: 1.2rem;
      margin: 0;
      font-weight: 700;
    }}
    .readme-body {{
      font-size: 0.88rem;
      color: #cbd5e1;
      line-height: 1.6;
    }}
    .readme-body h1 {{ font-size: 1.3rem; margin: 1.25rem 0 0.5rem 0; color: white; }}
    .readme-body h2 {{ font-size: 1.15rem; margin: 1.1rem 0 0.4rem 0; color: white; }}
    .readme-body h3 {{ font-size: 1rem; margin: 1rem 0 0.35rem 0; color: #38bdf8; }}
    .readme-body p {{ margin: 0.5rem 0; }}
    .readme-body ul, .readme-body ol {{ padding-left: 1.35rem; margin: 0.5rem 0 1rem 0; }}
    .readme-body li {{ margin-bottom: 0.35rem; }}
    .readme-body code {{
      background: #020617;
      color: #38bdf8;
      padding: 0.15rem 0.4rem;
      border-radius: 4px;
      font-family: monospace;
      font-size: 0.85em;
    }}
    .readme-body pre {{
      background: #020617;
      border: 1px solid var(--card-border);
      padding: 0.9rem;
      border-radius: 8px;
      overflow-x: auto;
      margin: 0.75rem 0;
    }}
    .readme-body blockquote {{
      border-left: 3px solid var(--primary);
      margin: 0.75rem 0;
      padding: 0.4rem 0 0.4rem 1rem;
      color: var(--text-muted);
      background: rgba(59, 130, 246, 0.05);
      border-radius: 0 8px 8px 0;
    }}
    .readme-body hr {{
      border: none;
      border-top: 1px solid var(--card-border);
      margin: 1.5rem 0;
    }}

    /* Password Modal */
    .modal-backdrop {{
      display: none;
      position: fixed;
      inset: 0;
      background: rgba(0, 0, 0, 0.75);
      backdrop-filter: blur(8px);
      z-index: 999;
      justify-content: center;
      align-items: center;
      padding: 1rem;
    }}
    .modal-box {{
      background: #0f172a;
      border: 1px solid var(--card-border);
      border-radius: 16px;
      padding: 2rem;
      max-width: 400px;
      width: 100%;
      box-shadow: 0 25px 50px -12px rgba(0, 0, 0, 0.8);
    }}
    .modal-box h3 {{
      margin: 0 0 0.5rem 0;
      font-size: 1.25rem;
    }}
    .modal-box p {{
      color: var(--text-muted);
      font-size: 0.85rem;
      margin-bottom: 1.25rem;
    }}
    .modal-input {{
      width: 100%;
      background: #020617;
      border: 1px solid var(--card-border);
      color: white;
      padding: 0.75rem 1rem;
      border-radius: 8px;
      font-size: 0.95rem;
      margin-bottom: 1.25rem;
    }}
    .modal-error {{
      display: none;
      background: rgba(239, 68, 68, 0.15);
      border: 1px solid rgba(239, 68, 68, 0.35);
      color: #fca5a5;
      padding: 0.6rem 0.85rem;
      border-radius: 8px;
      font-size: 0.8rem;
      font-weight: 500;
      margin-bottom: 1rem;
      align-items: center;
      gap: 0.5rem;
    }}
    @keyframes shake {{
      0%, 100% {{ transform: translateX(0); }}
      20%, 60% {{ transform: translateX(-6px); }}
      40%, 80% {{ transform: translateX(6px); }}
    }}
    .shake {{
      animation: shake 0.35s ease-in-out;
      border-color: #ef4444 !important;
    }}
    .modal-actions {{
      display: flex;
      justify-content: flex-end;
      gap: 0.75rem;
    }}
    .modal-btn {{
      padding: 0.6rem 1.15rem;
      border-radius: 8px;
      font-weight: 600;
      font-size: 0.85rem;
      cursor: pointer;
      border: none;
      transition: all 0.15s ease;
    }}
    .modal-btn:disabled {{
      opacity: 0.6;
      cursor: not-allowed;
    }}
    .modal-btn-cancel {{
      background: transparent;
      color: var(--text-muted);
    }}
    .modal-btn-submit {{
      background: var(--primary);
      color: white;
    }}
  </style>
</head>
<body>
  <div class="container">
    
    <!-- Navigation Bar -->
    <nav class="nav-bar">
      <div class="brand-title">
        <span class="brand-icon">☣️</span>
        <span>Vsrania • Hub</span>
      </div>
      <div class="nav-links">
        <a href="/" class="nav-item active">Packages</a>
        <a href="javascript:void(0)" onclick="openBackups()" class="nav-item">Backups 🔒</a>
      </div>
    </nav>

    <!-- Header & Status Card -->
    <div class="header-card">
      <div class="header-top">
        <div class="server-title-block">
          <h1>Vsrania Dedicated Server</h1>
          <p>Managed via Akash Network & Controller</p>
        </div>
        <div class="status-badge {badge_class}" id="status-badge-container">
          {dot_html}
          <span id="status-badge-text">{badge_text}</span>
        </div>
      </div>

      <!-- Address / Status Widget -->
      <div id="address-widget-container">
        {address_widget_html}
      </div>
    </div>

    <!-- Clean Torrent Client Download Banner -->
    <div class="torrent-card">
      <div class="torrent-info">
        <div class="torrent-title">🎮 Clean Game Client (.torrent)</div>
        <div class="torrent-desc">Pre-tested client matching server version. Recommended to avoid mod errors.</div>
      </div>
      <a href="/game.torrent" class="torrent-btn" download>
        <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"></path><polyline points="7 10 12 15 17 10"></polyline><line x1="12" y1="15" x2="12" y2="3"></line></svg>
        <span>Download .torrent</span>
      </a>
    </div>

    <!-- 3 Action Cards (Client, Common, Server) -->
    <div class="cards-grid">
      
      <!-- Client Package -->
      <div class="action-card card-client">
        <div>
          <div class="card-header">
            <div class="card-icon-box client-icon-box">🎮</div>
            <h3 class="card-title">Client Files</h3>
          </div>
          <div class="card-stats">{client_stats_str}</div>
        </div>
        <a href="/client.zip" class="card-btn btn-client" download>
          <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"></path><polyline points="7 10 12 15 17 10"></polyline><line x1="12" y1="15" x2="12" y2="3"></line></svg>
          <span>Download client.zip</span>
        </a>
      </div>

      <!-- Common Package -->
      <div class="action-card card-common">
        <div>
          <div class="card-header">
            <div class="card-icon-box common-icon-box">📦</div>
            <h3 class="card-title">Common Files</h3>
          </div>
          <div class="card-stats">{common_stats_str}</div>
        </div>
        <a href="/common.zip" class="card-btn btn-common" download>
          <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"></path><polyline points="7 10 12 15 17 10"></polyline><line x1="12" y1="15" x2="12" y2="3"></line></svg>
          <span>Download common.zip</span>
        </a>
      </div>

      <!-- Server Package (Protected / Locked) -->
      <div class="action-card card-server" id="server-card">
        <div>
          <div class="card-header">
            <div class="card-icon-box server-icon-box" id="server-icon-box">🔒</div>
            <h3 class="card-title">Server Files</h3>
          </div>
          <div class="card-stats" id="server-stats">{server_stats_str}</div>
        </div>
        <button type="button" class="card-btn btn-locked" id="server-btn" onclick="handleServerDownload()">
          <span id="server-btn-icon">🔒</span>
          <span id="server-btn-text">Unlock server.zip</span>
        </button>
      </div>

    </div>

    <!-- Faded Backups Button at Bottom -->
    <div class="backups-footer">
      <button type="button" class="backups-faded-btn" onclick="openBackups()">
        <span>🗄️</span>
        <span>Backups</span>
        <span>🔒</span>
      </button>
    </div>

    <!-- Dynamic Readme Section -->
    <div class="readme-card">
      <div class="readme-header">
        <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="#38bdf8" stroke-width="2"><path d="M4 19.5A2.5 2.5 0 0 1 6.5 17H20"></path><path d="M6.5 2H20v20H6.5A2.5 2.5 0 0 1 4 19.5v-15A2.5 2.5 0 0 1 6.5 2z"></path></svg>
        <h2>Quick Guide</h2>
      </div>
      <div class="readme-body">
        {readme_content_html}
      </div>
    </div>

  </div>

  <!-- Password Unlock Modal -->
  <div class="modal-backdrop" id="auth-modal" onclick="if(event.target===this) closeModal()">
    <div class="modal-box">
      <h3 id="auth-modal-title">🔒 Authorization Required</h3>
      <p id="auth-modal-desc">Enter password to continue:</p>
      <div class="modal-error" id="auth-error-msg"></div>
      <input type="password" class="modal-input" id="auth-password-input" placeholder="Password..." onkeydown="if(event.key==='Enter') submitPassword()" />
      <div class="modal-actions">
        <button type="button" class="modal-btn modal-btn-cancel" onclick="closeModal()">Cancel</button>
        <button type="button" class="modal-btn modal-btn-submit" id="auth-submit-btn" onclick="submitPassword()">Unlock</button>
      </div>
    </div>
  </div>

  <script>
    let savedServerToken = sessionStorage.getItem("pz_server_files_token") || "{server_token}";
    let savedBackupToken = sessionStorage.getItem("pz_backups_token") || "{backup_token}";
    let pendingAction = null;

    // Verify and apply saved server files token asynchronously
    if (savedServerToken) {{
      fetch(`/api/verify?type=server_files&token=${{encodeURIComponent(savedServerToken)}}`)
        .then(r => r.json())
        .then(data => {{
          if (data && data.ok) {{
            applyUnlockedServerUI();
          }} else {{
            savedServerToken = "";
            sessionStorage.removeItem("pz_server_files_token");
          }}
        }})
        .catch(() => {{}});
    }}

    function copyValue(text, btn) {{
      if (!text) return;
      navigator.clipboard.writeText(text).then(() => {{
        const origHtml = btn.innerHTML;
        btn.innerHTML = '<span>Copied! ✨</span>';
        setTimeout(() => btn.innerHTML = origHtml, 1500);
      }});
    }}

    function openModal(action) {{
      pendingAction = action;
      const modal = document.getElementById("auth-modal");
      const title = document.getElementById("auth-modal-title");
      const desc = document.getElementById("auth-modal-desc");
      const input = document.getElementById("auth-password-input");
      const errorBox = document.getElementById("auth-error-msg");
      const submitBtn = document.getElementById("auth-submit-btn");

      errorBox.style.display = "none";
      errorBox.innerHTML = "";
      input.classList.remove("shake");
      submitBtn.disabled = false;
      submitBtn.innerText = "Unlock";

      if (action === "server_download") {{
        title.innerText = "🔒 Unlock Server Files";
        desc.innerText = "Enter password to download server.zip (server configs & mods):";
        input.placeholder = "Server files password...";
      }} else if (action === "backups") {{
        title.innerText = "🔒 Unlock Backups";
        desc.innerText = "Enter password to access world save archives:";
        input.placeholder = "Backups password...";
      }}

      modal.style.display = "flex";
      input.value = "";
      input.focus();
    }}

    function closeModal() {{
      document.getElementById("auth-modal").style.display = "none";
      pendingAction = null;
    }}

    async function submitPassword() {{
      const input = document.getElementById("auth-password-input");
      const errorBox = document.getElementById("auth-error-msg");
      const submitBtn = document.getElementById("auth-submit-btn");
      const val = input.value.trim();

      errorBox.style.display = "none";
      input.classList.remove("shake");

      if (!val) {{
        showModalError("Please enter a password.");
        return;
      }}

      const authType = (pendingAction === "server_download") ? "server_files" : "backups";
      
      submitBtn.disabled = true;
      submitBtn.innerText = "Verifying...";

      try {{
        const res = await fetch(`/api/verify?type=${{authType}}&token=${{encodeURIComponent(val)}}`);
        const data = await res.json().catch(() => ({{}}));

        if (res.ok && data.ok) {{
          if (pendingAction === "server_download") {{
            savedServerToken = val;
            sessionStorage.setItem("pz_server_files_token", val);
            applyUnlockedServerUI();
            closeModal();
            
            // Trigger file download cleanly without page reload
            const a = document.createElement("a");
            a.href = `/server.zip?token=${{encodeURIComponent(val)}}`;
            a.download = "server.zip";
            document.body.appendChild(a);
            a.click();
            a.remove();
          }} else if (pendingAction === "backups") {{
            savedBackupToken = val;
            sessionStorage.setItem("pz_backups_token", val);
            closeModal();
            window.location.href = `/backups?token=${{encodeURIComponent(val)}}`;
          }}
        }} else {{
          showModalError(data.error || "Incorrect password. Access denied.");
          input.classList.add("shake");
          input.select();
        }}
      }} catch (err) {{
        showModalError("Network error while verifying password.");
      }} finally {{
        submitBtn.disabled = false;
        submitBtn.innerText = "Unlock";
      }}
    }}

    function showModalError(msg) {{
      const errorBox = document.getElementById("auth-error-msg");
      errorBox.innerHTML = `<span>⚠️ ${{msg}}</span>`;
      errorBox.style.display = "flex";
    }}

    function applyUnlockedServerUI() {{
      const card = document.getElementById("server-card");
      const iconBox = document.getElementById("server-icon-box");
      const btn = document.getElementById("server-btn");
      const btnText = document.getElementById("server-btn-text");
      const btnIcon = document.getElementById("server-btn-icon");

      if (card) card.classList.add("unlocked");
      if (iconBox) iconBox.innerHTML = "🔓";
      if (btn) {{
        btn.className = "card-btn btn-unlocked";
        btnText.innerText = "Download server.zip";
        btnIcon.innerText = "⬇️";
      }}
    }}

    function handleServerDownload() {{
      if (savedServerToken) {{
        const a = document.createElement("a");
        a.href = `/server.zip?token=${{encodeURIComponent(savedServerToken)}}`;
        a.download = "server.zip";
        document.body.appendChild(a);
        a.click();
        a.remove();
      }} else {{
        openModal("server_download");
      }}
    }}

    function openBackups() {{
      if (savedBackupToken) {{
        window.location.href = `/backups?token=${{encodeURIComponent(savedBackupToken)}}`;
      }} else {{
        openModal("backups");
      }}
    }}

    // Live Server Status Polling (Auto-refreshes every 4s without full page reload)
    let lastKnownStatus = "{status}";
    let lastKnownIp = "{ip}";

    async function pollServerStatus() {{
      try {{
        const res = await fetch("/server_info.json");
        if (!res.ok) return;
        const info = await res.json();
        const st = (info.status || "stopped").toLowerCase();
        const rawIp = info.raw_ip || info.ip || "";
        const ip = (st === "online") ? rawIp : "";
        const port = info.port || 16261;

        if (st !== lastKnownStatus || ip !== lastKnownIp) {{
          lastKnownStatus = st;
          lastKnownIp = ip;
          updateStatusUI(st, ip, port);
        }}
      }} catch (e) {{}}
    }}

    function updateStatusUI(st, ip, port) {{
      const badge = document.getElementById("status-badge-container");
      const widget = document.getElementById("address-widget-container");
      if (!badge || !widget) return;

      const isOnline = (st === "online" && ip && ip !== "pending");
      const isBooting = (st === "booting");

      badge.className = "status-badge " + (isOnline ? "badge-online" : isBooting ? "badge-booting" : "badge-offline");
      badge.innerHTML = (isOnline ? '<span class="status-dot online"></span><span id="status-badge-text">ONLINE</span>' :
                        isBooting ? '<span class="status-dot booting"></span><span id="status-badge-text">STARTING UP</span>' :
                        '<span class="status-dot offline"></span><span id="status-badge-text">OFFLINE</span>');

      if (isOnline) {{
        widget.innerHTML = `
          <div class="address-grid">
            <div class="address-card">
              <div class="address-label">SERVER IP</div>
              <div class="address-value-row">
                <span class="address-text" id="ip-val">${{ip}}</span>
                <button type="button" class="copy-btn" onclick="copyValue('${{ip}}', this)">
                  <svg class="copy-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="9" y="9" width="13" height="13" rx="2" ry="2"></rect><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"></path></svg>
                  <span>Copy</span>
                </button>
              </div>
            </div>
            <div class="address-card">
              <div class="address-label">PORT</div>
              <div class="address-value-row">
                <span class="address-text" id="port-val">${{port}}</span>
                <button type="button" class="copy-btn" onclick="copyValue('${{port}}', this)">
                  <svg class="copy-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="9" y="9" width="13" height="13" rx="2" ry="2"></rect><path d="M5 15H4a2 2 0 0 1-2-2v-4"></path></svg>
                  <span>Copy</span>
                </button>
              </div>
            </div>
            <div class="address-card">
              <div class="address-label">SERVER PASSWORD</div>
              <div class="address-value-row">
                <span class="address-text" id="pwd-val" style="color:#fbbf24;">1488</span>
                <button type="button" class="copy-btn" onclick="copyValue('1488', this)">
                  <svg class="copy-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="9" y="9" width="13" height="13" rx="2" ry="2"></rect><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"></path></svg>
                  <span>Copy</span>
                </button>
              </div>
            </div>
          </div>
        `;
      }} else if (isBooting) {{
        widget.innerHTML = `
          <div class="status-banner booting-banner">
            <div class="status-banner-icon">🚀</div>
            <div class="status-banner-content">
              <div class="status-banner-title">Server is booting up...</div>
              <div class="status-banner-desc">Starting dedicated server, allocating resources and syncing mods. IP & connection details will appear here as soon as the server is ready.</div>
            </div>
          </div>
        `;
      }} else {{
        widget.innerHTML = `
          <div class="status-banner offline-banner">
            <div class="status-banner-icon">💤</div>
            <div class="status-banner-content">
              <div class="status-banner-title">Server is currently offline</div>
              <div class="status-banner-desc">The Project Zomboid dedicated server is stopped. Push a <code>start</code> trigger to spin up the server on Akash.</div>
            </div>
          </div>
        `;
      }}
    }}

    setInterval(pollServerStatus, 4000);
  </script>
</body>
</html>
"""


def render_html_backups(server_info: dict, backups: list, is_authed: bool, token: str = "") -> str:
    status = server_info.get("status", "stopped").lower()
    ip = server_info.get("raw_ip", "") if status == "online" else ""
    port = server_info.get("port", 16261)
    is_online = (status == "online" and bool(ip) and ip != "pending")

    rows = []
    for b in backups:
        rows.append(f"""
        <tr>
          <td style="font-weight:600; font-family:monospace; color:#38bdf8;">{b['name']}</td>
          <td style="color:var(--text-muted); font-size:0.85rem;">{b['date_str']}</td>
          <td style="color:var(--text-muted); font-size:0.85rem;">{b['size_str']}</td>
          <td style="text-align:right;">
            <a href="/backups/{b['name']}{f'?token={token}' if token else ''}" class="copy-btn" download>
              ⬇️ Download
            </a>
          </td>
        </tr>
        """)

    wrong_pwd_html = '<div style="background:rgba(239,68,68,0.15); border:1px solid rgba(239,68,68,0.35); color:#fca5a5; padding:0.6rem 1rem; border-radius:8px; font-size:0.85rem; margin:0 auto 1.25rem; max-width:320px; font-weight:500;">❌ Incorrect backups password. Access denied.</div>' if (token and not is_authed) else ''

    table_html = f"""
    <table style="width:100%; border-collapse:collapse; margin-top:1rem;">
      <thead>
        <tr style="border-bottom:1px solid rgba(255,255,255,0.1); text-align:left; font-size:0.75rem; color:var(--text-muted); letter-spacing:0.05em;">
          <th style="padding:0.75rem 0.5rem;">ARCHIVE NAME</th>
          <th style="padding:0.75rem 0.5rem;">CREATION DATE</th>
          <th style="padding:0.75rem 0.5rem;">SIZE</th>
          <th style="padding:0.75rem 0.5rem; text-align:right;">ACTION</th>
        </tr>
      </thead>
      <tbody>
        {''.join(rows) if rows else '<tr><td colspan="4" style="text-align:center; padding:2rem; color:var(--text-muted);">No backup archives found in /data/backups/</td></tr>'}
      </tbody>
    </table>
    """ if is_authed else f"""
    <div style="text-align:center; padding:3rem 1rem; background:rgba(0,0,0,0.3); border-radius:12px; margin-top:1rem;">
      <div style="font-size:2.5rem; margin-bottom:0.75rem;">🔒</div>
      <h3 style="margin:0 0 0.5rem 0;">Backups Password Required</h3>
      <p style="color:var(--text-muted); font-size:0.85rem; margin-bottom:1.25rem;">Server backups and world archives are protected.</p>
      {wrong_pwd_html}
      <form action="/backups" method="GET" style="display:inline-flex; gap:0.5rem;" onsubmit="saveBackupToken(this)">
        <input type="password" name="token" id="backup-pwd-input" placeholder="Backups Password..." style="background:#020617; border:1px solid rgba(255,255,255,0.1); color:white; padding:0.6rem 1rem; border-radius:8px; font-size:0.9rem;" required />
        <button type="submit" class="copy-btn" style="background:#3b82f6; border:none; padding:0.6rem 1.25rem;">Unlock</button>
      </form>
    </div>
    """

    return f"""<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>Vsrania • Backups</title>
  <style>
    :root {{
      --bg: #07090e;
      --card-bg: rgba(15, 23, 42, 0.75);
      --card-border: rgba(255, 255, 255, 0.08);
      --text: #f8fafc;
      --text-muted: #94a3b8;
      --primary: #3b82f6;
    }}
    * {{ box-sizing: border-box; }}
    body {{
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
      background: radial-gradient(circle at 50% 0%, #1e1b4b 0%, #0b0f19 50%, #030712 100%);
      color: var(--text);
      margin: 0;
      padding: 2rem 1rem 4rem;
      min-height: 100vh;
      display: flex;
      justify-content: center;
    }}
    .container {{
      max-width: 820px;
      width: 100%;
    }}
    .nav-bar {{
      display: flex;
      justify-content: space-between;
      align-items: center;
      margin-bottom: 2rem;
    }}
    .brand-title {{
      font-size: 1.35rem;
      font-weight: 700;
    }}
    .nav-item {{
      color: var(--text-muted);
      text-decoration: none;
      font-weight: 500;
      font-size: 0.9rem;
      padding: 0.5rem 1rem;
      border-radius: 8px;
    }}
    .nav-item:hover, .nav-item.active {{
      color: white;
      background: rgba(255, 255, 255, 0.05);
    }}
    .card {{
      background: var(--card-bg);
      backdrop-filter: blur(16px);
      border: 1px solid var(--card-border);
      border-radius: 16px;
      padding: 1.75rem;
      margin-bottom: 1.5rem;
    }}
    .copy-btn {{
      background: rgba(255, 255, 255, 0.08);
      color: var(--text);
      border: 1px solid rgba(255, 255, 255, 0.12);
      border-radius: 6px;
      padding: 0.4rem 0.85rem;
      font-size: 0.8rem;
      font-weight: 600;
      cursor: pointer;
      text-decoration: none;
      display: inline-flex;
      align-items: center;
      gap: 0.35rem;
    }}
    .copy-btn:hover {{
      background: rgba(255, 255, 255, 0.15);
    }}
  </style>
</head>
<body>
  <div class="container">
    <nav class="nav-bar">
      <div class="brand-title">☣️ Vsrania • Hub</div>
      <div>
        <a href="/" class="nav-item">Packages</a>
        <a href="/backups{f'?token={token}' if token else ''}" class="nav-item active">Backups 🔒</a>
      </div>
    </nav>

    <div class="card">
      <div style="display:flex; justify-content:space-between; align-items:center; flex-wrap:wrap; gap:1rem; margin-bottom:1rem;">
        <div>
          <h1 style="font-size:1.4rem; margin:0 0 0.25rem 0;">🗄️ World Save Backups</h1>
          <p style="color:var(--text-muted); font-size:0.85rem; margin:0;">Automated and manual snapshots stored on the Controller</p>
        </div>
        <span style="background:rgba(255,255,255,0.05); padding:0.35rem 0.75rem; border-radius:8px; font-size:0.8rem; color:var(--text-muted);">
          {len(backups)} archive(s)
        </span>
      </div>
      {table_html}
    </div>

    {f'''
    <div class="card">
      <h2 style="font-size:1.15rem; margin:0 0 0.5rem 0;">⬆️ Upload Backup Archive</h2>
      <p style="color:var(--text-muted); font-size:0.85rem;">Upload an existing world save <code>.zip</code> into the Controller:</p>
      <form action="/upload{f"?token={token}" if token else ""}" method="POST" enctype="multipart/form-data" style="margin-top:1rem;">
        <input type="file" name="file" accept=".zip" style="color:var(--text-muted); margin-bottom:1rem; display:block;" required />
        <button type="submit" class="copy-btn" style="background:#3b82f6; border:none; padding:0.6rem 1.25rem;">Upload Backup .zip</button>
      </form>
    </div>
    ''' if is_authed else ''}
  </div>
  <script>
    const isAuthed = {"true" if is_authed else "false"};
    const currentToken = "{token}";

    if (isAuthed && currentToken) {{
      sessionStorage.setItem("pz_backups_token", currentToken);
    }} else if (!isAuthed) {{
      if (currentToken) {{
        sessionStorage.removeItem("pz_backups_token");
      }} else {{
        const stored = sessionStorage.getItem("pz_backups_token");
        if (stored && !window.location.search.includes("token=")) {{
          window.location.href = `/backups?token=${{encodeURIComponent(stored)}}`;
        }}
      }}
    }}

    function saveBackupToken(form) {{
      const input = form.querySelector("input[name='token']");
      if (input && input.value) {{
        sessionStorage.setItem("pz_backups_token", input.value);
      }}
    }}
  </script>
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

        # 2. Auth Verification API (GET)
        if path in ("/api/verify", "/api/verify-auth", "/auth/verify"):
            auth_type = query.get("type", ["server_files"])[0].lower()
            if auth_type in ("server", "server_files", "serverfiles"):
                is_valid = check_server_files_auth(self.headers, query)
                target_name = "Server Files"
            else:
                is_valid = check_backups_auth(self.headers, query)
                target_name = "Backups"

            if is_valid:
                body = json.dumps({"ok": True, "type": auth_type, "message": f"{target_name} password verified"}).encode("utf-8")
                self._send_response_headers(200, "application/json", len(body))
                self.wfile.write(body)
            else:
                body = json.dumps({"ok": False, "type": auth_type, "error": f"Incorrect {target_name} password. Access denied."}).encode("utf-8")
                self._send_response_headers(401, "application/json", len(body))
                self.wfile.write(body)
            return

        # 2. Main Dashboard (Player Downloads + Server IP + 3 Cards)
        if path in ("/", "/index.html"):
            server_info = get_server_info()
            manifest = get_manifest()
            
            server_token = query.get("server_token", [None])[0] or ""
            if not server_token and check_server_files_auth(self.headers, query):
                server_token = query.get("token", [None])[0] or query.get("key", [None])[0] or query.get("password", [None])[0] or ""
                
            backup_token = query.get("backup_token", [None])[0] or ""
            if not backup_token and check_backups_auth(self.headers, query):
                backup_token = query.get("token", [None])[0] or query.get("key", [None])[0] or query.get("password", [None])[0] or ""

            html = render_html_dashboard(server_info, manifest, server_token, backup_token).encode("utf-8")
            self._send_response_headers(200, "text/html; charset=utf-8", len(html))
            self.wfile.write(html)
            return

        # 3. Game Torrent Download (Public)
        if path in ("/game.torrent", "/torrent"):
            candidates = [
                SERVES_REPO / "game.torrent",
                PACKAGES_DIR / "game.torrent",
                Path("/data/game.torrent")
            ]
            for c in candidates:
                if c.is_file():
                    self._stream_file(c, "game.torrent", as_attachment=True)
                    return
            self._send_error(404, "game.torrent file not found in repository root")
            return

        # 4. Dedicated Backups Folder / Page
        if path in ("/backups", "/backups/"):
            server_info = get_server_info()
            is_authed = check_backups_auth(self.headers, query)
            token = query.get("token", [None])[0] or query.get("key", [None])[0] or query.get("password", [None])[0] or query.get("backup_token", [None])[0] or ""

            # Check if JSON API requested
            accept_header = self.headers.get("Accept", "")
            if "application/json" in accept_header:
                if not is_authed:
                    self._send_error(401, "Unauthorized: Valid backups password required")
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

        # 5. Live server_info.json (Public)
        if path == "/server_info.json":
            info = get_server_info()
            body = json.dumps(info, indent=2).encode("utf-8")
            self._send_response_headers(200, "application/json", len(body))
            self.wfile.write(body)
            return

        # 6. Packages Manifest (Public)
        if path in ("/manifest", "/packages_manifest.json"):
            manifest = get_manifest()
            body = json.dumps(manifest, indent=2).encode("utf-8")
            self._send_response_headers(200, "application/json", len(body))
            self.wfile.write(body)
            return

        # 7. Public Downloads: client.zip & common.zip
        if path == "/client.zip":
            self._stream_file(PACKAGES_DIR / "client.zip", "client.zip", as_attachment=True)
            return

        if path == "/common.zip":
            self._stream_file(PACKAGES_DIR / "common.zip", "common.zip", as_attachment=True)
            return

        # 8. PROTECTED DOWNLOAD: server.zip (Server Files Only)
        if path == "/server.zip":
            if not check_server_files_auth(self.headers, query):
                self._send_error(401, "Unauthorized: Valid server files password required for server.zip")
                return
            log("Authenticated download of server.zip accepted.")
            self._stream_file(PACKAGES_DIR / "server.zip", "server.zip", as_attachment=True)
            return

        # 9. PROTECTED DOWNLOAD: /backups/<filename>
        if path.startswith("/backups/"):
            if not check_backups_auth(self.headers, query):
                self._send_error(401, "Unauthorized: Valid backups password required to access backups")
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

        # Auth Verification API (POST)
        if path in ("/api/verify", "/api/verify-auth", "/auth/verify"):
            try:
                content_length = int(self.headers.get("Content-Length", 0))
                body_bytes = self.rfile.read(content_length) if content_length > 0 else b"{}"
                data = json.loads(body_bytes.decode("utf-8")) if body_bytes else {}
            except Exception:
                data = {}

            auth_type = str(data.get("type") or query.get("type", ["server_files"])[0]).lower()
            token = data.get("token") or data.get("password")
            if token:
                query["token"] = [str(token)]

            if auth_type in ("server", "server_files", "serverfiles"):
                is_valid = check_server_files_auth(self.headers, query)
                target_name = "Server Files"
            else:
                is_valid = check_backups_auth(self.headers, query)
                target_name = "Backups"

            if is_valid:
                body = json.dumps({"ok": True, "type": auth_type, "message": f"{target_name} password verified"}).encode("utf-8")
                self._send_response_headers(200, "application/json", len(body))
                self.wfile.write(body)
            else:
                body = json.dumps({"ok": False, "type": auth_type, "error": f"Incorrect {target_name} password. Access denied."}).encode("utf-8")
                self._send_response_headers(401, "application/json", len(body))
                self.wfile.write(body)
            return

        # 1. PROTECTED UPLOAD: POST /upload
        if path in ("/upload", "/upload/"):
            if not check_backups_auth(self.headers, query):
                self._send_error(401, "Unauthorized: Valid backups password required for upload")
                return

            ctype = self.headers.get("Content-Type", "")
            if not ctype.startswith("multipart/form-data"):
                self._send_error(400, "Content-Type must be multipart/form-data")
                return

            try:
                content_length = int(self.headers.get("Content-Length", 0))
                body_bytes = self.rfile.read(content_length)
                raw_message = f"Content-Type: {ctype}\r\n\r\n".encode("utf-8") + body_bytes
                msg = BytesParser().parsebytes(raw_message)

                uploaded_files = []
                BACKUPS_DIR.mkdir(parents=True, exist_ok=True)

                for part in msg.walk():
                    raw_filename = part.get_filename()
                    if raw_filename:
                        data = part.get_payload(decode=True)
                        if isinstance(data, bytes) and data:
                            clean_name = os.path.basename(raw_filename).strip("'\"")
                            if not clean_name.lower().endswith(".zip"):
                                clean_name = f"{clean_name}.zip"
                            if not clean_name or clean_name.startswith("."):
                                clean_name = f"backup_upload_{datetime.datetime.now().strftime('%Y%m%d_%H%M%S')}_{secrets.token_hex(2)}.zip"
                            save_path = BACKUPS_DIR / clean_name
                            save_path.write_bytes(data)
                            uploaded_files.append(clean_name)

                log(f"Authenticated upload completed: {uploaded_files}")
                token = query.get("token", [None])[0] or query.get("key", [None])[0] or query.get("backup_token", [None])[0] or ""
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
    log(f"Server files protection: {'ACTIVE' if SERVER_FILES_PASSWORD else 'NO PASSWORD SET (WARNING)'}")
    log(f"Backups protection: {'ACTIVE' if BACKUPS_PASSWORD else 'NO PASSWORD SET (WARNING)'}")
    server.serve_forever()


if __name__ == "__main__":
    run_server()
