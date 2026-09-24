"""Real filesystem events, overflow and restart through the production CLI."""
import hashlib
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile

import run


def prepare():
    state = run.provision()
    directory = Path("/state/changed-paths")
    home = Path("/state/follower-home")
    for path in (directory, home, directory / "guard", directory / "overflow"):
        path.mkdir()
        os.chown(path, 1000, 1000)
    (directory / "guard/unchanged.txt").write_text("Orchid untouched branch.\n")
    source = directory / "activity.txt"
    source.write_text("Orchid first activity.\n")
    root = run.new_root(state, "Changed-path follower", directory, True)
    state["root"] = root
    run.save(state)
    environment = dict(os.environ, HOME=str(home), PUFFERFS_API_KEY=state["key"])
    command = ["pufferfs", "sync", str(directory), "--id", root, "--no-vector", "--follow", "--debounce", "100ms"]
    log = tempfile.TemporaryFile()

    def contents():
        return os.pread(log.fileno(), 100000, 0).decode()

    def alive():
        assert process.poll() is None, contents()[-3000:]

    def captured(path, text, deleted=False):
        digest = "" if deleted else "sha256:" + hashlib.sha256(text.encode()).hexdigest()
        def ready():
            alive()
            row = run.catalog(state).get(path)
            return row if row and row["content_hash"] == digest and row["deleted"] == deleted else None
        return run.eventually("changed path " + path, ready, 90)

    def start():
        return subprocess.Popen(command, env=environment, user=1000, group=1000,
            stdin=subprocess.DEVNULL, stdout=log, stderr=log)

    process = start()
    try:
        run.eventually("follower ready", lambda: (alive() is None and "Following " in contents()), 60)
        initial = captured("activity.txt", source.read_text())
        # The CLI runs without root privileges. Any full-tree walk now fails
        # on this unrelated directory; a changed-path capture must still work.
        guard = directory / "guard"
        guard.chmod(0)
        denied = subprocess.run([sys.executable, "-c", "import os; os.listdir('/state/changed-paths/guard')"],
            user=1000, group=1000, capture_output=True)
        assert denied.returncode != 0 and b"PermissionError" in denied.stderr
        with source.open("a") as output:
            output.write("Orchid appended activity.\n")
        appended = captured("activity.txt", source.read_text())
        before, after = run.assert_source_retained(initial), run.assert_source_retained(appended)
        assert after["extents"][:len(before["extents"])] == before["extents"]
        print("Append captured with an unreadable unrelated directory; source extent reuse preserved.", flush=True)
        guard.chmod(0o755)

        nested = directory / "new/deep"
        nested.mkdir(parents=True)
        (nested / "record.txt").write_text("Orchid nested event.\n")
        captured("new/deep/record.txt", "Orchid nested event.\n")
        (directory / "new").rename(directory / "moved")
        captured("new/deep/record.txt", "", deleted=True)
        captured("moved/deep/record.txt", "Orchid nested event.\n")
        ignored = directory / "ignored.txt"
        ignored.write_text("Orchid policy transition.\n")
        captured("ignored.txt", ignored.read_text())
        (directory / ".tpfsignore").write_text("ignored.txt\n")
        captured("ignored.txt", "", deleted=True)
        (directory / ".tpfsignore").unlink()
        captured("ignored.txt", ignored.read_text())

        # Force actual Linux inotify overflow at the OS boundary. The process
        # cannot drain its queue while stopped; a final event is necessarily
        # beyond that queue's configured capacity and requires a full repair.
        capacity = int(Path("/proc/sys/fs/inotify/max_queued_events").read_text())
        assert capacity <= 2000000, "OS event capacity too large for bounded fixture"
        process.send_signal(signal.SIGSTOP)
        try:
            # Alternating filenames prevent inotify from coalescing adjacent
            # identical events, without creating a million fixture inodes.
            transient = [directory / "overflow" / f"transient-{i}.txt" for i in range(2)]
            handles = [os.open(p, os.O_CREAT | os.O_WRONLY, 0o644) for p in transient]
            try:
                for i in range(capacity + 512):
                    os.write(handles[i % 2], b"x")
            finally:
                for handle in handles:
                    os.close(handle)
                for path in transient:
                    path.unlink()
            (directory / "overflow/survivor.txt").write_text("Orchid overflow recovery.\n")
        finally:
            process.send_signal(signal.SIGCONT)
        captured("overflow/survivor.txt", "Orchid overflow recovery.\n")
        run.eventually("real watcher overflow observed", lambda: "watcher requires full reconciliation" in contents(), 30)
        print("Nested create/rename, ignore-rule changes and actual OS event overflow reconciled.", flush=True)

        process.kill()
        process.wait(timeout=10)
        source.write_text("Orchid offline restart replacement.\n")
        process = start()
        captured("activity.txt", source.read_text())
        process.terminate()
        assert process.wait(timeout=15) == 0, contents()[-3000:]
        state["follow_expected"] = {p.relative_to(directory).as_posix(): p.read_text()
            for p in directory.rglob("*.txt") if p.is_file()}
        run.save(state)
        print("SIGKILL restart found an offline rewrite; SIGTERM stopped cleanly.", flush=True)
    finally:
        (directory / "guard").chmod(0o755)
        if process.poll() is None:
            process.kill()
        process.wait(timeout=10)
        log.close()


def verify():
    state = json.loads(run.STATE.read_text())
    files = run.wait_indexed(state)
    for path, expected in state["follow_expected"].items():
        run.assert_source_retained(files[path])
        result = run.request("POST", f"/roots/{state['root']}/read", {"path": path, "lines": {"start": 1, "end": 100}}, key=state["key"])
        assert [line["content"] for line in result["lines"]] == expected.splitlines()
    assert run.request("POST", "/query", {"root_id": state["root"], "query": "orchid", "mode": "fts"}, key=state["key"])["results"]
    print("Changed-path captures published and returned exact retained bytes, reads and search.", flush=True)


if __name__ == "__main__":
    {"prepare": prepare, "verify": verify}[sys.argv[1]]()
