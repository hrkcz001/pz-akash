#!/usr/bin/env python3
"""
GitHub webhook listener for the autosaver.

POST /webhook  -> verifies X-Hub-Signature-256 (when WEBHOOK_SECRET is set),
                  then spawns /usr/local/bin/trigger.sh in the background
                  (git pull + consume start/backup/halt triggers).
GET  /healthz  -> 200 OK

Self-healing: the listener writes its PID to WEBHOOK_PID_FILE so the
autosaver loop can restart it when /healthz stops answering, and logs a
heartbeat every WEBHOOK_HEARTBEAT_SEC so a silently dead listener is visible
in the container logs.

Env:
  WEBHOOK_PORT            listen port (default 8080)
  WEBHOOK_SECRET          shared secret configured in the GitHub webhook
                          (optional, but strongly recommended)
  WEBHOOK_PID_FILE        pid file for the watchdog (default /data/webhook.pid)
  WEBHOOK_HEARTBEAT_SEC   heartbeat interval in seconds (default 600)
"""
import hashlib
import hmac
import os
import shlex
import subprocess
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(os.environ.get("WEBHOOK_PORT", "8080"))
SECRET = os.environ.get("WEBHOOK_SECRET", "")
TRIGGER = os.environ.get("WEBHOOK_TRIGGER", "/usr/local/bin/trigger.sh")
LOG = os.environ.get("WEBHOOK_LOG", "/data/webhook.log")
PID_FILE = os.environ.get("WEBHOOK_PID_FILE", "/data/webhook.pid")
HEARTBEAT_SEC = int(os.environ.get("WEBHOOK_HEARTBEAT_SEC", "600"))


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):  # silence request logging
        pass

    def _send(self, code, body=b""):
        self.send_response(code)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        if body:
            self.wfile.write(body)

    def do_GET(self):
        if self.path == "/healthz":
            self._send(200, b"ok")
        else:
            self._send(404, b"not found")

    def do_POST(self):
        if self.path != "/webhook":
            self._send(404, b"not found")
            return
        length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(length) if length else b""

        if SECRET:
            sig = self.headers.get("X-Hub-Signature-256", "")
            mac = hmac.new(SECRET.encode(), body, hashlib.sha256).hexdigest()
            if not hmac.compare_digest(sig, "sha256=" + mac):
                self._send(403, b"bad signature")
                return

        try:
            os.makedirs(os.path.dirname(LOG), exist_ok=True)
            # Trigger output goes BOTH to the container stdout (visible in the
            # Akash Console logs) and to the webhook log file.
            subprocess.Popen(
                ["bash", "-c", "%s 2>&1 | tee -a %s" % (shlex.quote(TRIGGER), shlex.quote(LOG))]
            )
        except OSError as e:
            self._send(500, ("trigger spawn failed: %s" % e).encode())
            return
        print("[webhook] POST /webhook accepted, trigger spawned", flush=True)
        self._send(200, b'{"ok": true}')


def heartbeat():
    while True:
        time.sleep(HEARTBEAT_SEC)
        print(f"[webhook] heartbeat — listener alive (pid {os.getpid()})", flush=True)


if __name__ == "__main__":
    try:
        with open(PID_FILE, "w") as f:
            f.write(str(os.getpid()))
    except OSError as e:
        print(f"[webhook] WARNING: cannot write pid file {PID_FILE}: {e}", flush=True)
    threading.Thread(target=heartbeat, daemon=True).start()
    server = ThreadingHTTPServer(("0.0.0.0", PORT), Handler)
    print(f"webhook listening on :{PORT} (secret={'set' if SECRET else 'NOT SET'})", flush=True)
    server.serve_forever()
