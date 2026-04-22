#!/usr/bin/env python3
"""Minimal HTTP server for manual webhook testing: logs POST body and returns 200."""

from __future__ import annotations

import http.server
import os
import socketserver


def main() -> None:
    port = int(os.environ.get("WEBHOOK_SINK_PORT", "9999"))

    class Handler(http.server.BaseHTTPRequestHandler):
        def log_message(self, fmt: str, *args: object) -> None:
            print(f"[webhook-sink] {fmt % args}")

        def do_GET(self) -> None:
            self.send_response(200)
            self.end_headers()
            self.wfile.write(
                b"webhook sink: POST JSON here (e.g. /webhook). "
                b"Set DEFAULT_WEBHOOK_URL=http://127.0.0.1:%d/webhook\n" % port
            )

        def do_POST(self) -> None:
            length = int(self.headers.get("Content-Length", "0"))
            body = self.rfile.read(length) if length else b""
            print("--- POST", self.path)
            print(body.decode("utf-8", errors="replace"))
            for k, v in self.headers.items():
                if k.lower().startswith("x-payflow") or k.lower() == "idempotency-key":
                    print(f"  {k}: {v}")
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b"ok\n")

    with socketserver.TCPServer(("", port), Handler) as httpd:
        print(f"Listening on http://127.0.0.1:{port}/ (WEBHOOK_SINK_PORT={port})")
        print("Point DEFAULT_WEBHOOK_URL or MERCHANT_WEBHOOK_URLS at this URL for local tests.")
        httpd.serve_forever()


if __name__ == "__main__":
    main()
