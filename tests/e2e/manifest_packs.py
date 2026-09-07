"""Packed capture replay and corrupt range responses over real HTTP."""
import hashlib
import json
import subprocess
import sys
import urllib.request
import uuid

from api_access import servers
from capture_batches import packed, published, manifest
import run


def relay(method="GET", path="/status", body=None):
    request = urllib.request.Request("http://manifest-relay:8080"+path, method=method,
        data=None if body is None else json.dumps(body).encode(),
        headers={"X-E2E-Control":"e2e-manifest-fault-only", "Content-Type":"application/json"})
    with urllib.request.urlopen(request, timeout=30) as response:
        return json.load(response)


def capture(state, body, *, puts, peer=None):
    before = relay()["puts"]
    result = run.request("POST",f"/roots/{state['root']}/versions",body,
        key=state["key"],server=peer,statuses=(202,))
    assert relay()["puts"]-before == puts
    print(json.dumps({"event":"manifest_uploads","files":len(body["files"]),"puts":puts}),flush=True)
    return result


def stamps(root):
    return run.sql("""SELECT v.id,v.xmin::text AS stamp,v.source_manifest_ref
        FROM file_versions v JOIN file_catalog f ON f.id=v.file_id
        WHERE f.root_id=%s ORDER BY v.id""",(root,))


def verify_command(state):
    result = subprocess.run([sys.executable,"/app/modal/source_verify.py","--org-id",state["org"],
        "--root-id",state["root"],"--timeout","120"],capture_output=True,text=True,timeout=150)
    assert result.returncode == 0, result.stdout[-2000:]+result.stderr[-1000:]
    summary = json.loads(result.stdout.splitlines()[-1])
    assert summary["status"] == "verified" and summary["failed_versions"] == 0
    print(json.dumps(summary),flush=True)


def verify():
    state = run.provision()
    state["root"] = run.new_root(state,"Packed manifests","/state/manifest-packs",True)
    peers = servers()
    run.save(state)
    # A full 128-file capture shares one physical object, with independent records.
    data = [f"Orchid calibration packed source {i}.\n".encode() for i in range(128)]
    sources = packed(state,state["root"],data)
    new = {"capture_id":str(uuid.uuid4()),"files":[{"path":f"packed-{i}.txt","source":source} for i,source in enumerate(sources)]}
    receipt = capture(state,new,puts=1,peer=peers[1])
    expected = {f["path"]:d.decode() for f,d in zip(new["files"],data)}
    files = published(state,state["root"],expected)
    refs = [files[f["path"]]["source_manifest_ref"] for f in new["files"]]
    keys = {ref.split("#")[0] for ref in refs}
    assert len(keys) == 1 and len(set(refs)) == 128
    key, = keys
    with run.s3.get_object(Bucket=run.BUCKET,Key=key)["Body"] as stream:
        raw = stream.read()
    assert hashlib.sha256(raw).hexdigest() == key.rsplit("/",1)[1].removesuffix(".jsonl")
    assert len(raw.splitlines()) == 128
    original = stamps(state["root"])
    reversed_body = {**new,"files":list(reversed(new["files"]))}
    reversed_receipt = capture(state,reversed_body,puts=1,peer=peers[0])
    assert reversed_receipt["versions"] == list(reversed(receipt["versions"]))
    subset = {**new,"files":new["files"][17:19]}
    assert capture(state,subset,puts=1,peer=peers[1])["versions"] == receipt["versions"][17:19]
    assert stamps(state["root"]) == original
    verify_command(state)
    # Tombstone-only batches do not create a manifest object.
    deleted = {"capture_id":str(uuid.uuid4()),"files":[{"path":new["files"][0]["path"],
        "previous_version_id":receipt["versions"][0]["version_id"],"deleted":True}]}
    capture(state,deleted,puts=0)
    expected[new["files"][0]["path"]] = None
    published(state,state["root"],expected)
    for mode, error in (("flip","stored source manifest hash mismatch"),
                        ("truncate","source manifest response length mismatch")):
        root = run.new_root(state,"Manifest transport integrity","/state/manifest-"+mode,True)
        payload = b"Orchid calibration transport integrity.\n"
        source = packed(state,root,[payload])[0]
        prefix = f"sources/{state['org']}/{root}/manifests/"
        relay("POST","/fault",{"prefix":prefix,"mode":mode})
        try:
            run.request("POST",f"/roots/{root}/versions",{"capture_id":str(uuid.uuid4()),
                "files":[{"path":"source.txt","source":source}]},key=state["key"],statuses=(202,))
            def failed():
                rows = run.sql("""SELECT w.error FROM file_work w JOIN file_extractions e ON e.id=w.extraction_id
                    JOIN file_versions v ON v.id=e.version_id JOIN file_catalog f ON f.id=v.file_id
                    WHERE f.root_id=%s AND w.stage='transform'""",(root,))
                return rows and error in rows[0]["error"]
            run.eventually("manifest integrity rejection",failed,timeout=120)
            events = [e for e in relay()["events"] if e["key"].startswith(prefix)]
            assert events and all(e["range"] and e["mode"] == mode for e in events)
            assert all(not f["indexed_version_id"] for f in run.catalog(state,root).values())
            assert not run.request("POST","/query",{"root_id":root,"query":"calibration","mode":"fts"},key=state["key"])["results"]
        finally:
            relay("DELETE","/fault")
        published(state,root,{"source.txt":payload.decode()})
        print(f"Manifest {mode}: rejected before indexing, then normal queue retry published exact source bytes.",flush=True)
    run.cleanup()
    run.STATE.unlink()
    print("128-file single manifest PUT, reordered/subset replay, integrity retries and complete S3 cleanup passed.",flush=True)

if __name__ == "__main__":
    {"packed":verify}[sys.argv[1]]()
