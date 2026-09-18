#!/usr/bin/env python3
"""Build the single public CLI manifest from verified release checksums."""

import argparse
import json
from pathlib import Path
import re

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--version", required=True)
parser.add_argument("--checksums", type=Path, required=True)
parser.add_argument("--output", type=Path, required=True)
parser.add_argument("--base-url", default="https://pufferfs.com/releases")
parser.add_argument("--minimum", default="0.7.0")
args = parser.parse_args()
version = args.version.removeprefix("v")
if not re.fullmatch(r"\d+\.\d+\.\d+(?:-[A-Za-z0-9.-]+)?", version):
    parser.error("version must be a release version")
checksums = {}
for line in args.checksums.read_text().splitlines():
    digest, name = line.split()
    if not re.fullmatch(r"[0-9a-f]{64}", digest):
        parser.error("invalid SHA-256")
    checksums[name.removeprefix("*")] = digest
protocol_source = Path(__file__).resolve().parents[2] / "pkg/models/models.go"
protocol = int(re.search(r"const SyncProtocolVersion = (\d+)", protocol_source.read_text())[1])
downloads = {}
for target in ("darwin-amd64", "darwin-arm64", "linux-amd64", "linux-arm64"):
    archive = f"pufferfs_{version}_{target.replace('-', '_')}.tar.gz"
    downloads[target] = {"url": f"{args.base_url.rstrip('/')}/v{version}/{archive}", "sha256": checksums[archive]}
args.output.write_text(json.dumps({"latest": version, "minimum": args.minimum,
    "protocol_min": protocol, "protocol_max": protocol, "downloads": downloads,
    "notes_url": f"https://github.com/suhjohn/pufferfs/releases/tag/v{version}"}, indent=2) + "\n")
