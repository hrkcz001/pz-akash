#!/usr/bin/env python3
"""
GitHub webhook listener for the autosaver.

POST /webhook  -> verifies X-Hub-Signature-256 (when WEBHOOK_SECRET is set),
                  then spawns /usr/local/bin/trigger.sh in the background
                  (git pull + consume start/backup/halt triggers).
GET  /healthz  -> 200 OK

Env:
  WEBHOOK_PORT    listen port (default 8080)
  WEBHOOK_SECRET  shared secret configured in the GitHub webhook (optional,
                  but strongly recommended)
"""
import hashlib
import hmac
import os
import subprocess
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(os.environ.get("WEBHOOK_PORT", "8080"))
SECRET = os.environ.get("WEBHOOK_SECRET", "")
TRIGGER = os.environ.get("WEBHOOK_TRIGGER", "/usr/local/bin/trigger.sh")
LOG = os.environ.get("WEBHOOK_LOG", "/data/webhook.log")


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
            with open(LOG, "ab") as f:
                subprocess.Popen([TRIGGER], stdout=f, stderr=subprocess.STDOUT)
        except OSError as e:
            self._send(500, ("trigger spawn failed: %s" % e).encode())
            return
        self._send(200, b'{"ok": true}')


if __name__ == "__main__":
    server = ThreadingHTTPServer(("0.0.0.0", PORT), Handler)
    print(f"webhook listening on :{PORT} (secret={'set' if SECRET else 'NOT SET'})", flush=True)
    server.serve_forever()
