#!/usr/bin/env python3
"""Minimal imsgd webhook receiver (standard library only).

Verifies the X-Imsgd-Signature header, de-duplicates by event ID, and prints
each event. Register it with:

  curl -H "Authorization: Bearer $IMSGD_KEY" -H 'Content-Type: application/json' \
    -d '{"url":"http://<this-host>:9876/","events":["message.*"]}' $IMSGD/api/v1/webhooks

then run:  IMSGD_WEBHOOK_SECRET=whsec_... python3 receiver.py
"""
import hashlib
import hmac
import http.server
import json
import os
import time

SECRET = os.environ["IMSGD_WEBHOOK_SECRET"].encode()
TOLERANCE = 300  # seconds
seen = set()


def verify(header: str, body: bytes) -> bool:
    try:
        parts = dict(p.split("=", 1) for p in header.split(","))
        ts = int(parts["t"])
    except (KeyError, ValueError):
        return False
    if abs(time.time() - ts) > TOLERANCE:
        return False
    mac = hmac.new(SECRET, f"{ts}.".encode() + body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(mac, parts.get("v1", ""))


class Handler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        if not verify(self.headers.get("X-Imsgd-Signature", ""), body):
            self.send_response(401)
            self.end_headers()
            return
        event = json.loads(body)
        # Acknowledge quickly; retries reuse the same event ID.
        self.send_response(204)
        self.end_headers()
        if event["id"] in seen:
            return
        seen.add(event["id"])
        data = event["data"]
        if event["type"].startswith("message."):
            m = data["message"]
            who = "me" if m["from_me"] else (m["sender"] or {}).get("name") or (m["sender"] or {}).get("address")
            extra = f" changes={data['changes']}" if "changes" in data else ""
            print(f"{event['type']:<17} [{m['type']}] {who}: {m['text']!r}{extra}")
        else:
            print(event["type"], json.dumps(data))

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    port = int(os.environ.get("PORT", "9876"))
    print(f"listening on :{port}")
    http.server.ThreadingHTTPServer(("0.0.0.0", port), Handler).serve_forever()
