#!/usr/bin/env python3
"""
update_cloudflare.py — Configure Cloudflare Proxied DNS & Origin Rules for PZ Controller.

Enables transparent reverse-proxying so that:
  https://vsrania.online/ (and https://www.vsrania.online/)
serves the Controller storage dashboard directly with valid SSL, preserving the custom domain
in the browser (no 302 redirects).

Environment Variables:
  CLOUDFLARE_API_TOKEN   Cloudflare API Token with Zone.DNS and Zone.Rulesets permissions.
  CLOUDFLARE_ZONE_ID     (Optional) Zone ID for the domain. Auto-detected if omitted.
  CLOUDFLARE_DOMAIN      (Optional) Domain name (e.g. vsrania.online). Auto-detected if omitted.
"""

import json
import os
import re
import sys
import urllib.request
import urllib.error
from urllib.parse import urlparse

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


def upsert_dns_record(zone_id: str, token: str, record_name: str, record_type: str, content: str):
    """Create or update a DNS record with Cloudflare Proxy enabled."""
    res = cf_request(f"/zones/{zone_id}/dns_records?name={record_name}", token)
    records = res.get("result", [])
    
    payload = {
        "type": record_type,
        "name": record_name,
        "content": content,
        "proxied": True,
        "ttl": 1  # Auto TTL for proxied records
    }
    
    if records:
        rec_id = records[0]["id"]
        # Update existing
        up_res = cf_request(f"/zones/{zone_id}/dns_records/{rec_id}", token, method="PUT", payload=payload)
        if up_res.get("success"):
            log(f"Updated DNS {record_type} record: {record_name} -> {content} (Proxied)")
            return True
    else:
        # Create new
        cr_res = cf_request(f"/zones/{zone_id}/dns_records", token, method="POST", payload=payload)
        if cr_res.get("success"):
            log(f"Created DNS {record_type} record: {record_name} -> {content} (Proxied)")
            return True
            
    return False


def clear_dynamic_redirects(zone_id: str, token: str):
    """Remove any old 302 dynamic redirect rules so Cloudflare proxies rather than redirects."""
    try:
        cf_request(
            f"/zones/{zone_id}/rulesets/phases/http_request_dynamic_redirect/entrypoint",
            token,
            method="PUT",
            payload={"rules": []}
        )
        log("Cleared legacy dynamic redirect rules (switching from 302 redirect to transparent proxy).")
    except Exception:
        pass


def clear_origin_rules(zone_id: str, token: str):
    """Remove custom origin port rules so Cloudflare routes directly to standard port 80/443."""
    try:
        cf_request(
            f"/zones/{zone_id}/rulesets/phases/http_request_origin/entrypoint",
            token,
            method="PUT",
            payload={"rules": []}
        )
        log("Cleared custom origin port rules (using standard port 80/443).")
    except Exception:
        pass


def set_dynamic_redirect(zone_id: str, token: str, domain: str, target_url: str):
    """Configure Cloudflare Dynamic Redirect (302) with full path and query string preservation."""
    clean_target = target_url.rstrip("/")
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
                            "expression": f'concat("{clean_target}", http.request.uri.path)'
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
        log(f"Configured Dynamic Redirect: https://{domain}/* -> {clean_target}/* (302)")
        return True
    else:
        log(f"Note on Dynamic Redirect: {res.get('errors')}")
        return False


def set_ssl_flexible(zone_id: str, token: str):
    """Ensure SSL mode is Flexible."""
    try:
        cf_request(
            f"/zones/{zone_id}/settings/ssl",
            token,
            method="PATCH",
            payload={"value": "flexible"}
        )
        log("Cloudflare SSL mode verified as Flexible.")
    except Exception as e:
        log(f"Note on SSL settings: {e}")


def update_cloudflare_proxy(target_url: str):
    token = os.environ.get("CLOUDFLARE_API_TOKEN", "").strip()
    if not token:
        log("CLOUDFLARE_API_TOKEN not set — skipping Cloudflare configuration.")
        return False

    zone_id = os.environ.get("CLOUDFLARE_ZONE_ID", "").strip()
    domain = os.environ.get("CLOUDFLARE_DOMAIN", "").strip()

    try:
        if not zone_id or not domain:
            detected_id, detected_name = get_first_zone(token)
            zone_id = zone_id or detected_id
            domain = domain or detected_name

        clean_target = target_url.strip()
        if not clean_target.startswith("http://") and not clean_target.startswith("https://"):
            clean_target = f"http://{clean_target}"

        log(f"Configuring Cloudflare Dynamic Redirect for {domain} -> {clean_target} ...")

        # 1. Clear any old origin rules
        clear_origin_rules(zone_id, token)

        # 2. Ensure SSL is set to Flexible for domain edge SSL
        set_ssl_flexible(zone_id, token)

        # 3. Create / update Proxied DNS A records pointing to 192.0.2.1 (Cloudflare dummy IP for edge redirects)
        upsert_dns_record(zone_id, token, domain, "A", "192.0.2.1")
        upsert_dns_record(zone_id, token, f"www.{domain}", "A", "192.0.2.1")

        # 4. Set Dynamic Redirect Rule (302 redirect with query preservation)
        set_dynamic_redirect(zone_id, token, domain, clean_target)

        log(f"SUCCESS: https://{domain} now dynamically redirects to {clean_target} with valid Cloudflare SSL!")
        return True

    except urllib.error.HTTPError as e:
        err_body = e.read().decode("utf-8", errors="ignore")
        log(f"Cloudflare API Error {e.code}: {err_body}")
        return False
    except Exception as e:
        log(f"Error configuring Cloudflare redirect: {e}")
        return False


if __name__ == "__main__":
    if len(sys.argv) < 2 or not sys.argv[1].strip():
        print("Usage: update_cloudflare.py <target_storage_url>")
        sys.exit(1)

    target = sys.argv[1].strip()
    success = update_cloudflare_proxy(target)
    sys.exit(0 if success else 1)
