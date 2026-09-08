"""Capacity observations through the installed CLI and deployed production roles.

This creates an explicitly named synthetic user root. It never edits database
work state, queues, or deployment settings. State contains cleanup identities,
fixture expectations, and timings, never credentials. Authentication uses the
CLI's ordinary configuration. Production fault injection is outside its scope.
"""

import argparse
import datetime
import json
from pathlib import Path
import subprocess
import time
import uuid


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=("capture", "verify", "query", "cleanup"))
    parser.add_argument("--binary", required=True)
    parser.add_argument("--state", required=True, type=Path)
    parser.add_argument("--root", help="Existing root for read-only query measurements")
    parser.add_argument("--query", default="telescope calibration")
    parser.add_argument("--seconds", type=int, default=600)
    parser.add_argument("--interval", type=float, default=15)
    parser.add_argument("--force", action="store_true")
    args = parser.parse_args()
    if args.seconds < 1 or args.interval < 1:
        parser.error("seconds and interval must be positive")
    if args.root and args.mode != "query":
        parser.error("--root is only accepted for read-only query measurements")
    args.state.parent.mkdir(parents=True, exist_ok=True)
    state = json.loads(args.state.read_text()) if args.state.exists() else {}

    def save():
        args.state.write_text(json.dumps(state, indent=2))

    def event(**values):
        row = {"at": datetime.datetime.now(datetime.timezone.utc).isoformat(), **values}
        with args.state.with_suffix(".events.jsonl").open("a") as output:
            output.write(json.dumps(row) + "\n")
        print(json.dumps(row), flush=True)

    def cli(*arguments):
        started = time.monotonic()
        response = subprocess.run([args.binary, *arguments], capture_output=True, text=True, timeout=600)
        elapsed = time.monotonic() - started
        if response.returncode:
            event(operation=arguments[0], exit_code=response.returncode, seconds=round(elapsed, 3))
            raise RuntimeError("CLI operation failed; credentials and response bodies were not logged")
        return json.loads(response.stdout), elapsed

    if args.mode == "capture":
        if not state:
            from reportlab.pdfgen import canvas
            nonce = uuid.uuid4().hex
            directory = args.state.parent / ("capacity-fixtures-" + nonce)
            directory.mkdir()
            state = {"run_id": nonce, "directory": str(directory), "fixtures": []}
            for ordinal, records in enumerate((8, 64, 128, 768) * 4):
                path = directory / f"measurements-{ordinal:02d}.jsonl"
                with path.open("w") as output:
                    for number in range(records):
                        record = {"run": nonce, "file": ordinal, "record": number,
                            "text": "Telescope calibration measures temperature, humidity, exposure and alignment. " * 55}
                        line = json.dumps(record, ensure_ascii=False)
                        assert 3000 < len(line.encode()) < 6000
                        output.write(line + "\n")
                state["fixtures"].append({"path": path.name, "lines": records, "kind": "native"})
            path = directory / "instrument-readings.csv"
            path.write_text("instrument,temperature,humidity\n" +
                            "".join(f"sensor-{i},{18 + i % 8},{40 + i % 12}\n" for i in range(500)))
            state["fixtures"].append({"path": path.name, "kind": "structured",
                                      "required_text": ["sensor-0", "sensor-499"]})
            path = directory / "calibration-report.pdf"
            document = canvas.Canvas(str(path))
            for page in range(1, 5):
                document.setFont("Helvetica", 14)
                document.drawString(50, 790, f"Telescope calibration report: page {page}")
                document.setFont("Helvetica", 10)
                for row in range(30):
                    document.drawString(50, 760 - row * 22,
                        f"Instrument {page * 100 + row}: temperature {18 + row % 8} C; humidity {40 + row % 12} percent.")
                document.showPage()
            document.save()
            state["fixtures"].append({"path": path.name, "kind": "document", "pages": 4,
                                      "required_text": ["instrument"]})
            state["source_bytes"] = sum(p.stat().st_size for p in directory.iterdir())
            save()
        arguments = ["sync", state["directory"], "--json"]
        if state.get("root_id"):
            arguments.extend(["--id", state["root_id"]])
        else:
            arguments.extend(["--scope", "user", "--name", "capacity-" + state["run_id"]])
        if args.force:
            arguments.append("--force")
        state["capture_started_at"] = time.time()
        save()
        result, elapsed = cli(*arguments)
        state.update(root_id=result["root_id"], capture_seconds=elapsed, capture_finished_at=time.time())
        save()
        event(operation="capture", root_id=state["root_id"], files=len(state["fixtures"]),
              source_bytes=state["source_bytes"], seconds=round(elapsed, 3), force=args.force)
        return

    root = args.root or state.get("root_id")
    if not root:
        parser.error("capture first or supply --root for query measurements")
    if args.mode == "query":
        deadline = time.monotonic() + args.seconds
        while time.monotonic() < deadline:
            for mode in ("fts", "vector", "hybrid"):
                result, elapsed = cli("query", args.query, "--root", root,
                                      "--mode", mode, "--top-k", "5", "--json")
                event(operation="query", root_id=root, mode=mode, seconds=round(elapsed, 3),
                      results=len(result.get("results", [])))
            time.sleep(args.interval)
        return
    if args.mode == "cleanup":
        if args.root or not state.get("run_id"):
            parser.error("cleanup accepts only this script's recorded synthetic root")
        response = subprocess.run([args.binary, "root", "delete", root, "--yes"],
                                  capture_output=True, text=True, timeout=600)
        if response.returncode:
            raise RuntimeError("Synthetic root cleanup failed; recorded identity retained")
        state["cleanup_requested_at"] = time.time()
        save()
        event(operation="cleanup_requested", root_id=root)
        return

    deadline = time.monotonic() + args.seconds
    while time.monotonic() < deadline:
        result, elapsed = cli("sync", "status", root, "--json")
        event(operation="status", root_id=root, status=result.get("status"),
              total=result.get("total"), states=result.get("states"), seconds=round(elapsed, 3))
        if result.get("status") == "complete":
            state.setdefault("publication_observed_at", time.time())
            state["capture_to_search_seconds"] = state["publication_observed_at"] - state["capture_started_at"]
            save()
            break
        if result.get("status") == "failed":
            raise RuntimeError("Synthetic production root failed indexing")
        time.sleep(args.interval)
    else:
        raise RuntimeError("Observation deadline reached; root and state retained for continued verification")
    for fixture in state["fixtures"]:
        if fixture["kind"] == "native":
            result, _ = cli("read", fixture["path"], "--root", root, "--lines",
                            f"1:{fixture['lines']}", "--json")
            expected = (Path(state["directory"]) / fixture["path"]).read_text().splitlines()
            assert [row["content"] for row in result["lines"]] == expected
        elif fixture["kind"] == "document":
            result, _ = cli("read", fixture["path"], "--root", root,
                            "--pages", f"1:{fixture['pages']}", "--json")
            assert len(result["pages"]) == fixture["pages"]
            assert all(text in page["content"].lower() for page in result["pages"]
                       for text in fixture["required_text"])
        elif fixture["kind"] == "structured":
            # Spreadsheet chunks carry sheet/row coordinates, not physical
            # source-line metadata. Verify their supplied content expectations
            # through the public search contract instead of inventing line reads.
            for text in fixture["required_text"]:
                result, elapsed = cli("query", text, "--root", root, "--glob", fixture["path"],
                                      "--mode", "fts", "--top-k", "20", "--json")
                hits = result["results"]
                assert hits and all(hit["root_id"] == root and hit["file_path"] == fixture["path"]
                                    for hit in hits)
                assert any(text in hit["content"] for hit in hits)
                event(operation="structured_search_verified", path=fixture["path"],
                      seconds=round(elapsed, 3), results=len(hits))
    for mode in ("fts", "vector", "hybrid"):
        result, elapsed = cli("query", "telescope calibration", "--root", root,
                              "--mode", mode, "--top-k", "5", "--json")
        assert result["results"]
        assert all(hit["root_id"] == root for hit in result["results"])
        event(operation="query_verified", mode=mode, seconds=round(elapsed, 3), results=len(result["results"]))
    state["verified_at"] = time.time()
    save()
    event(operation="verified", root_id=root, capture_to_search_seconds=round(state["capture_to_search_seconds"], 3))


if __name__ == "__main__":
    main()
