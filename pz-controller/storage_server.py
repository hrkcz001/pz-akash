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
import hashlib
import hmac
import json
import os
import re
import secrets
import shlex
import subprocess
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlparse

HTTP_PORT = int(os.environ.get("HTTP_PORT", "8000"))
GAME_PORT = int(os.environ.get("GAME_PORT", "16261"))
RCON_PORT = int(os.environ.get("RCON_PORT", "27015"))
STORAGE_PASSWORD = os.environ.get("STORAGE_PASSWORD") or os.environ.get("CONTROLLER_SECRET") or ""
SERVER_FILES_PASSWORD = os.environ.get("SERVER_FILES_PASSWORD") or os.environ.get("SERVERFILES_PASSWORD") or STORAGE_PASSWORD
BACKUPS_PASSWORD = os.environ.get("BACKUPS_PASSWORD") or os.environ.get("BACKUP_PASSWORD") or os.environ.get("BACKUPS_SECRET") or STORAGE_PASSWORD
RCON_PASSWORD = os.environ.get("RCON_PASSWORD") or STORAGE_PASSWORD
WEBHOOK_SECRET = os.environ.get("WEBHOOK_SECRET", "")
WEBHOOK_TRIGGER = os.environ.get("WEBHOOK_TRIGGER", "/usr/local/bin/trigger.sh")
WEBHOOK_LOG = os.environ.get("WEBHOOK_LOG", "/data/webhook.log")
PACKAGES_DIR = Path(os.environ.get("PACKAGES_DIR", "/data/packages"))
BACKUPS_DIR = Path(os.environ.get("BACKUPS_DIR", "/data/backups"))
SERVES_REPO = Path(os.environ.get("SERVES_REPO", "/root/pz-saves"))

CHUNK_SIZE = 256 * 1024  # 256 KB streaming chunk

# Try importing rcon_query from local or standard paths
try:
    from rcon import rcon_query
except ImportError:
    try:
        sys.path.insert(0, "/usr/local/bin")
        sys.path.insert(0, str(Path(__file__).parent))
        from rcon import rcon_query
    except Exception:
        rcon_query = None


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


def parse_player_count(text: str) -> int:
    """Parse connected player count from PZ RCON 'players' response."""
    if not text:
        return 0
    # Match "Players connected (2):", "Players connected (0)", "Players (1):", etc.
    m = re.search(r"Players(?:\s+connected)?\s*\(\s*(\d+)\s*\)", text, re.IGNORECASE)
    if m:
        try:
            return int(m.group(1))
        except ValueError:
            pass
    if re.search(r"no\s+players(?:\s+connected)?", text, re.IGNORECASE):
        return 0
    # Check bullet lines if server sends a direct player list
    player_lines = [line.strip() for line in text.splitlines() if line.strip().startswith(("-", "*"))]
    if player_lines:
        return len(player_lines)
    m2 = re.search(r"(\d+)\s+players?", text, re.IGNORECASE)
    if m2:
        try:
            return int(m2.group(1))
        except ValueError:
            pass
    return 0


_player_count_cache = {"count": 0, "last_check": 0.0}
_player_count_lock = threading.Lock()


def get_player_count(ip: str, port: int = RCON_PORT, password: str = RCON_PASSWORD) -> int:
    """Fetch live player count via RCON with caching."""
    global _player_count_cache
    if not ip or ip == "pending":
        return 0
    now = time.time()
    with _player_count_lock:
        if now - _player_count_cache["last_check"] < 5.0:
            return _player_count_cache["count"]

    # Resolve RCON password if missing
    pwd = password
    if not pwd:
        dep_file = SERVES_REPO / "deployment.yaml"
        if dep_file.is_file():
            try:
                text = dep_file.read_text(encoding="utf-8")
                match = re.search(r"RCON_PASSWORD=([^\s]+)", text)
                if not match:
                    match = re.search(r"STORAGE_PASSWORD=([^\s]+)", text)
                if match:
                    pwd = match.group(1)
            except Exception:
                pass
        if not pwd:
            pwd = STORAGE_PASSWORD or ""

    if rcon_query is not None:
        try:
            ok, resp = rcon_query(ip, port, pwd, "players", timeout=3)
            if ok:
                count = parse_player_count(resp)
                with _player_count_lock:
                    _player_count_cache["count"] = count
                    _player_count_cache["last_check"] = now
                return count
        except Exception:
            pass

    with _player_count_lock:
        return _player_count_cache["count"]


def get_server_info():
    """Read server_info.json from pz-saves repo with git sync. Default to offline status and empty IP."""
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
            st = data.get("status", "offline").lower()
            if st == "stopped":
                st = "offline"
            p_hr = data.get("price_per_hour")
            if p_hr is None and data.get("price_per_day"):
                try:
                    p_hr = round(float(data.get("price_per_day")) / 24.0, 4)
                except Exception:
                    pass
            if p_hr is None:
                p_hr = 0.011

            raw_ip = data.get("ip", "")
            players_count = data.get("players_count")
            if st == "online" and raw_ip and raw_ip != "pending":
                if players_count is None:
                    players_count = get_player_count(raw_ip)
                else:
                    try:
                        players_count = int(players_count)
                    except Exception:
                        players_count = get_player_count(raw_ip)
            else:
                players_count = 0

            return {
                "ip": raw_ip if st == "online" else "",
                "raw_ip": raw_ip,
                "port": int(data.get("game_port") or (data.get("port") if data.get("port") != 2222 else None) or GAME_PORT),
                "ssh_port": int(data.get("ssh_port") or data.get("port") or 2222),
                "price_per_hour": float(p_hr),
                "status": st,
                "players_count": players_count,
                "players": players_count,
            }
        except Exception as e:
            log(f"Error reading server_info.json: {e}")
    return {"ip": "", "raw_ip": "", "port": GAME_PORT, "ssh_port": 2222, "price_per_hour": 0.011, "status": "offline", "players_count": 0, "players": 0}


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


def get_pz_version() -> str:
    """Resolve Project Zomboid version string for display."""
    candidates = [
        Path("/pz-server/PINNED_VERSION"),
        SERVES_REPO / "VERSION",
        SERVES_REPO / "PINNED_VERSION"
    ]
    for c in candidates:
        if c.is_file():
            try:
                v = c.read_text(encoding="utf-8").strip()
                if v:
                    return v
            except Exception:
                pass
    return os.environ.get("PZ_VERSION", "42.20.2")


