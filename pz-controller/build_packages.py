#!/usr/bin/env python3
"""
build_packages.py — Controller build-time mod downloader & package builder.

Parses mods.json from common/, client/, and server/ folders in pz-saves,
downloads mods using SteamCMD, auto-configures server .ini files (Mods= & Map=),
and generates three distinct zip archives:
  - common.zip (shared files + common mods)
  - client.zip (client-specific files + client mods)
  - server.zip (server-specific files + server mods + configured .ini/.lua)
"""

import argparse
import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import zipfile
from pathlib import Path


def log(msg: str):
    print(f"[build_packages] {msg}", flush=True)


def parse_mod_info(mod_info_path: Path):
    """Extract id and name from mod.info file."""
    if not mod_info_path.is_file():
        return None
    try:
        content = mod_info_path.read_text(encoding="utf-8", errors="ignore")
    except Exception as e:
        log(f"Error reading {mod_info_path}: {e}")
        return None

    # Remove BOM
    content = content.lstrip("\ufeff")

    mod_id = None
    name = None
    for line in content.splitlines():
        line = line.strip()
        if not line or line.startswith(("#", "//", ";")):
            continue
        if re.match(r"^id\s*=", line, re.IGNORECASE):
            mod_id = re.sub(r"^id\s*=\s*", "", line, flags=re.IGNORECASE).strip()
        elif re.match(r"^modId\s*=", line, re.IGNORECASE) and not mod_id:
            mod_id = re.sub(r"^modId\s*=\s*", "", line, flags=re.IGNORECASE).strip()
        elif re.match(r"^name\s*=", line, re.IGNORECASE):
            name = re.sub(r"^name\s*=\s*", "", line, flags=re.IGNORECASE).strip()

    return {"id": mod_id, "name": name}


def find_mod_info(mod_dir: Path):
    """Find best mod.info prioritizing 42.x version subdirectories."""
    # Check 42.x version directories
    subdirs = [d for d in mod_dir.iterdir() if d.is_dir()]
    version_dirs = []
    for d in subdirs:
        m = re.match(r"^42(\.\d+)*$", d.name)
        if m:
            version_dirs.append(d)
    
    # Sort version dirs descending (e.g. 42.1 > 42.0 > 42)
    def version_key(path):
        parts = re.findall(r"\d+", path.name)
        return [int(p) for p in parts]

    version_dirs.sort(key=version_key, reverse=True)
    for vdir in version_dirs:
        info_file = vdir / "mod.info"
        if info_file.is_file():
            info = parse_mod_info(info_file)
            if info and info.get("id"):
                return info, vdir

    # Root mod.info
    root_info = mod_dir / "mod.info"
    if root_info.is_file():
        info = parse_mod_info(root_info)
        if info and info.get("id"):
            return info, mod_dir

    # common/mod.info
    common_info = mod_dir / "common" / "mod.info"
    if common_info.is_file():
        info = parse_mod_info(common_info)
        if info and info.get("id"):
            return info, mod_dir / "common"

    return {"id": mod_dir.name, "name": mod_dir.name}, mod_dir


def find_mod_maps(mod_dir: Path):
    """Scan for maps in common/media/maps or 42.x/media/maps."""
    maps = []
    candidates = [
        mod_dir / "common" / "media" / "maps",
        mod_dir / "media" / "maps"
    ]
    # Also check version subdirs
    for d in mod_dir.iterdir():
        if d.is_dir() and re.match(r"^42(\.\d+)*$", d.name):
            candidates.append(d / "common" / "media" / "maps")
            candidates.append(d / "media" / "maps")

    for c in candidates:
        if c.is_dir():
            for mdir in c.iterdir():
                if mdir.is_dir() and (mdir / "map.info").is_file() or mdir.is_dir():
                    maps.append(mdir.name)
    return list(dict.fromkeys(maps))


def download_workshop_mods(steamcmd_path: str, mod_ids: list, download_dir: Path):
    """Download Workshop items using SteamCMD."""
    if not mod_ids:
        return {}

    download_dir.mkdir(parents=True, exist_ok=True)
    script_path = download_dir / "download_script.txt"

    lines = [
        "@sSteamCmdForcePlatformType linux",
        f"force_install_dir {download_dir.as_posix()}",
        "login anonymous",
    ]
    for mid in mod_ids:
        lines.append(f"workshop_download_item 108600 {mid}")
    lines.append("quit\n")

    script_path.write_text("\n".join(lines), encoding="utf-8")

    log(f"Running SteamCMD to download {len(mod_ids)} workshop item(s)...")
    cmd = [steamcmd_path, "+runscript", str(script_path)]
    res = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    log(f"SteamCMD output:\n{res.stdout}")
    if res.returncode != 0:
        log(f"WARNING: SteamCMD exited with code {res.returncode}")

    # Process downloaded workshop content: steamapps/workshop/content/108600/<mod_id>/mods/*
    workshop_content = download_dir / "steamapps" / "workshop" / "content" / "108600"
    extracted_mods = {}

    if workshop_content.is_dir():
        for item_dir in workshop_content.iterdir():
            if not item_dir.is_dir():
                continue
            mods_subdir = item_dir / "mods"
            if mods_subdir.is_dir():
                for actual_mod in mods_subdir.iterdir():
                    if actual_mod.is_dir():
                        info, _ = find_mod_info(actual_mod)
                        extracted_mods[actual_mod.name] = {
                            "source_path": actual_mod,
                            "workshop_id": item_dir.name,
                            "mod_id": info.get("id") or actual_mod.name,
                            "mod_name": info.get("name") or actual_mod.name,
                            "maps": find_mod_maps(actual_mod)
                        }
            else:
                # Direct mod folder structure
                info, _ = find_mod_info(item_dir)
                extracted_mods[item_dir.name] = {
                    "source_path": item_dir,
                    "workshop_id": item_dir.name,
                    "mod_id": info.get("id") or item_dir.name,
                    "mod_name": info.get("name") or item_dir.name,
                    "maps": find_mod_maps(item_dir)
                }

    log(f"Successfully processed {len(extracted_mods)} mod(s) from workshop download.")
    return extracted_mods


