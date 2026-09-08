"""Create a bounded dense-JSONL fixture manifest for production-capacity.py.

This only writes local synthetic data. Capture it through the normal CLI with
production-capacity.py capture --state <manifest>; verification and cleanup use
that same recorded identity. Dense identifiers are separated by JSON syntax,
so one giant unbroken string does not collapse into a tokenizer's unknown token.
"""

import argparse
import hashlib
import json
from pathlib import Path
import time
import uuid


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--state", type=Path, required=True)
    parser.add_argument("--files", type=int, default=64)
    parser.add_argument("--records", type=int, default=1024)
    args = parser.parse_args()
    if args.state.exists():
        parser.error("state already exists; use its recorded capture/verify/cleanup workflow")
    if not 1 <= args.files <= 2048 or not 1 <= args.records <= 16384:
        parser.error("files must be 1..2048 and records must be 1..16384")
    args.state.parent.mkdir(parents=True, exist_ok=True)
    nonce = uuid.uuid4().hex
    directory = args.state.parent / ("capacity-load-fixtures-" + nonce)
    directory.mkdir()
    state = {"run_id": nonce, "directory": str(directory), "fixtures": [],
             "workload_description": "Synthetic JSONL diagnostic records with prose and unique arrays "
             "of up-to-64-character hexadecimal telemetry identifiers; equal-sized files, cold content hashes. "
             "A machine-generated-text stress workload, not an unchanged replay of sessions.",
             "cache_intent": "cold"}
    started = time.monotonic()
    for ordinal in range(args.files):
        path = directory / f"diagnostics-{ordinal:03d}.jsonl"
        with path.open("w") as output:
            for number in range(args.records):
                dense = hashlib.shake_256(f"{nonce}:{ordinal}:{number}".encode()).hexdigest(2200)
                row = {"run": nonce, "file": ordinal, "record": number,
                       "text": "Telescope calibration records instrument temperature, humidity, exposure and alignment. " * 10,
                       "telemetry": [dense[i:i + 64] for i in range(0, len(dense), 64)]}
                line = json.dumps(row)
                assert 5000 < len(line.encode()) < 6000
                output.write(line + "\n")
        state["fixtures"].append({"path": path.name, "kind": "native", "lines": args.records})
    state["source_bytes"] = sum(path.stat().st_size for path in directory.iterdir())
    args.state.write_text(json.dumps(state, indent=2))
    print(json.dumps({"state": str(args.state), "files": args.files,
                      "records": args.files * args.records, "source_bytes": state["source_bytes"],
                      "generation_seconds": round(time.monotonic() - started, 3)}))


if __name__ == "__main__":
    main()
