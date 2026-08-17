#!/usr/bin/env python3
"""
update_cloudflare.py — Automatically update Cloudflare Dynamic Redirect Rules for PZ Controller.

Redirects:
  - http(s)://yourdomain.com -> http://<akash-provider>:<storage-port>
  - http(s)://www.yourdomain.com -> http://<akash-provider>:<storage-port>

Env vars:
  CLOUDFLARE_API_TOKEN   Cloudflare API Token with Zone.Rulesets/DNS Edit permissions
  CLOUDFLARE_ZONE_ID     (Optional) Zone ID for the domain. Auto-detected if omitted.
  CLOUDFLARE_DOMAIN      (Optional) Domain name (e.g. vsrania.online). Auto-detected if omitted.
"""

import json
import os
import sys
import urllib.request
import urllib.error

CF_API_BASE = "https://api.cloudflare.com/client/v4"


def log(msg: str):
    print(f"[cloudflare] {msg}", flush=True)


def cf_request(endpoint: str, token: str, method: str = "GET", payload: dict = None):
    url = f"{CF_API_BASE}/{endpoint.lstrip('/')}"
    headers = {
        "Authorization": f"Bearer {token}",
        "Content-Type": "application/json"
    }
    data = json.dumps(payload).encode("utf-8") if payload is not None else None
    req = urllib.request.Request(url, headers=headers, data=data, method=method)
    with urllib.request.urlopen(req, timeout=15) as resp:
        return json.loads(resp.read().decode("utf-8"))


def get_first_zone(token: str):
    """Retrieve first active zone and domain name."""
    res = cf_request("/zones", token)
    zones = res.get("result", [])
    if not zones:
        raise RuntimeError("No zones found for this Cloudflare API token.")
    return zones[0]["id"], zones[0]["name"]


def update_redirect(target_url: str):
    token = os.environ.get("CLOUDFLARE_API_TOKEN", "").strip()
    if not token:
        log("CLOUDFLARE_API_TOKEN not set — skipping Cloudflare redirect update.")
        return False

    zone_id = os.environ.get("CLOUDFLARE_ZONE_ID", "").strip()
    domain = os.environ.get("CLOUDFLARE_DOMAIN", "").strip()

    try:
        if not zone_id or not domain:
            detected_id, detected_name = get_first_zone(token)
            zone_id = zone_id or detected_id
            domain = domain or detected_name

        log(f"Configuring redirect on zone {domain} (ID: {zone_id}) -> {target_url} ...")

        # Build expression matching root domain and www subdomain
        expression = f'(http.host eq "{domain}" or http.host eq "www.{domain}")'

        payload = {
            "rules": [
                {
                    "description": f"PZ Controller Dynamic Redirect ({domain})",
                    "expression": expression,
                    "action": "redirect",
                    "action_parameters": {
                        "from_value": {
                            "status_code": 302,
                            "target_url": {
                                "value": target_url
                            },
                            "preserve_query_string": True
                        }
                    },
                    "enabled": True
                }
            ]
        }

        res = cf_request(
            f"/zones/{zone_id}/rulesets/phases/http_request_dynamic_redirect/entrypoint",
            token,
            method="PUT",
            payload=payload
        )

        if res.get("success"):
            log(f"SUCCESS: Cloudflare redirect active! http://{domain} -> {target_url}")
            return True
        else:
            log(f"Failed to update ruleset: {res.get('errors')}")
            return False

    except urllib.error.HTTPError as e:
        err_body = e.read().decode("utf-8", errors="ignore")
        log(f"Cloudflare HTTP Error {e.code}: {err_body}")
        return False
    except Exception as e:
        log(f"Error updating Cloudflare redirect: {e}")
        return False


if __name__ == "__main__":
    if len(sys.argv) < 2 or not sys.argv[1].strip():
        print("Usage: update_cloudflare.py <target_storage_url>")
        sys.exit(1)

    target_url = sys.argv[1].strip()
    success = update_redirect(target_url)
    sys.exit(0 if success else 1)
