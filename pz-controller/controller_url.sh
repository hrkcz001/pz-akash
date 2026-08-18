#!/bin/bash
# =============================================================================
# controller_url.sh — resolve Controller's public storage & webhook URLs on Akash.
#
# Usage:
#   controller_url.sh [port]   (default: 8000)
# =============================================================================

set -uo pipefail

API_BASE="${API_BASE:-https://console-api.akash.network}"
AUTOSAVE_SERVICE_NAMES="${AUTOSAVE_SERVICE_NAMES:-controller pz-controller autosave pz-autosave autosaver}"
TARGET_PORT="${1:-8000}"

[ -n "${AKASH_API_KEY:-}" ] || { echo "AKASH_API_KEY is not set" >&2; exit 1; }

api() { curl -sS --max-time 20 "$API_BASE$1" -H "x-api-key: $AKASH_API_KEY" 2>/dev/null; }

LIST=$(api "/v1/deployments?limit=1000")

for dseq in $(echo "$LIST" | jq -r '.data.deployments[]?.deployment.id.dseq // empty' 2>/dev/null); do
  DEP_JSON=$(mktemp)
  api "/v1/deployments/$dseq" > "$DEP_JSON"
  URL=$(python3 - "$AUTOSAVE_SERVICE_NAMES" "$TARGET_PORT" "$DEP_JSON" <<'PYEOF'
import json, sys
names = set(sys.argv[1].split())
target_port = str(sys.argv[2])
try:
    d = json.load(open(sys.argv[3]))
except Exception:
    sys.exit(0)
leases = (d.get("data") or {}).get("leases") or []
for l in leases:
    if l.get("state") != "active":
        continue
    st = l.get("status") or {}
    svc = st.get("services") or {}
    for name in names:
        if name not in svc:
            continue
        # 1. Ingress URIs (port 80 HTTP ingress)
        uris = (svc.get(name) or {}).get("uris") or []
        for u in uris:
            if u:
                print(f"http://{u}")
                sys.exit(0)
        # 2. IP lease
        for arr in (st.get("ips") or {}).values():
            for e in arr or []:
                ip = e.get("ip") or e.get("IP")
                if ip:
                    print(f"http://{ip}:{target_port}")
                    sys.exit(0)
        # 3. Shared endpoint (forwarded ports)
        fp = (st.get("forwarded_ports") or {}).get(name) or []
        match = None
        for e in fp:
            if str(e.get("port")) == target_port and e.get("host"):
                match = e
                break
        if match is None:
            match = next((e for e in fp if e.get("host")), None)
        if match:
            ext = match.get("external_port") or match.get("externalPort") or target_port
            print(f"http://{match['host']}:{ext}")
            sys.exit(0)
PYEOF
)
  rm -f "$DEP_JSON"
  if [ -n "$URL" ]; then
    echo "$URL"
    exit 0
  fi
done

exit 1
