#!/usr/bin/env python3
"""Connect a ChatGPT account to a remote Gyemoim server.

Run this on the machine that has your browser:

    python3 gyemoim-connect.py <server-url> <enrollment-code>

The enrollment code comes from the Gyemoim web UI (single use, valid for about
10 minutes). The script starts a temporary local listener, claims the code on
the Gyemoim server, opens your browser for the OpenAI sign-in, and forwards the
provider redirect back to the server. The server owns the whole OAuth flow:
dynamic client registration, PKCE, the token exchange, and ID-token
verification all happen server-side; this script is only a thin bridge.

Requires only the Python standard library (Python 3.8+).
"""

import argparse
import json
import ssl
import sys
import threading
import urllib.error
import urllib.parse
import urllib.request
import webbrowser
from http.server import BaseHTTPRequestHandler, HTTPServer

CALLBACK_WAIT_SECONDS = 600
REQUEST_TIMEOUT_SECONDS = 15
MAX_RESPONSE_BYTES = 1 << 20

# Exit code and plain-language message for each server-reported outcome.
OUTCOMES = {
    "connected": (
        0,
        "Success: the ChatGPT account is connected to Gyemoim.",
    ),
    "plan_usage_disabled": (
        1,
        "The account was connected, but direct API use is not enabled for this\n"
        "ChatGPT plan. Enable API access on the plan, then reconnect with a new\n"
        "enrollment code.",
    ),
    "require_reauthentication": (
        1,
        "The account was connected, but Gyemoim reports it must be\n"
        "re-authenticated. Request a new enrollment code and try again.",
    ),
    "authorization_denied": (
        1,
        "Authorization was denied in the browser. Nothing was changed; request a\n"
        "new enrollment code and try again.",
    ),
    "failed": (
        1,
        "The connection failed. The enrollment code is used up either way;\n"
        "request a new one from the Gyemoim web UI and try again.",
    ),
}

CLOSE_PAGE = b"""<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><title>Gyemoim</title></head>
<body>
<p><strong>You can close this window.</strong></p>
<p>Return to the Gyemoim web UI to see the connection result.</p>
</body>
</html>
"""


class FlowError(Exception):
    """A clean, printable failure — never shown as a traceback."""


def certificate_hint(error):
    """An actionable message for TLS verification problems on user machines."""
    reason = getattr(error, "reason", None)
    if isinstance(error, ssl.SSLError) or isinstance(reason, ssl.SSLError):
        return (
            "The server's TLS certificate could not be verified.\n"
            "On macOS with a python.org Python, run \"Install Certificates.command\"\n"
            "from the Python 3 folder in Applications (or install the certifi\n"
            "package). On other systems, make sure your system CA certificates\n"
            "are up to date."
        )
    return None