def get_readme_html(lang: str = "ru") -> str:
    """Read README from pz-saves repository or return localized guide with Zomboid folder clean/rename recommendation."""
    if lang == "ru":
        candidates = [
            SERVES_REPO / "README.ru.md",
            SERVES_REPO / "README_RU.md",
            Path("/data/README.ru.md"),
        ]
        for c in candidates:
            if c.is_file():
                try:
                    content = c.read_text(encoding="utf-8")
                    if content.strip():
                        return markdown_to_html(content)
                except Exception as e:
                    log(f"Error reading {c}: {e}")

        # Russian Fallback instructions with clean/rename recommendation
        pz_v = get_pz_version()
        fallback_md = f"""# ☣️ Быстрый старт и подключение

> ⚠️ **Важно:** Перед установкой модов **настоятельно рекомендуется удалить или переименовать** существующую папку `Zomboid` (например, в `Zomboid_old`), чтобы избежать конфликтов со старыми версиями модов и поврежденным кэшем:
> * **Windows**: `%USERPROFILE%\\Zomboid\\` (например, `C:\\Users\\<Имя>\\Zomboid\\`)
> * **Linux / Steam Deck**: `~/Zomboid/`

1. **Чистый клиент игры**: Скачайте клиент через **`game.torrent`** выше (версия сервера **v{pz_v}**).
2. **Скачайте архивы модов**: Скачайте **`common.zip`** и **`client.zip`** с этой страницы.
3. **Распакуйте в папку Zomboid**: Распакуйте содержимое обоих архивов в вашу папку `Zomboid` с заменой файлов:
   * **Windows**: `%USERPROFILE%\\Zomboid\\`
   * **Linux / Steam Deck**: `~/Zomboid/`
4. **Подключение к серверу**: Запустите Project Zomboid → **Сетевая игра (Join)** → Введите **IP сервера**, Порт `16261` и Пароль сервера `1488`.
"""
        return markdown_to_html(fallback_md)
    else:
        candidates = [
            SERVES_REPO / "README.en.md",
            SERVES_REPO / "README_EN.md",
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

        pz_v = get_pz_version()
        fallback_md = f"""# ☣️ Quick Join Guide

> ⚠️ **Important:** Before installing mods, it is **strongly recommended to delete or rename** your existing `Zomboid` folder (e.g. to `Zomboid_old`) to avoid conflicts with old mod versions and corrupted cache:
> * **Windows**: `%USERPROFILE%\\Zomboid\\` (e.g. `C:\\Users\\<Name>\\Zomboid\\`)
> * **Linux / Steam Deck**: `~/Zomboid/`

1. **Clean Game Client**: Download via **`game.torrent`** above (matching server version **v{pz_v}**).
2. **Download Mod Packages**: Get both **`common.zip`** and **`client.zip`** from this hub.
3. **Extract & Overwrite**: Unzip both archives directly into your `Zomboid` folder with file replacement:
   * **Windows**: `%USERPROFILE%\\Zomboid\\`
   * **Linux / Steam Deck**: `~/Zomboid/`
4. **Connect to Server**: Launch Project Zomboid → **Join** → Enter **Server IP**, Port `16261`, and Server Password `1488`.
"""
        return markdown_to_html(fallback_md)


def format_pkg_stats(info: dict, lang: str = "ru") -> str:
    """Format package subtitle stats (mods count, files count, size) with localization."""
    mods = info.get("mods_count", 0)
    files = info.get("files_count", 0)
    size_bytes = info.get("size", 0)
    
    size_str = f"{size_bytes / (1024*1024):.1f} MB" if size_bytes >= 1024*1024 else (f"{size_bytes / 1024:.1f} KB" if size_bytes > 0 else ("Готов" if lang == "ru" else "Ready"))
    
    parts = []
    if mods > 0:
        if lang == "ru":
            n = abs(mods) % 100
            n1 = n % 10
            word = "модов" if 10 < n < 20 else ("мода" if 1 < n1 < 5 else ("мод" if n1 == 1 else "модов"))
            parts.append(f"{mods} {word}")
        else:
            parts.append(f"{mods} mod{'s' if mods != 1 else ''}")
    if files > 0:
        if lang == "ru":
            n = abs(files) % 100
            n1 = n % 10
            word = "файлов" if 10 < n < 20 else ("файла" if 1 < n1 < 5 else ("файл" if n1 == 1 else "файлов"))
            parts.append(f"{files} {word}")
        else:
            parts.append(f"{files} file{'s' if files != 1 else ''}")
    parts.append(size_str)
    
    return " • ".join(parts)


def render_html_dashboard(server_info: dict, manifest: dict, server_token: str = "", backup_token: str = "") -> str:
    status = server_info.get("status", "offline").lower()
    if status == "stopped":
        status = "offline"
    ip = server_info.get("raw_ip", "") if status == "online" else ""
    port = server_info.get("port", 16261)
    players_count = int(server_info.get("players_count", 0))
    pz_version = get_pz_version()
    
    is_online = (status == "online" and bool(ip) and ip != "pending")
    is_booting = (status == "booting")
    is_stopping = (status == "stopping")
    
    price_per_hour = float(server_info.get("price_per_hour", 0.011))
    price_fmt = f"${price_per_hour:.3f}/hr" if price_per_hour < 0.1 else f"${price_per_hour:.2f}/hr"
    show_price = (is_online or is_booting or is_stopping)
    
    # Status Badge (Default Russian text)
    if is_online:
        badge_class = "badge-online"
        badge_text = "ОНЛАЙН"
        dot_html = '<span class="status-dot online"></span>'
    elif is_booting:
        badge_class = "badge-booting"
        badge_text = "ЗАПУСК"
        dot_html = '<span class="status-dot booting"></span>'
    elif is_stopping:
        badge_class = "badge-stopping"
        badge_text = "ОСТАНОВКА"
        dot_html = '<span class="status-dot stopping"></span>'
    else:
        badge_class = "badge-offline"
        badge_text = "ОФФЛАЙН"
        dot_html = '<span class="status-dot offline"></span>'

    # Russian players count formatting
    n_p = abs(players_count) % 100
    n1_p = n_p % 10
    word_p = "игроков" if 10 < n_p < 20 else ("игрока" if 1 < n1_p < 5 else ("игрок" if n1_p == 1 else "игроков"))
    players_text = f"{players_count} {word_p}"

    # Connection Address / Status Widget (IP, Port, Server Password)
    if is_online:
        address_widget_html = f"""
        <div class="address-grid">
          <div class="address-card">
            <div class="address-label" id="lbl-server-ip">IP СЕРВЕРА</div>
            <div class="address-value-row">
              <span class="address-text" id="ip-val">{ip}</span>
            </div>
          </div>
          <div class="address-card">
            <div class="address-label" id="lbl-port">ПОРТ</div>
            <div class="address-value-row">
              <span class="address-text" id="port-val">{port}</span>
            </div>
          </div>
          <div class="address-card">
            <div class="address-label" id="lbl-password">ПАРОЛЬ СЕРВЕРА</div>
            <div class="address-value-row">
              <span class="address-text" id="pwd-val" style="color:#fbbf24;">1488</span>
            </div>
          </div>
        </div>
        """
    elif is_booting:
        address_widget_html = """
        <div class="status-banner booting-banner">
          <div class="status-banner-icon">🚀</div>
          <div class="status-banner-text">
            <div class="status-banner-title">Сервер запускается</div>
            <div class="status-banner-desc">Инициализация инстанса на Akash Network. IP и порт появятся здесь автоматически после старта.</div>
          </div>
        </div>
        """
    elif is_stopping:
        address_widget_html = """
        <div class="status-banner stopping-banner">
          <div class="status-banner-icon">🛑</div>
          <div class="status-banner-text">
            <div class="status-banner-title">Сервер выключается</div>
            <div class="status-banner-desc">Сохранение игрового мира, создание бэкапа и остановка инстанса. Скоро сервер перейдет в оффлайн.</div>
          </div>
        </div>
        """
    else:
        address_widget_html = """
        <div class="status-banner offline-banner">
          <div class="status-banner-icon">⏸️</div>
          <div class="status-banner-text">
            <div class="status-banner-title">Сервер оффлайн</div>
            <div class="status-banner-desc">Сервер выключен. Вы можете скачать клиент и моды ниже для подготовки к игре.</div>
          </div>
        </div>
        """

    client_info = manifest.get("client", {})
    common_info = manifest.get("common", {})
    server_pkg_info = manifest.get("server", {})

    client_stats_str = format_pkg_stats(client_info, "ru")
    common_stats_str = format_pkg_stats(common_info, "ru")
    server_stats_str = format_pkg_stats(server_pkg_info, "ru")

    readme_html_ru = get_readme_html(lang="ru")
    readme_html_en = get_readme_html(lang="en")

    pkg_stats_json = json.dumps({
        "client": {"mods": client_info.get("mods_count", 0), "files": client_info.get("files_count", 0), "size": client_info.get("size", 0)},
        "common": {"mods": common_info.get("mods_count", 0), "files": common_info.get("files_count", 0), "size": common_info.get("size", 0)},
        "server": {"mods": server_pkg_info.get("mods_count", 0), "files": server_pkg_info.get("files_count", 0), "size": server_pkg_info.get("size", 0)},
    })

    return f"""<!DOCTYPE html>
<html lang="ru">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>Всрания • Хаб</title>
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
      align-items: center;
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
    .lang-switch {{
      display: inline-flex;
      align-items: center;
      background: rgba(255, 255, 255, 0.05);
      border: 1px solid var(--card-border);
      border-radius: 8px;
      padding: 0.2rem 0.35rem;
      gap: 0.25rem;
      margin-left: 0.5rem;
    }}
    .lang-btn {{
      background: transparent;
      border: none;
      color: var(--text-muted);
      font-size: 0.78rem;
      font-weight: 700;
      padding: 0.25rem 0.5rem;
      border-radius: 6px;
      cursor: pointer;
      transition: all 0.15s ease;
    }}
    .lang-btn.active {{
      background: var(--primary);
      color: white;
      box-shadow: 0 0 10px var(--primary-glow);
    }}
    .lang-btn:hover:not(.active) {{
      color: white;
      background: rgba(255, 255, 255, 0.08);
    }}
    .lang-divider {{
      color: rgba(255, 255, 255, 0.2);
      font-size: 0.75rem;
      user-select: none;
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
    .status-group {{
      display: inline-flex;
      align-items: center;
      gap: 0.6rem;
      flex-wrap: wrap;
    }}
    .price-badge {{
      display: inline-flex;
      align-items: center;
      gap: 0.35rem;
      padding: 0.35rem 0.85rem;
      border-radius: 9999px;
      font-size: 0.78rem;
      font-weight: 700;
      font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
      letter-spacing: 0.03em;
      border: 1.5px dashed rgba(56, 189, 248, 0.45);
      background: rgba(14, 165, 233, 0.08);
      color: #38bdf8;
      box-shadow: 0 0 12px rgba(14, 165, 233, 0.1);
      transition: all 0.2s ease;
    }}
    .price-badge:hover {{
      border-color: rgba(56, 189, 248, 0.8);
      background: rgba(14, 165, 233, 0.15);
    }}
    .players-badge {{
      display: inline-flex;
      align-items: center;
      gap: 0.35rem;
      padding: 0.35rem 0.85rem;
      border-radius: 9999px;
      font-size: 0.78rem;
      font-weight: 700;
      letter-spacing: 0.03em;
      border: 1.5px solid rgba(16, 185, 129, 0.4);
      background: rgba(16, 185, 129, 0.1);
      color: #34d399;
      box-shadow: 0 0 12px rgba(16, 185, 129, 0.15);
      transition: all 0.2s ease;
    }}
    .players-badge:hover {{
      border-color: rgba(16, 185, 129, 0.8);
      background: rgba(16, 185, 129, 0.2);
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
    .badge-stopping {{
      background: rgba(249, 115, 22, 0.15);
      color: #fb923c;
      border-color: rgba(249, 115, 22, 0.35);
      box-shadow: 0 0 15px rgba(249, 115, 22, 0.2);
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
    .status-dot.stopping {{
      background: #f97316;
      animation: pulse 1.2s infinite;
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
      letter-spacing: 0.05em;
      margin-bottom: 0.35rem;
    }}
    .address-value-row {{
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 0.5rem;
    }}
    .address-text {{
      font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
      font-size: 1.15rem;
      font-weight: 700;
      color: white;
      letter-spacing: 0.02em;
    }}
    .copy-btn {{
      background: rgba(255, 255, 255, 0.08);
      border: 1px solid rgba(255, 255, 255, 0.12);
      color: var(--text-muted);
      border-radius: 6px;
      padding: 0.35rem 0.65rem;
      font-size: 0.75rem;
      font-weight: 600;
      cursor: pointer;
      transition: all 0.15s;
      display: inline-flex;
      align-items: center;
      gap: 0.3rem;
    }}
    .copy-btn:hover {{
      background: rgba(255, 255, 255, 0.18);
      color: white;
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
    .stopping-banner {{
      background: rgba(249, 115, 22, 0.08);
      border: 1px solid rgba(249, 115, 22, 0.25);
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
    .version-badge {{
      display: inline-flex;
      align-items: center;
      background: rgba(139, 92, 246, 0.3);
      border: 1px solid rgba(167, 139, 250, 0.4);
      color: #ddd6fe;
      font-size: 0.75rem;
      font-weight: 700;
      padding: 0.2rem 0.6rem;
      border-radius: 9999px;
      letter-spacing: 0.04em;
      box-shadow: 0 0 10px rgba(139, 92, 246, 0.2);
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
      min-height: 180px;
      transition: transform 0.2s, border-color 0.2s;
    }}
    .action-card:hover {{
      transform: translateY(-3px);
      border-color: rgba(255, 255, 255, 0.15);
    }}
    .card-client {{
      box-shadow: 0 10px 20px -5px var(--accent-glow);
    }}
    .card-common {{
      box-shadow: 0 10px 20px -5px var(--primary-glow);
    }}
    .card-server {{
      opacity: 0.85;
      position: relative;
    }}
    .card-server.unlocked {{
      opacity: 1;
      box-shadow: 0 10px 20px -5px rgba(245, 158, 11, 0.25);
    }}
    .card-header {{
      display: flex;
      align-items: center;
      gap: 0.75rem;
      margin-bottom: 0.6rem;
    }}
    .card-icon-box {{
      width: 36px;
      height: 36px;
      border-radius: 10px;
      display: flex;
      align-items: center;
      justify-content: center;
      font-size: 1.15rem;
    }}
    .client-icon-box {{
      background: rgba(16, 185, 129, 0.15);
      border: 1px solid rgba(16, 185, 129, 0.3);
    }}
    .common-icon-box {{
      background: rgba(59, 130, 246, 0.15);
      border: 1px solid rgba(59, 130, 246, 0.3);
    }}
    .server-icon-box {{
      background: rgba(245, 158, 11, 0.15);
      border: 1px solid rgba(245, 158, 11, 0.3);
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
      width: 100%;
      padding: 0.7rem;
      border-radius: 10px;
      font-weight: 600;
      font-size: 0.85rem;
      text-decoration: none;
      display: flex;
      align-items: center;
      justify-content: center;
      gap: 0.5rem;
      cursor: pointer;
      transition: all 0.15s;
      border: none;
    }}
    .btn-client {{
      background: var(--accent);
      color: #064e3b;
      font-weight: 700;
    }}
    .btn-client:hover {{
      background: #34d399;
    }}
    .btn-common {{
      background: var(--primary);
      color: white;
    }}
    .btn-common:hover {{
      background: var(--primary-hover);
    }}
    .btn-locked {{
      background: rgba(255, 255, 255, 0.08);
      color: var(--text-muted);
      border: 1px solid var(--card-border);
    }}
    .btn-unlocked {{
      background: #f59e0b;
      color: #451a03;
      font-weight: 700;
    }}
    .btn-unlocked:hover {{
      background: #fbbf24;
    }}

    /* Backups Button Footer */
    .backups-footer {{
      display: flex;
      justify-content: center;
      margin: 1rem 0 2rem;
    }}
    .backups-faded-btn {{
      background: transparent;
      border: 1px solid rgba(255, 255, 255, 0.08);
      color: var(--text-muted);
      border-radius: 9999px;
      padding: 0.5rem 1.25rem;
      font-size: 0.85rem;
      font-weight: 500;
      cursor: pointer;
      display: inline-flex;
      align-items: center;
      gap: 0.5rem;
      transition: all 0.2s;
    }}
    .backups-faded-btn:hover {{
      background: rgba(255, 255, 255, 0.05);
      color: var(--text);
      border-color: rgba(255, 255, 255, 0.2);
      transform: translateY(-1px);
    }}

    /* Dynamic Readme Section */
    .readme-card {{
      background: var(--card-bg);
      backdrop-filter: blur(16px);
      border: 1px solid var(--card-border);
      border-radius: 16px;
      padding: 2rem;
    }}
    .readme-header {{
      display: flex;
      align-items: center;
      gap: 0.6rem;
      margin-bottom: 1.25rem;
      border-bottom: 1px solid var(--card-border);
      padding-bottom: 1rem;
    }}
    .readme-header h2 {{
      font-size: 1.25rem;
      margin: 0;
    }}
    .readme-body {{
      font-size: 0.9rem;
      line-height: 1.65;
      color: #cbd5e1;
    }}
    .readme-body h1 {{ font-size: 1.35rem; color: white; margin: 1.25rem 0 0.75rem; }}
    .readme-body h2 {{ font-size: 1.15rem; color: white; margin: 1.25rem 0 0.5rem; }}
    .readme-body h3 {{ font-size: 1.05rem; color: white; margin: 1rem 0 0.5rem; }}
    .readme-body p {{ margin: 0.6rem 0; }}
    .readme-body ul, .readme-body ol {{ padding-left: 1.5rem; margin: 0.6rem 0; }}
    .readme-body li {{ margin: 0.35rem 0; }}
    .readme-body code {{
      background: rgba(0, 0, 0, 0.5);
      padding: 0.15rem 0.4rem;
      border-radius: 5px;
      font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
      font-size: 0.85em;
      color: #38bdf8;
    }}
    .readme-body blockquote {{
      background: rgba(245, 158, 11, 0.08);
      border-left: 3px solid #f59e0b;
      margin: 1rem 0;
      padding: 0.75rem 1rem;
      border-radius: 0 8px 8px 0;
      color: #fef08a;
    }}
    .readme-body pre {{
      background: #020617;
      padding: 1rem;
      border-radius: 8px;
      overflow-x: auto;
      border: 1px solid rgba(255, 255, 255, 0.05);
    }}
    .readme-body a {{
      color: #38bdf8;
      text-decoration: none;
    }}
    .readme-body a:hover {{
      text-decoration: underline;
    }}

    /* Password Modal */
    .modal-backdrop {{
      position: fixed;
      top: 0; left: 0; right: 0; bottom: 0;
      background: rgba(0, 0, 0, 0.75);
      backdrop-filter: blur(8px);
      display: none;
      align-items: center;
      justify-content: center;
      z-index: 100;
      padding: 1rem;
    }}
    .modal-box {{
      background: #0f172a;
      border: 1px solid rgba(255, 255, 255, 0.15);
      border-radius: 16px;
      padding: 1.75rem;
      max-width: 400px;
      width: 100%;
      box-shadow: 0 25px 50px -12px rgba(0, 0, 0, 0.8);
      animation: modalSlide 0.2s ease-out;
    }}
    @keyframes modalSlide {{
      from {{ transform: translateY(15px); opacity: 0; }}
      to {{ transform: translateY(0); opacity: 1; }}
    }}
    .modal-box h3 {{
      margin: 0 0 0.5rem 0;
      font-size: 1.2rem;
    }}
    .modal-box p {{
      margin: 0 0 1.25rem 0;
      color: var(--text-muted);
      font-size: 0.85rem;
      line-height: 1.4;
    }}
    .modal-input {{
      width: 100%;
      background: #020617;
      border: 1px solid rgba(255, 255, 255, 0.15);
      border-radius: 8px;
      padding: 0.75rem 1rem;
      color: white;
      font-size: 0.95rem;
      margin-bottom: 1rem;
      outline: none;
      transition: border-color 0.15s;
    }}
    .modal-input:focus {{
      border-color: var(--primary);
    }}
    .modal-error {{
      color: #fca5a5;
      background: rgba(239, 68, 68, 0.15);
      border: 1px solid rgba(239, 68, 68, 0.3);
      padding: 0.5rem 0.75rem;
      border-radius: 6px;
      font-size: 0.8rem;
      margin-bottom: 1rem;
      display: none;
    }}
    .modal-actions {{
      display: flex;
      justify-content: flex-end;
      gap: 0.75rem;
    }}
    .modal-btn {{
      padding: 0.6rem 1.15rem;
      border-radius: 8px;
      font-size: 0.85rem;
      font-weight: 600;
      cursor: pointer;
      border: none;
      transition: all 0.15s;
    }}
    .modal-btn-cancel {{
      background: rgba(255, 255, 255, 0.08);
      color: var(--text-muted);
    }}
    .modal-btn-cancel:hover {{
      background: rgba(255, 255, 255, 0.15);
      color: white;
    }}
    .modal-btn-submit {{
      background: var(--primary);
      color: white;
    }}
    .modal-btn-submit:hover {{
      background: var(--primary-hover);
    }}
    .shake {{
      animation: shake 0.35s ease-in-out;
    }}
    @keyframes shake {{
      0%, 100% {{ transform: translateX(0); }}
      20%, 60% {{ transform: translateX(-6px); }}
      40%, 80% {{ transform: translateX(6px); }}
    }}
  </style>
</head>
<body>
  <div class="container">
    
    <!-- Navigation Bar -->
    <nav class="nav-bar">
      <div class="brand-title">
        <span class="brand-icon">☣️</span>
        <span id="brand-title-text">Всрания • Хаб</span>
      </div>
      <div class="nav-links">
        <a href="/" class="nav-item active" id="nav-packages-link">Пакеты</a>
        <a href="javascript:void(0)" onclick="openBackups()" class="nav-item" id="nav-backups-link">Бэкапы 🔒</a>
        <div class="lang-switch">
          <button type="button" class="lang-btn active" id="lang-btn-ru" onclick="setLanguage('ru')">RU</button>
          <span class="lang-divider">|</span>
          <button type="button" class="lang-btn" id="lang-btn-en" onclick="setLanguage('en')">EN</button>
        </div>
      </div>
    </nav>

    <!-- Header & Status Card -->
    <div class="header-card">
      <div class="header-top">
        <div class="server-title-block">
          <h1><span id="server-title-text">Статус сервера</span></h1>
          <p id="server-subtitle-text">Akash Network deployment</p>
        </div>
        <div class="status-group">
          <div class="players-badge" id="players-badge-container" style="{'display:inline-flex;' if is_online else 'display:none;'}">
            <span>👥</span>
            <span id="players-count-text">{players_text}</span>
          </div>
          <div class="price-badge" id="price-badge-container" style="{'display:inline-flex;' if show_price else 'display:none;'}">
            <span id="price-badge-text">{price_fmt}</span>
          </div>
          <div class="status-badge {badge_class}" id="status-badge-container">
            {dot_html}
            <span id="status-badge-text">{badge_text}</span>
          </div>
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
        <div style="display:flex; align-items:center; gap:0.6rem; margin-bottom:0.35rem; flex-wrap:wrap;">
          <div class="torrent-title" id="torrent-title-text">🎮 Чистый клиент игры (.torrent)</div>
          <span class="version-badge">v{pz_version}</span>
        </div>
        <div class="torrent-desc" id="torrent-desc-text">Проверенный клиент, соответствующий версии сервера. Настоятельно рекомендуется удалить или переименовать папку Zomboid перед установкой.</div>
      </div>
      <a href="/game.torrent" class="torrent-btn" download>
        <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"></path><polyline points="7 10 12 15 17 10"></polyline><line x1="12" y1="15" x2="12" y2="3"></line></svg>
        <span id="torrent-btn-text">Скачать .torrent</span>
      </a>
    </div>

    <!-- 3 Action Cards (Client, Common, Server) -->
    <div class="cards-grid">
      
      <!-- Client Package -->
      <div class="action-card card-client">
        <div>
          <div class="card-header">
            <div class="card-icon-box client-icon-box">🎮</div>
            <h3 class="card-title" id="card-client-title">Файлы клиента</h3>
          </div>
          <div class="card-stats" id="card-client-stats">{client_stats_str}</div>
        </div>
        <a href="/client.zip" class="card-btn btn-client" download>
          <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"></path><polyline points="7 10 12 15 17 10"></polyline><line x1="12" y1="15" x2="12" y2="3"></line></svg>
          <span id="card-client-btn-text">Скачать client.zip</span>
        </a>
      </div>

      <!-- Common Package -->
      <div class="action-card card-common">
        <div>
          <div class="card-header">
            <div class="card-icon-box common-icon-box">📦</div>
            <h3 class="card-title" id="card-common-title">Общие файлы</h3>
          </div>
          <div class="card-stats" id="card-common-stats">{common_stats_str}</div>
        </div>
        <a href="/common.zip" class="card-btn btn-common" download>
          <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"></path><polyline points="7 10 12 15 17 10"></polyline><line x1="12" y1="15" x2="12" y2="3"></line></svg>
          <span id="card-common-btn-text">Скачать common.zip</span>
        </a>
      </div>

      <!-- Server Package (Protected / Locked) -->
      <div class="action-card card-server" id="server-card">
        <div>
          <div class="card-header">
            <div class="card-icon-box server-icon-box" id="server-icon-box">🔒</div>
            <h3 class="card-title" id="card-server-title">Файлы сервера</h3>
          </div>
          <div class="card-stats" id="server-stats">{server_stats_str}</div>
        </div>
        <button type="button" class="card-btn btn-locked" id="server-btn" onclick="handleServerDownload()">
          <span id="server-btn-icon">🔒</span>
          <span id="server-btn-text">Разблокировать server.zip</span>
        </button>
      </div>

    </div>

    <!-- Faded Backups Button at Bottom -->
    <div class="backups-footer">
      <button type="button" class="backups-faded-btn" onclick="openBackups()">
        <span>🗄️</span>
        <span id="backups-footer-btn-text">Бэкапы 🔒</span>
      </button>
    </div>

    <!-- Dynamic Readme Section -->
    <div class="readme-card">
      <div class="readme-header">
        <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="#38bdf8" stroke-width="2"><path d="M4 19.5A2.5 2.5 0 0 1 6.5 17H20"></path><path d="M6.5 2H20v20H6.5A2.5 2.5 0 0 1 4 19.5v-15A2.5 2.5 0 0 1 6.5 2z"></path></svg>
        <h2 id="guide-header-title">Инструкция по установке</h2>
      </div>
      <div class="readme-body" id="readme-body-container">
        <div id="readme-content-ru">{readme_html_ru}</div>
        <div id="readme-content-en" style="display:none;">{readme_html_en}</div>
      </div>
    </div>

  </div>

  <!-- Password Unlock Modal -->
  <div class="modal-backdrop" id="auth-modal" onclick="if(event.target===this) closeModal()">
    <div class="modal-box">
      <h3 id="auth-modal-title">🔒 Требуется пароль</h3>
      <p id="auth-modal-desc">Введите пароль для продолжения:</p>
      <div class="modal-error" id="auth-error-msg"></div>
      <input type="password" class="modal-input" id="auth-password-input" placeholder="Пароль..." onkeydown="if(event.key==='Enter') submitPassword()" />
      <div class="modal-actions">
        <button type="button" class="modal-btn modal-btn-cancel" id="auth-cancel-btn" onclick="closeModal()">Отмена</button>
        <button type="button" class="modal-btn modal-btn-submit" id="auth-submit-btn" onclick="submitPassword()">Разблокировать</button>
      </div>
    </div>
  </div>

  <script>
    const i18nData = {{
      ru: {{
        page_title: "Всрания • Хаб",
        brand: "Всрания • Хаб",
        nav_packages: "Пакеты",
        nav_backups: "Бэкапы 🔒",
        server_title: "Статус сервера",
        server_subtitle: "Akash Network deployment",
        status_online: "ОНЛАЙН",
        status_booting: "ЗАПУСК",
        status_stopping: "ОСТАНОВКА",
        status_offline: "ОФФЛАЙН",
        lbl_ip: "IP СЕРВЕРА",
        lbl_port: "ПОРТ",
        lbl_password: "ПАРОЛЬ СЕРВЕРА",
        copied: "Скопировано! ✨",
        copy: "Копировать",
        banner_booting_title: "Сервер запускается",
        banner_booting_desc: "Инициализация инстанса на Akash Network. IP и порт появятся здесь автоматически после старта.",
        banner_stopping_title: "Сервер выключается",
        banner_stopping_desc: "Сохранение игрового мира, создание бэкапа и остановка инстанса. Скоро сервер перейдет в оффлайн.",
        banner_offline_title: "Сервер оффлайн",
        banner_offline_desc: "Сервер выключен. Вы можете скачать клиент и моды ниже для подготовки к игре.",
        torrent_title: "🎮 Чистый клиент игры (.torrent)",
        torrent_desc: "Проверенный клиент, соответствующий версии сервера. Настоятельно рекомендуется удалить или переименовать папку Zomboid перед установкой.",
        torrent_btn: "Скачать .torrent",
        card_client_title: "Файлы клиента",
        card_client_btn: "Скачать client.zip",
        card_common_title: "Общие файлы",
        card_common_btn: "Скачать common.zip",
        card_server_title: "Файлы сервера",
        card_server_btn_locked: "Разблокировать server.zip",
        card_server_btn_unlocked: "Скачать server.zip",
        backups_footer: "Бэкапы 🔒",
        guide_title: "Инструкция по установке",
        modal_server_title: "🔒 Файлы сервера",
        modal_server_desc: "Введите пароль для скачивания server.zip (конфигурации и серверные моды):",
        modal_server_placeholder: "Пароль файлов сервера...",
        modal_backups_title: "🔒 Доступ к бэкапам",
        modal_backups_desc: "Введите пароль для доступа к архивам мира:",
        modal_backups_placeholder: "Пароль бэкапов...",
        modal_cancel: "Отмена",
        modal_unlock: "Разблокировать",
        modal_verifying: "Проверка...",
        modal_err_empty: "Пожалуйста, введите пароль.",
        modal_err_wrong: "Неверный пароль. Доступ запрещен.",
        modal_err_network: "Ошибка сети при проверке пароля.",
      }},
      en: {{
        page_title: "Vsrania • Hub",
        brand: "Vsrania • Hub",
        nav_packages: "Packages",
        nav_backups: "Backups 🔒",
        server_title: "Server status",
        server_subtitle: "Akash Network deployment",
        status_online: "ONLINE",
        status_booting: "STARTING UP",
        status_stopping: "STOPPING",
        status_offline: "OFFLINE",
        lbl_ip: "SERVER IP",
        lbl_port: "PORT",
        lbl_password: "SERVER PASSWORD",
        copied: "Copied! ✨",
        copy: "Copy",
        banner_booting_title: "Vsrania is Starting Up",
        banner_booting_desc: "Initializing instance on Akash. IP and Port will appear here automatically when ready.",
        banner_stopping_title: "Vsrania is Shutting Down",
        banner_stopping_desc: "Saving game world, creating backup, and stopping instance. Server will be offline shortly.",
        banner_offline_title: "Vsrania is Offline",
        banner_offline_desc: "Server is offline. You can download mods below in preparation for the session.",
        torrent_title: "🎮 Clean Game Client (.torrent)",
        torrent_desc: "Pre-tested client matching server version. Recommended to delete or rename your existing Zomboid folder before install.",
        torrent_btn: "Download .torrent",
        card_client_title: "Client Files",
        card_client_btn: "Download client.zip",
        card_common_title: "Common Files",
        card_common_btn: "Download common.zip",
        card_server_title: "Server Files",
        card_server_btn_locked: "Unlock server.zip",
        card_server_btn_unlocked: "Download server.zip",
        backups_footer: "Backups 🔒",
        guide_title: "Quick Guide",
        modal_server_title: "🔒 Unlock Server Files",
        modal_server_desc: "Enter password to download server.zip (server configs & mods):",
        modal_server_placeholder: "Server files password...",
        modal_backups_title: "🔒 Unlock Backups",
        modal_backups_desc: "Enter password to access world save archives:",
        modal_backups_placeholder: "Backups password...",
        modal_cancel: "Cancel",
        modal_unlock: "Unlock",
        modal_verifying: "Verifying...",
        modal_err_empty: "Please enter a password.",
        modal_err_wrong: "Incorrect password. Access denied.",
        modal_err_network: "Network error while verifying password.",
      }}
    }};

    const pkgStatsData = {pkg_stats_json};
    let savedServerToken = sessionStorage.getItem("pz_server_files_token") || "{server_token}";
    let savedBackupToken = sessionStorage.getItem("pz_backups_token") || "{backup_token}";
    let pendingAction = null;
    let currentLang = localStorage.getItem("pz_lang") || "ru";

    let lastKnownStatus = "{status}";
    let lastKnownIp = "{ip}";
    let lastKnownPort = {port};
    let lastKnownPrice = {price_per_hour};
    let lastKnownPlayers = {players_count};

    function formatPlayersCount(count, lang) {{
      if (lang === "en") {{
        return `${{count}} player${{count === 1 ? '' : 's'}}`;
      }}
      const n = Math.abs(count) % 100;
      const n1 = n % 10;
      if (n > 10 && n < 20) return `${{count}} игроков`;
      if (n1 > 1 && n1 < 5) return `${{count}} игрока`;
      if (n1 === 1) return `${{count}} игрок`;
      return `${{count}} игроков`;
    }}

    function formatStats(mods, files, sizeBytes, lang) {{
      const sizeStr = sizeBytes >= 1024*1024 ? (sizeBytes / (1024*1024)).toFixed(1) + " MB" : (sizeBytes > 0 ? (sizeBytes / 1024).toFixed(1) + " KB" : (lang === "ru" ? "Готов" : "Ready"));
      const parts = [];
      if (mods > 0) {{
        if (lang === "ru") {{
          const n = Math.abs(mods) % 100;
          const n1 = n % 10;
          const word = (n > 10 && n < 20) ? "модов" : (n1 > 1 && n1 < 5) ? "мода" : (n1 === 1) ? "мод" : "модов";
          parts.push(`${{mods}} ${{word}}`);
        }} else {{
          parts.push(`${{mods}} mod${{mods !== 1 ? 's' : ''}}`);
        }}
      }}
      if (files > 0) {{
        if (lang === "ru") {{
          const n = Math.abs(files) % 100;
          const n1 = n % 10;
          const word = (n > 10 && n < 20) ? "файлов" : (n1 > 1 && n1 < 5) ? "файла" : (n1 === 1) ? "файл" : "файлов";
          parts.push(`${{files}} ${{word}}`);
        }} else {{
          parts.push(`${{files}} file${{files !== 1 ? 's' : ''}}`);
        }}
      }}
      parts.push(sizeStr);
      return parts.join(" • ");
    }}

    function setLanguage(lang) {{
      currentLang = lang;
      localStorage.setItem("pz_lang", lang);

      const t = i18nData[lang] || i18nData.ru;
      document.title = t.page_title;

      const btnRu = document.getElementById("lang-btn-ru");
      const btnEn = document.getElementById("lang-btn-en");
      if (btnRu && btnEn) {{
        btnRu.className = "lang-btn" + (lang === "ru" ? " active" : "");
        btnEn.className = "lang-btn" + (lang === "en" ? " active" : "");
      }}

      const brandEl = document.getElementById("brand-title-text");
      if (brandEl) brandEl.textContent = t.brand;

      const navPkg = document.getElementById("nav-packages-link");
      if (navPkg) navPkg.textContent = t.nav_packages;
      const navBkp = document.getElementById("nav-backups-link");
      if (navBkp) navBkp.textContent = t.nav_backups;

      const sTitle = document.getElementById("server-title-text");
      if (sTitle) sTitle.textContent = t.server_title;
      const sSub = document.getElementById("server-subtitle-text");
      if (sSub) sSub.textContent = t.server_subtitle;

      const tTitle = document.getElementById("torrent-title-text");
      if (tTitle) tTitle.textContent = t.torrent_title;
      const tDesc = document.getElementById("torrent-desc-text");
      if (tDesc) tDesc.textContent = t.torrent_desc;
      const tBtn = document.getElementById("torrent-btn-text");
      if (tBtn) tBtn.textContent = t.torrent_btn;

      const cTitle = document.getElementById("card-client-title");
      if (cTitle) cTitle.textContent = t.card_client_title;
      const cBtn = document.getElementById("card-client-btn-text");
      if (cBtn) cBtn.textContent = t.card_client_btn;
      const cStats = document.getElementById("card-client-stats");
      if (cStats && pkgStatsData.client) cStats.textContent = formatStats(pkgStatsData.client.mods, pkgStatsData.client.files, pkgStatsData.client.size, lang);

      const cmTitle = document.getElementById("card-common-title");
      if (cmTitle) cmTitle.textContent = t.card_common_title;
      const cmBtn = document.getElementById("card-common-btn-text");
      if (cmBtn) cmBtn.textContent = t.card_common_btn;
      const cmStats = document.getElementById("card-common-stats");
      if (cmStats && pkgStatsData.common) cmStats.textContent = formatStats(pkgStatsData.common.mods, pkgStatsData.common.files, pkgStatsData.common.size, lang);

      const srvTitle = document.getElementById("card-server-title");
      if (srvTitle) srvTitle.textContent = t.card_server_title;
      const srvBtn = document.getElementById("server-btn-text");
      if (srvBtn) {{
        const isUnlocked = srvBtn.getAttribute("data-unlocked") === "true";
        srvBtn.textContent = isUnlocked ? t.card_server_btn_unlocked : t.card_server_btn_locked;
      }}
      const srvStats = document.getElementById("server-stats");
      if (srvStats && pkgStatsData.server) srvStats.textContent = formatStats(pkgStatsData.server.mods, pkgStatsData.server.files, pkgStatsData.server.size, lang);

      const bkpFooter = document.getElementById("backups-footer-btn-text");
      if (bkpFooter) bkpFooter.textContent = t.backups_footer;

      const gTitle = document.getElementById("guide-header-title");
      if (gTitle) gTitle.textContent = t.guide_title;

      const readmeRu = document.getElementById("readme-content-ru");
      const readmeEn = document.getElementById("readme-content-en");
      if (readmeRu && readmeEn) {{
        readmeRu.style.display = (lang === "ru") ? "block" : "none";
        readmeEn.style.display = (lang === "en") ? "block" : "none";
      }}

      // Refresh status UI with active language
      updateStatusUI(lastKnownStatus, lastKnownIp, lastKnownPort, lastKnownPrice, lastKnownPlayers);
    }}

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
      const t = i18nData[currentLang] || i18nData.ru;
      navigator.clipboard.writeText(text).then(() => {{
        const origHtml = btn.innerHTML;
        btn.innerHTML = `<span>${{t.copied}}</span>`;
        setTimeout(() => btn.innerHTML = origHtml, 1500);
      }});
    }}

    function openModal(action) {{
      pendingAction = action;
      const t = i18nData[currentLang] || i18nData.ru;
      const modal = document.getElementById("auth-modal");
      const title = document.getElementById("auth-modal-title");
      const desc = document.getElementById("auth-modal-desc");
      const input = document.getElementById("auth-password-input");
      const errorBox = document.getElementById("auth-error-msg");
      const submitBtn = document.getElementById("auth-submit-btn");
      const cancelBtn = document.getElementById("auth-cancel-btn");

      errorBox.style.display = "none";
      errorBox.innerHTML = "";
      input.classList.remove("shake");
      submitBtn.disabled = false;
      submitBtn.innerText = t.modal_unlock;
      if (cancelBtn) cancelBtn.innerText = t.modal_cancel;

      if (action === "server_download") {{
        title.innerText = t.modal_server_title;
        desc.innerText = t.modal_server_desc;
        input.placeholder = t.modal_server_placeholder;
      }} else if (action === "backups") {{
        title.innerText = t.modal_backups_title;
        desc.innerText = t.modal_backups_desc;
        input.placeholder = t.modal_backups_placeholder;
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
      const t = i18nData[currentLang] || i18nData.ru;
      const input = document.getElementById("auth-password-input");
      const errorBox = document.getElementById("auth-error-msg");
      const submitBtn = document.getElementById("auth-submit-btn");
      const val = input.value.trim();

      errorBox.style.display = "none";
      input.classList.remove("shake");

      if (!val) {{
        showModalError(t.modal_err_empty);
        return;
      }}

      const authType = (pendingAction === "server_download") ? "server_files" : "backups";
      
      submitBtn.disabled = true;
      submitBtn.innerText = t.modal_verifying;

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
          showModalError(data.error || t.modal_err_wrong);
          input.classList.add("shake");
          input.select();
        }}
      }} catch (err) {{
        showModalError(t.modal_err_network);
      }} finally {{
        submitBtn.disabled = false;
        submitBtn.innerText = t.modal_unlock;
      }}
    }}

    function showModalError(msg) {{
      const errorBox = document.getElementById("auth-error-msg");
      errorBox.innerHTML = `<span>⚠️ ${{msg}}</span>`;
      errorBox.style.display = "flex";
    }}

    function applyUnlockedServerUI() {{
      const t = i18nData[currentLang] || i18nData.ru;
      const card = document.getElementById("server-card");
      const iconBox = document.getElementById("server-icon-box");
      const btn = document.getElementById("server-btn");
      const btnText = document.getElementById("server-btn-text");
      const btnIcon = document.getElementById("server-btn-icon");

      if (card) card.classList.add("unlocked");
      if (iconBox) iconBox.innerHTML = "🔓";
      if (btn) {{
        btn.className = "card-btn btn-unlocked";
      }}
      if (btnText) {{
        btnText.setAttribute("data-unlocked", "true");
        btnText.innerText = t.card_server_btn_unlocked;
      }}
      if (btnIcon) btnIcon.innerText = "⬇️";
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
    async function pollServerStatus() {{
      try {{
        const res = await fetch("/server_info.json");
        if (!res.ok) return;
        const info = await res.json();
        let st = (info.status || "offline").toLowerCase();
        if (st === "stopped") st = "offline";
        const rawIp = info.raw_ip || info.ip || "";
        const ip = (st === "online") ? rawIp : "";
        const port = info.port || 16261;
        const priceHr = info.price_per_hour;
        const playersCount = (info.players_count !== undefined) ? info.players_count : (info.players || 0);

        if (st !== lastKnownStatus || ip !== lastKnownIp || playersCount !== lastKnownPlayers) {{
          lastKnownStatus = st;
          lastKnownIp = ip;
          lastKnownPort = port;
          lastKnownPrice = priceHr;
          lastKnownPlayers = playersCount;
          updateStatusUI(st, ip, port, priceHr, playersCount);
        }}
      }} catch (e) {{}}
    }}

    function updateStatusUI(st, ip, port, priceHr, playersCount) {{
      const t = i18nData[currentLang] || i18nData.ru;
      const badge = document.getElementById("status-badge-container");
      const priceBadge = document.getElementById("price-badge-container");
      const priceText = document.getElementById("price-badge-text");
      const playersBadge = document.getElementById("players-badge-container");
      const playersText = document.getElementById("players-count-text");
      const widget = document.getElementById("address-widget-container");
      if (!badge || !widget) return;

      const isOnline = (st === "online" && ip && ip !== "pending");
      const isBooting = (st === "booting");
      const isStopping = (st === "stopping");

      if (isOnline) {{
        badge.className = "status-badge badge-online";
        badge.innerHTML = `<span class="status-dot online"></span><span id="status-badge-text">${{t.status_online}}</span>`;
      }} else if (isBooting) {{
        badge.className = "status-badge badge-booting";
        badge.innerHTML = `<span class="status-dot booting"></span><span id="status-badge-text">${{t.status_booting}}</span>`;
      }} else if (isStopping) {{
        badge.className = "status-badge badge-stopping";
        badge.innerHTML = `<span class="status-dot stopping"></span><span id="status-badge-text">${{t.status_stopping}}</span>`;
      }} else {{
        badge.className = "status-badge badge-offline";
        badge.innerHTML = `<span class="status-dot offline"></span><span id="status-badge-text">${{t.status_offline}}</span>`;
      }}

      if (playersBadge) {{
        if (isOnline) {{
          playersBadge.style.display = "inline-flex";
          if (playersText) {{
            const pCount = (playersCount !== undefined) ? parseInt(playersCount) : 0;
            playersText.textContent = formatPlayersCount(pCount, currentLang);
          }}
        }} else {{
          playersBadge.style.display = "none";
        }}
      }}

      if (priceBadge) {{
        if (isOnline || isBooting || isStopping) {{
          priceBadge.style.display = "inline-flex";
          if (priceHr !== undefined && priceText) {{
            const p = parseFloat(priceHr);
            priceText.textContent = (p < 0.1 ? `$${{p.toFixed(3)}}/hr` : `$${{p.toFixed(2)}}/hr`);
          }}
        }} else {{
          priceBadge.style.display = "none";
        }}
      }}

      if (isOnline) {{
        widget.innerHTML = `
          <div class="address-grid">
            <div class="address-card">
              <div class="address-label" id="lbl-server-ip">${{t.lbl_ip}}</div>
              <div class="address-value-row">
                <span class="address-text" id="ip-val">${{ip}}</span>
              </div>
            </div>
            <div class="address-card">
              <div class="address-label" id="lbl-port">${{t.lbl_port}}</div>
              <div class="address-value-row">
                <span class="address-text" id="port-val">${{port}}</span>
              </div>
            </div>
            <div class="address-card">
              <div class="address-label" id="lbl-password">${{t.lbl_password}}</div>
              <div class="address-value-row">
                <span class="address-text" id="pwd-val" style="color:#fbbf24;">1488</span>
              </div>
            </div>
          </div>
        `;
      }} else if (isBooting) {{
        widget.innerHTML = `
          <div class="status-banner booting-banner">
            <div class="status-banner-icon">🚀</div>
            <div class="status-banner-content">
              <div class="status-banner-title">${{t.banner_booting_title}}</div>
              <div class="status-banner-desc">${{t.banner_booting_desc}}</div>
            </div>
          </div>
        `;
      }} else if (isStopping) {{
        widget.innerHTML = `
          <div class="status-banner stopping-banner">
            <div class="status-banner-icon">🛑</div>
            <div class="status-banner-content">
              <div class="status-banner-title">${{t.banner_stopping_title}}</div>
              <div class="status-banner-desc">${{t.banner_stopping_desc}}</div>
            </div>
          </div>
        `;
      }} else {{
        widget.innerHTML = `
          <div class="status-banner offline-banner">
            <div class="status-banner-icon">⏸️</div>
            <div class="status-banner-content">
              <div class="status-banner-title">${{t.banner_offline_title}}</div>
              <div class="status-banner-desc">${{t.banner_offline_desc}}</div>
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
    status = server_info.get("status", "offline").lower()
    if status == "stopped":
        status = "offline"
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
              ⬇️ <span class="i18n-download">Скачать</span>
            </a>
          </td>
        </tr>
        """)

    wrong_pwd_html = '<div style="background:rgba(239,68,68,0.15); border:1px solid rgba(239,68,68,0.35); color:#fca5a5; padding:0.6rem 1rem; border-radius:8px; font-size:0.85rem; margin:0 auto 1.25rem; max-width:320px; font-weight:500;">❌ <span class="i18n-wrong-pwd">Неверный пароль бэкапов. Доступ запрещен.</span></div>' if (token and not is_authed) else ''

    # Archive count with Russian pluralization
    n_a = abs(len(backups)) % 100
    n1_a = n_a % 10
    archive_word = "архивов" if 10 < n_a < 20 else ("архива" if 1 < n1_a < 5 else ("архив" if n1_a == 1 else "архивов"))
    archives_count_text = f"{len(backups)} {archive_word}"

    table_html = f"""
    <table style="width:100%; border-collapse:collapse; margin-top:1rem;">
      <thead>
        <tr style="border-bottom:1px solid rgba(255,255,255,0.1); text-align:left; font-size:0.75rem; color:var(--text-muted); letter-spacing:0.05em;">
          <th style="padding:0.75rem 0.5rem;" class="i18n-th-name">ИМЯ АРХИВА</th>
          <th style="padding:0.75rem 0.5rem;" class="i18n-th-date">ДАТА СОЗДАНИЯ</th>
          <th style="padding:0.75rem 0.5rem;" class="i18n-th-size">РАЗМЕР</th>
          <th style="padding:0.75rem 0.5rem; text-align:right;" class="i18n-th-action">ДЕЙСТВИЕ</th>
        </tr>
      </thead>
      <tbody>
        {''.join(rows) if rows else '<tr><td colspan="4" style="text-align:center; padding:2rem; color:var(--text-muted);" class="i18n-no-backups">Архивы бэкапов не найдены в /data/backups/</td></tr>'}
      </tbody>
    </table>
    """ if is_authed else f"""
    <div style="text-align:center; padding:3rem 1rem; background:rgba(0,0,0,0.3); border-radius:12px; margin-top:1rem;">
      <div style="font-size:2.5rem; margin-bottom:0.75rem;">🔒</div>
      <h3 style="margin:0 0 0.5rem 0;" class="i18n-pwd-required">Требуется пароль бэкапов</h3>
      <p style="color:var(--text-muted); font-size:0.85rem; margin-bottom:1.25rem;" class="i18n-pwd-desc">Бэкапы и архивы мира защищены паролем.</p>
      {wrong_pwd_html}
      <form action="/backups" method="GET" style="display:inline-flex; gap:0.5rem;" onsubmit="saveBackupToken(this)">
        <input type="password" name="token" id="backup-pwd-input" placeholder="Пароль бэкапов..." style="background:#020617; border:1px solid rgba(255,255,255,0.1); color:white; padding:0.6rem 1rem; border-radius:8px; font-size:0.9rem;" required />
        <button type="submit" class="copy-btn" style="background:#3b82f6; border:none; padding:0.6rem 1.25rem;"><span class="i18n-unlock-btn">Разблокировать</span></button>
      </form>
    </div>
    """

    return f"""<!DOCTYPE html>
<html lang="ru">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>Всрания • Бэкапы</title>
  <style>
    :root {{
      --bg: #07090e;
      --card-bg: rgba(15, 23, 42, 0.75);
      --card-border: rgba(255, 255, 255, 0.08);
      --text: #f8fafc;
      --text-muted: #94a3b8;
      --primary: #3b82f6;
      --primary-glow: rgba(59, 130, 246, 0.3);
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
      flex-wrap: wrap;
      gap: 1rem;
    }}
    .brand-title {{
      display: flex;
      align-items: center;
      gap: 0.75rem;
      font-size: 1.35rem;
      font-weight: 700;
    }}
    .nav-links {{
      display: flex;
      align-items: center;
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
    .nav-item:hover, .nav-item.active {{
      color: white;
      background: rgba(255, 255, 255, 0.05);
      border-color: var(--card-border);
    }}
    .lang-switch {{
      display: inline-flex;
      align-items: center;
      background: rgba(255, 255, 255, 0.05);
      border: 1px solid var(--card-border);
      border-radius: 8px;
      padding: 0.2rem 0.35rem;
      gap: 0.25rem;
      margin-left: 0.5rem;
    }}
    .lang-btn {{
      background: transparent;
      border: none;
      color: var(--text-muted);
      font-size: 0.78rem;
      font-weight: 700;
      padding: 0.25rem 0.5rem;
      border-radius: 6px;
      cursor: pointer;
      transition: all 0.15s ease;
    }}
    .lang-btn.active {{
      background: var(--primary);
      color: white;
      box-shadow: 0 0 10px var(--primary-glow);
    }}
    .lang-btn:hover:not(.active) {{
      color: white;
      background: rgba(255, 255, 255, 0.08);
    }}
    .lang-divider {{
      color: rgba(255, 255, 255, 0.2);
      font-size: 0.75rem;
      user-select: none;
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
      <div class="brand-title">
        <span>☣️</span>
        <span id="brand-title-text">Всрания • Хаб</span>
      </div>
      <div class="nav-links">
        <a href="/" class="nav-item" id="nav-packages-link">Пакеты</a>
        <a href="/backups{f'?token={token}' if token else ''}" class="nav-item active" id="nav-backups-link">Бэкапы 🔒</a>
        <div class="lang-switch">
          <button type="button" class="lang-btn active" id="lang-btn-ru" onclick="setLanguage('ru')">RU</button>
          <span class="lang-divider">|</span>
          <button type="button" class="lang-btn" id="lang-btn-en" onclick="setLanguage('en')">EN</button>
        </div>
      </div>
    </nav>

    <div class="card">
      <div style="display:flex; justify-content:space-between; align-items:center; flex-wrap:wrap; gap:1rem; margin-bottom:1rem;">
        <div>
          <h1 style="font-size:1.4rem; margin:0 0 0.25rem 0;" id="backups-page-title">🗄️ Бэкапы игрового мира</h1>
          <p style="color:var(--text-muted); font-size:0.85rem; margin:0;" id="backups-page-desc">Автоматические и ручные снимки, хранящиеся на контроллере</p>
        </div>
        <span style="background:rgba(255,255,255,0.05); padding:0.35rem 0.75rem; border-radius:8px; font-size:0.8rem; color:var(--text-muted);" id="backups-count-badge">
          {archives_count_text}
        </span>
      </div>
      {table_html}
    </div>

    {f'''
    <div class="card">
      <h2 style="font-size:1.15rem; margin:0 0 0.5rem 0;" id="upload-title">⬆️ Загрузить архив бэкапа</h2>
      <p style="color:var(--text-muted); font-size:0.85rem;" id="upload-desc">Загрузите существующий архив сохранения мира <code>.zip</code> на контроллер:</p>
      <form action="/upload{f"?token={token}" if token else ""}" method="POST" enctype="multipart/form-data" style="margin-top:1rem;">
        <input type="file" name="file" accept=".zip" style="color:var(--text-muted); margin-bottom:1rem; display:block;" required />
        <button type="submit" class="copy-btn" style="background:#3b82f6; border:none; padding:0.6rem 1.25rem;" id="upload-btn">Загрузить .zip</button>
      </form>
    </div>
    ''' if is_authed else ''}
  </div>
  <script>
    const isAuthed = {"true" if is_authed else "false"};
    const currentToken = "{token}";
    const backupsCount = {len(backups)};
    let currentLang = localStorage.getItem("pz_lang") || "ru";

    const i18n = {{
      ru: {{
        page_title: "Всрания • Бэкапы",
        brand: "Всрания • Хаб",
        nav_packages: "Пакеты",
        nav_backups: "Бэкапы 🔒",
        backups_title: "🗄️ Бэкапы игрового мира",
        backups_desc: "Автоматические и ручные снимки, хранящиеся на контроллере",
        th_name: "ИМЯ АРХИВА",
        th_date: "ДАТА СОЗДАНИЯ",
        th_size: "РАЗМЕР",
        th_action: "ДЕЙСТВИЕ",
        download: "Скачать",
        no_backups: "Архивы бэкапов не найдены в /data/backups/",
        pwd_required: "Требуется пароль бэкапов",
        pwd_desc: "Бэкапы и архивы мира защищены паролем.",
        wrong_pwd: "Неверный пароль бэкапов. Доступ запрещен.",
        unlock: "Разблокировать",
        upload_title: "⬆️ Загрузить архив бэкапа",
        upload_desc: "Загрузите существующий архив сохранения мира .zip на контроллер:",
        upload_btn: "Загрузить .zip",
      }},
      en: {{
        page_title: "Vsrania • Backups",
        brand: "Vsrania • Hub",
        nav_packages: "Packages",
        nav_backups: "Backups 🔒",
        backups_title: "🗄️ World Save Backups",
        backups_desc: "Automated and manual snapshots stored on the Controller",
        th_name: "ARCHIVE NAME",
        th_date: "CREATION DATE",
        th_size: "SIZE",
        th_action: "ACTION",
        download: "Download",
        no_backups: "No backup archives found in /data/backups/",
        pwd_required: "Backups Password Required",
        pwd_desc: "Server backups and world archives are protected.",
        wrong_pwd: "Incorrect backups password. Access denied.",
        unlock: "Unlock",
        upload_title: "⬆️ Upload Backup Archive",
        upload_desc: "Upload an existing world save .zip into the Controller:",
        upload_btn: "Upload .zip",
      }}
    }};

    function formatArchivesCount(count, lang) {{
      if (lang === "en") return `${{count}} archive(s)`;
      const n = Math.abs(count) % 100;
      const n1 = n % 10;
      if (n > 10 && n < 20) return `${{count}} архивов`;
      if (n1 > 1 && n1 < 5) return `${{count}} архива`;
      if (n1 === 1) return `${{count}} архив`;
      return `${{count}} архивов`;
    }}

    function setLanguage(lang) {{
      currentLang = lang;
      localStorage.setItem("pz_lang", lang);
      const t = i18n[lang] || i18n.ru;

      document.title = t.page_title;

      const btnRu = document.getElementById("lang-btn-ru");
      const btnEn = document.getElementById("lang-btn-en");
      if (btnRu && btnEn) {{
        btnRu.className = "lang-btn" + (lang === "ru" ? " active" : "");
        btnEn.className = "lang-btn" + (lang === "en" ? " active" : "");
      }}

      const brandEl = document.getElementById("brand-title-text");
      if (brandEl) brandEl.textContent = t.brand;
      const navPkg = document.getElementById("nav-packages-link");
      if (navPkg) navPkg.textContent = t.nav_packages;
      const navBkp = document.getElementById("nav-backups-link");
      if (navBkp) navBkp.textContent = t.nav_backups;

      const bTitle = document.getElementById("backups-page-title");
      if (bTitle) bTitle.textContent = t.backups_title;
      const bDesc = document.getElementById("backups-page-desc");
      if (bDesc) bDesc.textContent = t.backups_desc;
      const bCount = document.getElementById("backups-count-badge");
      if (bCount) bCount.textContent = formatArchivesCount(backupsCount, lang);

      document.querySelectorAll(".i18n-th-name").forEach(el => el.textContent = t.th_name);
      document.querySelectorAll(".i18n-th-date").forEach(el => el.textContent = t.th_date);
      document.querySelectorAll(".i18n-th-size").forEach(el => el.textContent = t.th_size);
      document.querySelectorAll(".i18n-th-action").forEach(el => el.textContent = t.th_action);
      document.querySelectorAll(".i18n-download").forEach(el => el.textContent = t.download);
      document.querySelectorAll(".i18n-no-backups").forEach(el => el.textContent = t.no_backups);
      document.querySelectorAll(".i18n-pwd-required").forEach(el => el.textContent = t.pwd_required);
      document.querySelectorAll(".i18n-pwd-desc").forEach(el => el.textContent = t.pwd_desc);
      document.querySelectorAll(".i18n-wrong-pwd").forEach(el => el.textContent = t.wrong_pwd);
      document.querySelectorAll(".i18n-unlock-btn").forEach(el => el.textContent = t.unlock);

      const uploadTitle = document.getElementById("upload-title");
      if (uploadTitle) uploadTitle.textContent = t.upload_title;
      const uploadDesc = document.getElementById("upload-desc");
      if (uploadDesc) uploadDesc.textContent = t.upload_desc;
      const uploadBtn = document.getElementById("upload-btn");
      if (uploadBtn) uploadBtn.textContent = t.upload_btn;
    }}

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

    // Initialize language
    setLanguage(currentLang);
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

        # 1b. Webhook GET Info
        if path == "/webhook":
            body = json.dumps({"ok": True, "service": "pz-controller-webhook", "method": "POST required"}).encode("utf-8")
            self._send_response_headers(200, "application/json", len(body))
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

        # 0. GitHub Webhook Listener: POST /webhook
        if path == "/webhook":
            length = int(self.headers.get("Content-Length", 0))
            body = self.rfile.read(length) if length else b""

            if WEBHOOK_SECRET:
                sig = self.headers.get("X-Hub-Signature-256", "")
                mac = hmac.new(WEBHOOK_SECRET.encode(), body, hashlib.sha256).hexdigest()
                if not hmac.compare_digest(sig, "sha256=" + mac):
                    log("[webhook] POST /webhook rejected: invalid signature")
                    self._send_error(403, "Invalid webhook signature")
                    return

            try:
                os.makedirs(os.path.dirname(WEBHOOK_LOG), exist_ok=True)
                subprocess.Popen(
                    ["bash", "-c", "%s 2>&1 | tee -a %s" % (shlex.quote(WEBHOOK_TRIGGER), shlex.quote(WEBHOOK_LOG))]
                )
                log("[webhook] POST /webhook accepted, trigger spawned.")
                resp = json.dumps({"ok": True, "message": "Trigger spawned"}).encode("utf-8")
                self._send_response_headers(200, "application/json", len(resp))
                self.wfile.write(resp)
            except OSError as e:
                log(f"[webhook] Trigger spawn failed: {e}")
                self._send_error(500, f"Trigger spawn failed: {e}")
            return

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
