#!/usr/bin/env python3
"""Generate/check Claude adapters from the portable Repowire remote plugin."""
import argparse
import json
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1] / "plugins" / "repowire-remote"
parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--check", action="store_true", help="fail on adapter drift")
args = parser.parse_args()

manifest = json.loads((ROOT / "plugin.json").read_text())
manifest.pop("$schema", None)
mcp = json.loads((ROOT / "mcp.json").read_text())
mcp.pop("$schema", None)
for server in mcp["mcpServers"].values():
    if server["type"] == "streamable-http":
        server["type"] = "http"

for name, content in ((".claude-plugin/plugin.json", manifest), (".mcp.json", mcp)):
    path = ROOT / name
    expected = json.dumps(content, indent=2) + "\n"
    if args.check:
        if path.is_symlink() or not path.is_file() or path.read_text() != expected:
            raise SystemExit(f"Stale adapter: {name}; run scripts/package-remote-plugin.py")
    else:
        if path.is_symlink():
            path.unlink()
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(expected)
print("Remote plugin adapters are current.")
