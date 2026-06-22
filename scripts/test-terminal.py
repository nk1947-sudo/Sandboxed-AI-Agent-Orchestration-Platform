#!/usr/bin/env python3
"""
test-terminal.py — exercise the /terminal WebSocket bridge end-to-end.

Connects to the control plane's terminal endpoint, sends a shell script to run
inside a sandbox guest, and prints the streamed output / exit / HITL events.

Usage:
    PORT=7777 TOKEN=dev-secret-123 ./scripts/test-terminal.py <sandbox-id> "[script]"

Examples:
    # Benign command — should stream output and exit 0
    ./scripts/test-terminal.py sb-703812112563 "id; uname -a; echo hello-from-guest"

    # Sensitive command — should be intercepted by the HITL gate (prints approval_id)
    ./scripts/test-terminal.py sb-703812112563 "sudo rm -rf /etc"

Requires: pip install websockets   (or: pip install --user --break-system-packages websockets)
"""
import asyncio
import json
import os
import sys

try:
    import websockets
except ImportError:
    sys.exit("missing dependency: pip install websockets")


async def main():
    if len(sys.argv) < 2:
        sys.exit("usage: test-terminal.py <sandbox-id> \"[script]\"")
    sandbox = sys.argv[1]
    script = sys.argv[2] if len(sys.argv) > 2 else "id; uname -a; echo hello-from-guest"
    port = os.environ.get("PORT", "7777")
    token = os.environ.get("TOKEN", "dev-secret-123")

    uri = f"ws://localhost:{port}/terminal?sandbox={sandbox}"
    headers = {"Authorization": f"Bearer {token}"}

    # ping_interval=None disables client keepalive pings. The server blocks in
    # WaitForDecision while a HITL command is held and does not service WS
    # control frames during that window, so client pings would go unanswered and
    # the connection would be torn down before an operator can approve.
    # The header kwarg was also renamed across websockets versions.
    try:
        conn = websockets.connect(uri, additional_headers=headers, ping_interval=None)
    except TypeError:
        conn = websockets.connect(uri, extra_headers=headers, ping_interval=None)

    print(f"[client] connecting to {uri}")
    async with conn as ws:
        await ws.send(json.dumps({"script": script, "timeout_sec": 15}))
        print(f"[client] sent script: {script!r}\n")
        while True:
            try:
                # Generous timeout so a held HITL command can be approved from
                # another terminal without the client giving up.
                msg = await asyncio.wait_for(ws.recv(), timeout=180)
            except asyncio.TimeoutError:
                print("\n[client] timed out waiting for events")
                break
            except websockets.ConnectionClosed:
                print("\n[client] connection closed")
                break

            evt = json.loads(msg)
            kind = evt.get("kind")
            if kind == "output":
                print(evt.get("data", ""), end="", flush=True)
            elif kind == "exit":
                # exit_code is omitted from JSON when 0 (omitempty); on an exit
                # event a missing field unambiguously means success.
                print(f"\n[exit code={evt.get('exit_code', 0)}]")
                break
            elif kind == "pending":
                print(f"\n[HITL pending] approval_id={evt.get('approval_id')} "
                      f"reason={evt.get('reason')!r}")
                print("    approve: curl -s -X POST "
                      f"http://localhost:{port}/api/approvals/{evt.get('approval_id')} \\")
                print(f"      -H 'Authorization: Bearer {token}' "
                      "-H 'Content-Type: application/json' -d '{\"decision\":\"approve\"}'")
                print("    (waiting for decision — leave this running)")
            elif kind == "approved":
                print("\n[HITL approved] executing in guest...")
            elif kind == "rejected":
                print("\n[HITL rejected] command blocked")
                break
            elif kind == "error":
                print(f"\n[error] {evt.get('message')}")
                break


if __name__ == "__main__":
    asyncio.run(main())