def load_mods_json(folder: Path):
    """Load list of mod IDs from mods.json in the specified folder."""
    mods_json = folder / "mods.json"
    if not mods_json.is_file():
        return []
    try:
        data = json.loads(mods_json.read_text(encoding="utf-8"))
        if isinstance(data, list):
            return [str(m).strip() for m in data if str(m).strip()]
    except Exception as e:
        log(f"Failed to read {mods_json}: {e}")
    return []


def configure_server_ini(ini_path: Path, all_mod_ids: list, all_maps: list):
    """Update Mods= and Map= in a server .ini file."""
    if not ini_path.is_file():
        return

    content = ini_path.read_text(encoding="utf-8", errors="ignore")
    lines = content.splitlines()

    mods_line_idx = -1
    map_line_idx = -1
    existing_mods = []
    existing_maps = []

    for i, line in enumerate(lines):
        if line.startswith("Mods="):
            mods_line_idx = i
            raw_mods = line[len("Mods="):].strip()
            if raw_mods:
                existing_mods = [m.strip() for m in raw_mods.split(";") if m.strip()]
        elif line.startswith("Map="):
            map_line_idx = i
            raw_maps = line[len("Map="):].strip()
            if raw_maps:
                existing_maps = [m.strip() for m in raw_maps.split(";") if m.strip()]

    # Reconcile Mods: keep valid existing, append new
    final_mods = []
    seen_mods = set()
    installed_set = set(all_mod_ids)

    for m in existing_mods:
        if m in installed_set and m not in seen_mods:
            final_mods.append(m)
            seen_mods.add(m)
    for m in all_mod_ids:
        if m not in seen_mods:
            final_mods.append(m)
            seen_mods.add(m)

    # Reconcile Maps: mod maps first, vanilla Muldraugh, KY last
    final_maps = []
    seen_maps = set()
    installed_map_set = set(all_maps)

    for m in existing_maps:
        if m == "Muldraugh, KY":
            continue
        if m in installed_map_set and m not in seen_maps:
            final_maps.append(m)
            seen_maps.add(m)
    for m in all_maps:
        if m not in seen_maps and m != "Muldraugh, KY":
            final_maps.append(m)
            seen_maps.add(m)
    final_maps.append("Muldraugh, KY")

    new_mods_line = f"Mods={';'.join(final_mods)}"
    new_map_line = f"Map={';'.join(final_maps)}"

    if mods_line_idx >= 0:
        lines[mods_line_idx] = new_mods_line
    else:
        lines.append(new_mods_line)

    if map_line_idx >= 0:
        lines[map_line_idx] = new_map_line
    else:
        lines.append(new_map_line)

    ini_path.write_text("\n".join(lines) + "\n", encoding="utf-8")
    log(f"Auto-configured {ini_path.name}: {len(final_mods)} mods, {len(final_maps)} maps.")


def create_zip_archive(source_dir: Path, mods_dict: dict, zip_out_path: Path):
    """
    Package files from source_dir and mods into a clean zip archive.
    Inside the zip:
      - Files from source_dir are placed relative to root (excluding mods.json).
      - Mods are placed under mods/<mod_folder_name>/...
    """
    zip_out_path.parent.mkdir(parents=True, exist_ok=True)
    if zip_out_path.exists():
        zip_out_path.unlink()

    with zipfile.ZipFile(zip_out_path, "w", zipfile.ZIP_DEFLATED) as zf:
        # Add files from source folder
        if source_dir.is_dir():
            for root, dirs, files in os.walk(source_dir):
                # Exclude .git and pycache
                dirs[:] = [d for d in dirs if d not in (".git", "__pycache__", ".github")]
                for f in files:
                    if f in ("mods.json", ".gitignore"):
                        continue
                    file_path = Path(root) / f
                    arcname = file_path.relative_to(source_dir).as_posix()
                    zf.write(file_path, arcname)

        # Add mods
        for mod_folder_name, info in mods_dict.items():
            mod_src = info["source_path"]
            if mod_src.is_dir():
                for root, dirs, files in os.walk(mod_src):
                    for f in files:
                        file_path = Path(root) / f
                        rel_in_mod = file_path.relative_to(mod_src).as_posix()
                        arcname = f"mods/{mod_folder_name}/{rel_in_mod}"
                        zf.write(file_path, arcname)

    log(f"Created {zip_out_path.name} ({zip_out_path.stat().st_size / (1024*1024):.2f} MB)")


