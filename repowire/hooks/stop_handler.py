#!/usr/bin/env python3
from __future__ import annotations

import json
import socket
import sys
from pathlib import Path

SOCKET_PATH = "/tmp/repowire.sock"
PENDING_DIR = Path.home() / ".repowire" / "pending"


def extract_last_assistant_response(transcript_path: Path) -> str | None:
    if not transcript_path.exists():
        return None

    last_response = None
    with open(transcript_path, "r") as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            try:
                entry = json.loads(line)
                if entry.get("type") == "assistant":
                    message = entry.get("message", {})
                    content = message.get("content", [])
                    if isinstance(content, list):
                        texts = [
                            c.get("text", "")
                            for c in content
                            if isinstance(c, dict) and c.get("type") == "text"
                        ]
                        if texts:
                            last_response = " ".join(texts)
                    elif isinstance(content, str):
                        last_response = content
            except json.JSONDecodeError:
                continue

    return last_response


def send_to_session_manager(correlation_id: str, response: str) -> bool:
    try:
        sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        sock.settimeout(5.0)
        sock.connect(SOCKET_PATH)

        message = json.dumps(
            {
                "type": "response",
                "correlation_id": correlation_id,
                "response": response,
            }
        )
        sock.sendall(message.encode("utf-8"))
        sock.close()
        return True
    except (socket.error, OSError):
        return False


def main() -> int:
    try:
        input_data = json.loads(sys.stdin.read())
    except json.JSONDecodeError:
        return 0

    if input_data.get("stop_hook_active", False):
        return 0

    session_id = input_data.get("session_id")
    transcript_path_str = input_data.get("transcript_path")

    if not session_id or not transcript_path_str:
        return 0

    pending_file = PENDING_DIR / f"{session_id}.json"
    if not pending_file.exists():
        return 0

    try:
        with open(pending_file, "r") as f:
            pending = json.load(f)
    except (json.JSONDecodeError, OSError):
        return 0

    correlation_id = pending.get("correlation_id")
    if not correlation_id:
        pending_file.unlink(missing_ok=True)
        return 0

    transcript_path = Path(transcript_path_str).expanduser()
    response = extract_last_assistant_response(transcript_path)

    if response:
        send_to_session_manager(correlation_id, response)

    pending_file.unlink(missing_ok=True)

    return 0


if __name__ == "__main__":
    sys.exit(main())