def post_json(url, payload):
    """POST one JSON request and decode the JSON response."""
    data = json.dumps(payload).encode("utf-8")
    request = urllib.request.Request(
        url,
        data=data,
        headers={"Content-Type": "application/json", "Accept": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(request, timeout=REQUEST_TIMEOUT_SECONDS) as response:
            body = response.read(MAX_RESPONSE_BYTES + 1)
    except urllib.error.HTTPError as error:
        message = "the Gyemoim server rejected the request (HTTP %d)" % error.code
        try:
            detail = json.loads(error.read(MAX_RESPONSE_BYTES).decode("utf-8", "replace"))
            text = detail.get("error", {}).get("message")
            if isinstance(text, str) and text:
                message = "the Gyemoim server rejected the request: %s" % text
        except (ValueError, OSError, AttributeError):
            pass
        raise FlowError(message) from None
    except urllib.error.URLError as error:
        hint = certificate_hint(error)
        if hint:
            raise FlowError(hint) from None
        raise FlowError("could not reach the Gyemoim server: %s" % error.reason) from None
    except OSError as error:
        raise FlowError("could not reach the Gyemoim server: %s" % error) from None
    if len(body) > MAX_RESPONSE_BYTES:
        raise FlowError("the Gyemoim server returned an unexpectedly large response")
    try:
        return json.loads(body.decode("utf-8"))
    except ValueError:
        raise FlowError("the Gyemoim server returned an unreadable response") from None


class Capture:
    """Hands the provider redirect parameters from the handler to the main thread."""

    def __init__(self):
        self.event = threading.Event()
        self.params = {}
        self.raw_query = ""


def make_handler(capture):
    class CallbackHandler(BaseHTTPRequestHandler):
        def do_GET(self):
            parsed = urllib.parse.urlsplit(self.path)
            if parsed.path != "/auth/callback":
                self.send_response(404)
                self.send_header("Content-Type", "text/plain; charset=utf-8")
                self.send_header("Content-Length", "0")
                self.end_headers()
                return
            query = urllib.parse.parse_qs(parsed.query)
            capture.params = {key: values[0] for key, values in query.items() if values}
            # The raw query is forwarded verbatim so the server sees exactly
            # what the provider sent, including repeated or blank parameters.
            capture.raw_query = parsed.query
            self.send_response(200)
            self.send_header("Content-Type", "text/html; charset=utf-8")
            self.send_header("Content-Length", str(len(CLOSE_PAGE)))
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            self.wfile.write(CLOSE_PAGE)
            capture.event.set()

        def log_message(self, fmt, *args):
            pass  # keep the console clean

    return CallbackHandler


def main(argv=None):
    parser = argparse.ArgumentParser(
        description="Connect a ChatGPT account to a Gyemoim server using an "
        "enrollment code from the Gyemoim web UI."
    )
    parser.add_argument(
        "server_url",
        help="base URL of the Gyemoim server, for example https://gyemoim.example.com",
    )
    parser.add_argument(
        "enrollment_code",
        help="single-use enrollment code from the Gyemoim web UI (valid about 10 minutes)",
    )
    args = parser.parse_args(argv)

    server_url = args.server_url.strip().rstrip("/")
    if not server_url.startswith(("http://", "https://")) or len(server_url) <= 8:
        print("Error: the server URL must start with http:// or https://", file=sys.stderr)
        return 1
    code = args.enrollment_code.strip()
    if not code or len(code) > 128:
        print("Error: an enrollment code from the Gyemoim web UI is required.", file=sys.stderr)
        return 1
    parsed_url = urllib.parse.urlsplit(server_url)
    if parsed_url.scheme == "http" and parsed_url.hostname not in ("localhost", "127.0.0.1", "::1"):
        print("Warning: %s uses plain HTTP, so the enrollment code and the forwarded "
              "authorization code will travel unencrypted." % server_url, file=sys.stderr)

    capture = Capture()
    server = HTTPServer(("127.0.0.1", 0), make_handler(capture))
    port = server.server_address[1]
    server_thread = threading.Thread(target=server.serve_forever, daemon=True)
    server_thread.start()

    try:
        print("Connecting to %s ..." % server_url)
        claim = post_json(
            server_url + "/connect/claim", {"code": code, "callbackPort": port}
        )
        authorization_url = claim.get("authorizationUrl")
        provider_name = claim.get("providerName")
        if not isinstance(provider_name, str) or not provider_name:
            provider_name = "the ChatGPT account"
        if not isinstance(authorization_url, str) or not authorization_url:
            raise FlowError("the Gyemoim server returned no authorization URL")

        print("Opening your browser to sign in to %s." % provider_name)
        print("If the browser does not open, visit this URL manually:")
        print("  %s" % authorization_url)
        try:
            if not webbrowser.open(authorization_url):
                print("No default browser is available; use the URL above.")
        except Exception:
            print("No default browser is available; use the URL above.")

        print("Waiting for the provider to redirect back (up to 10 minutes) ...")
        if not capture.event.wait(CALLBACK_WAIT_SECONDS):
            raise FlowError(
                "Timed out waiting for the browser redirect. The enrollment code is\n"
                "used up; request a new one and try again."
            )

        params = capture.params
        if "error" in params:
            payload = {
                "code": code,
                "state": params.get("state", ""),
                "error": params["error"],
            }
        else:
            payload = {
                "code": code,
                "state": params.get("state", ""),
                "authorizationCode": params.get("code", ""),
            }
            if "client_id" in params:
                payload["clientID"] = params["client_id"]
        if capture.raw_query:
            payload["query"] = capture.raw_query
        outcome = post_json(server_url + "/connect/complete", payload)

        status = outcome.get("status") if isinstance(outcome, dict) else None
        exit_code, message = OUTCOMES.get(
            status, (1, "The server reported an unrecognized result: %r." % (status,))
        )
        print(message)
        return exit_code
    except FlowError as error:
        print("Error: %s" % error, file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        print("\nCancelled. The enrollment code may already be used; request a new\n"
              "one if you want to retry.", file=sys.stderr)
        return 1
    finally:
        server.shutdown()
        server.server_close()


if __name__ == "__main__":
    sys.exit(main())