def sha256_file(path: Path):
    if not path.is_file():
        return ""
    h = hashlib.sha256()
    with open(path, "rb") as f:
        while chunk := f.read(65536):
            h.update(chunk)
    return h.hexdigest()


def main():
    parser = argparse.ArgumentParser(description="PZ Controller Package & Mod Builder")
    parser.add_argument("--repo-path", default="/root/pz-saves", help="Path to pz-saves repository")
    parser.add_argument("--output-dir", default="/data/packages", help="Output directory for zip packages")
    parser.add_argument("--steamcmd", default="/steamcmd/steamcmd.sh", help="Path to steamcmd.sh")
    parser.add_argument("--temp-dir", default="/tmp/steam_downloads", help="Temporary directory for workshop items")
    parser.add_argument("--server-name", default="vsrania", help="Server name for .ini files")
    args = parser.parse_args()

    repo_path = Path(args.repo_path)
    output_dir = Path(args.output_dir)
    temp_dir = Path(args.temp_dir)
    output_dir.mkdir(parents=True, exist_ok=True)

    common_dir = repo_path / "common"
    client_dir = repo_path / "client"
    server_dir = repo_path / "server"

    # 1. Read mods.json from each folder
    common_mod_ids = load_mods_json(common_dir)
    client_mod_ids = load_mods_json(client_dir)
    server_mod_ids = load_mods_json(server_dir)

    log(f"Discovered mod IDs: common={len(common_mod_ids)}, client={len(client_mod_ids)}, server={len(server_mod_ids)}")

    # 2. Download mods if SteamCMD is present
    common_mods = {}
    client_mods = {}
    server_mods = {}

    if Path(args.steamcmd).is_file():
        if common_mod_ids:
            log("Downloading common mods...")
            common_mods = download_workshop_mods(args.steamcmd, common_mod_ids, temp_dir / "common")
        if client_mod_ids:
            log("Downloading client mods...")
            client_mods = download_workshop_mods(args.steamcmd, client_mod_ids, temp_dir / "client")
        if server_mod_ids:
            log("Downloading server mods...")
            server_mods = download_workshop_mods(args.steamcmd, server_mod_ids, temp_dir / "server")
    else:
        log(f"SteamCMD not found at {args.steamcmd}, skipping workshop download.")

    # 3. Collect all server-relevant mods (common + server) for .ini auto-configuration
    all_server_mod_ids = []
    all_server_maps = []

    for info in list(common_mods.values()) + list(server_mods.values()):
        if info.get("mod_id"):
            all_server_mod_ids.append(info["mod_id"])
        all_server_maps.extend(info.get("maps", []))

    all_server_mod_ids = list(dict.fromkeys(all_server_mod_ids))
    all_server_maps = list(dict.fromkeys(all_server_maps))

    # Auto-configure server .ini files in server_dir (e.g. server/Server/*.ini or server/*.ini)
    if server_dir.is_dir():
        for ini_file in server_dir.glob("**/*.ini"):
            configure_server_ini(ini_file, all_server_mod_ids, all_server_maps)

    # 4. Generate the 3 zip archives
    common_zip = output_dir / "common.zip"
    client_zip = output_dir / "client.zip"
    server_zip = output_dir / "server.zip"

    log("Building common.zip...")
    create_zip_archive(common_dir, common_mods, common_zip)

    log("Building client.zip...")
    create_zip_archive(client_dir, client_mods, client_zip)

    log("Building server.zip...")
    create_zip_archive(server_dir, server_mods, server_zip)

    # 5. Generate package manifest
    manifest = {
        "common": {
            "file": "common.zip",
            "sha256": sha256_file(common_zip),
            "size": common_zip.stat().st_size if common_zip.exists() else 0,
            "mods_count": len(common_mods),
            "mod_ids": [m["mod_id"] for m in common_mods.values()]
        },
        "client": {
            "file": "client.zip",
            "sha256": sha256_file(client_zip),
            "size": client_zip.stat().st_size if client_zip.exists() else 0,
            "mods_count": len(client_mods),
            "mod_ids": [m["mod_id"] for m in client_mods.values()]
        },
        "server": {
            "file": "server.zip",
            "sha256": sha256_file(server_zip),
            "size": server_zip.stat().st_size if server_zip.exists() else 0,
            "mods_count": len(server_mods),
            "mod_ids": [m["mod_id"] for m in server_mods.values()]
        }
    }

    manifest_path = output_dir / "packages_manifest.json"
    manifest_path.write_text(json.dumps(manifest, indent=2), encoding="utf-8")
    log(f"Wrote {manifest_path.name}")
    log("Build packages completed successfully!")


if __name__ == "__main__":
    main()
