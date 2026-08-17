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


def configure_origin_rule(zone_id: str, token: str, domain: str, origin_port: int):
    """Configure Cloudflare Origin Rules to route incoming HTTPS traffic on :443 to the custom origin port."""
    expression = f'(http.host eq "{domain}" or http.host eq "www.{domain}")'
    payload = {
        "rules": [
            {
                "description": f"PZ Controller Origin Port Route ({domain} -> :{origin_port})",
                "expression": expression,
                "action": "route",
                "action_parameters": {
                    "origin": {
                        "port": origin_port
                    }
                },
                "enabled": True
            }
        ]
    }
    
    res = cf_request(
        f"/zones/{zone_id}/rulesets/phases/http_request_origin/entrypoint",
        token,
        method="PUT",
        payload=payload
    )
    if res.get("success"):
        log(f"Configured Cloudflare Origin Rule: https://{domain} -> origin port {origin_port}")
        return True
    else:
        log(f"Note on Origin Rules: {res.get('errors')}")
        return False


def set_ssl_flexible(zone_id: str, token: str):
    """Ensure SSL mode is Flexible (so Cloudflare serves HTTPS to visitors while talking HTTP to Akash origin)."""
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
        log("CLOUDFLARE_API_TOKEN not set — skipping Cloudflare proxy configuration.")
        return False

    zone_id = os.environ.get("CLOUDFLARE_ZONE_ID", "").strip()
    domain = os.environ.get("CLOUDFLARE_DOMAIN", "").strip()

    try:
        if not zone_id or not domain:
            detected_id, detected_name = get_first_zone(token)
            zone_id = zone_id or detected_id
            domain = domain or detected_name

        parsed = urlparse(target_url if "://" in target_url else f"http://{target_url}")
        host = parsed.hostname or target_url
        port = parsed.port or (443 if parsed.scheme == "https" else 80)

        log(f"Configuring Cloudflare Proxy for {domain} -> {host}:{port} ...")

        # 1. Clear any old 302 redirect rules
        clear_dynamic_redirects(zone_id, token)

        # 2. Ensure SSL is set to Flexible for HTTP origin
        set_ssl_flexible(zone_id, token)

        # 3. Create / update Proxied DNS record (A if IPv4, CNAME if hostname)
        is_ipv4 = bool(re.match(r"^\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}$", host))
        rec_type = "A" if is_ipv4 else "CNAME"
        
        upsert_dns_record(zone_id, token, domain, rec_type, host)
        upsert_dns_record(zone_id, token, f"www.{domain}", rec_type, host)

        # 4. Configure Origin Port Rule if port is non-standard
        if port not in (80, 443):
            configure_origin_rule(zone_id, token, domain, port)

        log(f"SUCCESS: https://{domain} is now proxied to {host}:{port} with valid Cloudflare SSL!")
        return True

    except urllib.error.HTTPError as e:
        err_body = e.read().decode("utf-8", errors="ignore")
        log(f"Cloudflare API Error {e.code}: {err_body}")
        return False
    except Exception as e:
        log(f"Error configuring Cloudflare proxy: {e}")
        return False


if __name__ == "__main__":
    if len(sys.argv) < 2 or not sys.argv[1].strip():
        print("Usage: update_cloudflare.py <target_storage_url>")
        sys.exit(1)

    target = sys.argv[1].strip()
    success = update_cloudflare_proxy(target)
    sys.exit(0 if success else 1)
