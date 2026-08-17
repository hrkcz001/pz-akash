#!/bin/bash
# =============================================================================
# webhook_url.sh — print the autosaver's own public webhook URL.
#
# Finds the autosaver's deployment among the account's deployments by matching
# the service name in the lease status (set via AUTOSAVE_SERVICE_NAMES), then
# resolves the public address: IP lease -> http://<ip>:<WEBHOOK_PORT>/webhook,
# shared endpoint -> http://<host>:<external_port>/webhook.
#
# Run it inside the autosaver container (or exec via Akash Console shell), or
# read the value the autosaver logs at startup.
# =============================================================================

set -uo pipefail

API_BASE="${API_BASE:-https://console-api.akash.network}"
AUTOSAVE_SERVICE_NAMES="${AUTOSAVE_SERVICE_NAMES:-controller pz-controller autosave pz-autosave autosaver}"
WEBHOOK_PORT="${WEBHOOK_PORT:-8080}"

[ -n "${AKASH_API_KEY:-}" ] || { echo "AKASH_API_KEY is not set" >&2; exit 1; }

api() { curl -sS --max-time 20 "$API_BASE$1" -H "x-api-key: $AKASH_API_KEY" 2>/dev/null; }

LIST=$(api "/v1/deployments?limit=1000")

for dseq in $(echo "$LIST" | jq -r '.data.deployments[]?.deployment.id.dseq // empty' 2>/dev/null); do
  DEP_JSON=$(mktemp)
  api "/v1/deployments/$dseq" > "$DEP_JSON"
  URL=$(python3 - "$AUTOSAVE_SERVICE_NAMES" "$WEBHOOK_PORT" "$DEP_JSON" <<'PYEOF'
import json, sys
names = set(sys.argv[1].split())
wh_port = sys.argv[2]
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
        # IP lease: dedicated IP, port is deterministic
        for arr in (st.get("ips") or {}).values():
            for e in arr or []:
                ip = e.get("ip") or e.get("IP")
                if ip:
                    print(f"http://{ip}:{wh_port}/webhook")
                    sys.exit(0)
        # shared endpoint: forwarded port is provider-assigned (host + externalPort).
        # Pick the entry whose container port is the webhook port — never fp[0]
        # (the service may also expose the upload server on another port).
        fp = (st.get("forwarded_ports") or {}).get(name) or []
        wh = None
        for e in fp:
            if str(e.get("port")) == str(wh_port) and e.get("host"):
                wh = e
                break
        if wh is None:
            wh = next((e for e in fp if e.get("host")), None)
        if wh:
            ext = wh.get("external_port") or wh.get("externalPort") or wh_port
            print(f"http://{wh['host']}:{ext}/webhook")
            sys.exit(0)
PYEOF
)
  rm -f "$DEP_JSON"
  if [ -n "$URL" ]; then
    echo "$URL"
    exit 0
  fi
done

echo "No active deployment with services in ($AUTOSAVE_SERVICE_NAMES) found — check the lease status in the Akash Console." >&2
exit 1
