"""Batched capture through two API processes, real storage, workers and search."""

import hashlib
import json
from pathlib import Path
import sys
import time
import uuid

from api_access import servers
from api_groups import calls, parallel
from source_retention import upload, begin_retention, finish_retention, check_packed_isolation
from retention_security import check_tenant_uploads, check_capture_acl_race
from capture_permissions import check_capture_revocations
import run


def counts():
    return [calls(pattern) for pattern in (
        "INSERT INTO file_catalog(id,root_id,path)%",
        "SELECT f.id,f.path,COALESCE(f.captured_version_id,%",
        "WITH requested AS MATERIALIZED (%SELECT i.previous_version_id,e.*%",
        "WITH input AS MATERIALIZED (%INSERT INTO file_versions%",
        "INSERT INTO file_versions(id,file_id,capture_id%",
        "INSERT INTO file_version_extents(version_id,ordinal,object_key,byte_offset,byte_length)%")]


def manifest(data, extents):
    return {"format":1,"size":len(data),"content_hash":"sha256:"+hashlib.sha256(data).hexdigest(),"extents":extents}


def packed(state, root, payloads, parts=3):
    key = upload(state, root, b"".join(payloads))["object_key"]
    offset, sources = 0, []
    for data in payloads:
        boundaries = [i*len(data)//parts for i in range(parts+1)]
        extents = [{"object_key":key,"offset":offset+a,"length":b-a} for a,b in zip(boundaries,boundaries[1:])]
        assert all(e["length"]>0 for e in extents)
        sources.append(manifest(data,extents))
        offset += len(data)
    return sources


def register(state, root, body, peer, expected):
    before, started = counts(), time.monotonic()
    response = run.request("POST",f"/roots/{root}/versions",body,key=state["key"],server=peer,statuses=(202,))
    delta = [b-a for a,b in zip(before,counts())]
    assert delta == expected, f"capture statement counts: {delta}, expected {expected}"
    print(json.dumps({"event":"capture_batch","files":len(body["files"]),"statements":delta,
        "request_seconds":round(time.monotonic()-started,3)}),flush=True)
    return response


def durable(root, body, response):
    rows = run.sql("""SELECT f.path,f.captured_version_id,v.*,e.id AS extraction_id,e.revision,
        e.status AS extraction_status,w.id AS work_id,w.stage,
        (SELECT count(*) FROM file_version_extents x WHERE x.version_id=v.id) AS extents
        FROM file_catalog f JOIN file_versions v ON v.file_id=f.id
        JOIN file_extractions e ON e.version_id=v.id JOIN file_work w ON w.extraction_id=e.id
        WHERE f.root_id=%s AND v.capture_id=%s AND w.id=ANY(%s)""",
        (root,body["capture_id"],[r["work_id"] for r in response["versions"]]))
    stored = {r["path"]:r for r in rows}
    assert len(stored)==len(body["files"])
    for file,result in zip(body["files"],response["versions"]):
        row=stored[file["path"]]
        assert row["id"]==result["version_id"] and row["sequence"]==result["sequence"]
        assert all(row[key]==result[key] for key in ("file_id","extraction_id","work_id","stage"))
        assert row["deleted"]==file.get("deleted",False)
        assert row["extents"]==len(file.get("source",{}).get("extents",[]))
        if file.get("deleted"):
            assert row["extraction_status"]=="complete" and row["stage"]=="index"
        else:
            assert row["content_hash"]==file["source"]["content_hash"] and row["size_bytes"]==file["source"]["size"]
    return stored


def published(state, root, expected):
    files=run.wait_indexed(state,root)
    assert set(files)==set(expected)
    peers=servers()
    for i,(path,text) in enumerate(expected.items()):
        if text is None:
            assert files[path]["deleted"]
            run.request("POST",f"/roots/{root}/read",{"path":path,"lines":{"start":1,"end":1}},
                key=state["key"],server=peers[i%2],statuses=(404,))
            continue
        run.assert_source_retained(files[path])
        read=run.request("POST",f"/roots/{root}/read",{"path":path,"lines":{"start":1,"end":20}},
            key=state["key"],server=peers[i%2],statuses=(200,) if text else (400,))
        if text:
            assert [line["content"] for line in read["lines"]]==text.splitlines()
        else:
            assert "unavailable" in read["error"]
    for peer in peers:
        hits=run.request("POST","/query",{"root_id":root,"query":"calibration","mode":"fts","top_k":200},
            key=state["key"],server=peer)["results"]
        assert hits
        assert all(expected[hit["file_path"]] is not None and hit["content"] in expected[hit["file_path"]] for hit in hits)
    return files


def verify():
    state=run.provision()
    peers=servers()
    # Start actual signed-upload expiry while the other workflows run. There
    # are no timestamp edits or application fault modes in this scenario.
    begin_retention(state)
    root=run.new_root(state,"Capture batch limit","/state/capture-batches",True)
    state["root"]=root
    run.save(state)
    payloads=[f"Orchid calibration record {i}: pressure and temperature measurements.\n".encode() for i in range(128)]
    sources=packed(state,root,payloads)
    body={"capture_id":str(uuid.uuid4()),"files":[{"path":f"records/{i}.txt","source":source} for i,source in enumerate(sources)]}
    response=register(state,root,body,peers[0],[1,1,1,1,0,0])
    durable(root,body,response)
    expected={file["path"]:data.decode() for file,data in zip(body["files"],payloads)}
    published(state,root,expected)
    stamps=run.sql("SELECT v.id,v.xmin::text AS revision FROM file_versions v JOIN file_catalog f ON f.id=v.file_id WHERE f.root_id=%s ORDER BY v.id",(root,))
    for peer in peers:
        assert register(state,root,body,peer,[1,1,0,0,0,0])==response
        assert run.sql("SELECT v.id,v.xmin::text AS revision FROM file_versions v JOIN file_catalog f ON f.id=v.file_id WHERE f.root_id=%s ORDER BY v.id",(root,))==stamps

    # Every append reuses three authorized spans and adds a range in a shared
    # fresh pack. One SQL validation covers every file and all 512 spans.
    additions=[f"Additional calibration sample {i}.\n".encode() for i in range(128)]
    tails=packed(state,root,additions,parts=1)
    updated={"capture_id":str(uuid.uuid4()),"files":[]}
    for file,result,data,tail,extra in zip(body["files"],response["versions"],payloads,tails,additions):
        updated["files"].append({"path":file["path"],"previous_version_id":result["version_id"],
            "source":manifest(data+extra,file["source"]["extents"]+tail["extents"])})
        expected[file["path"]]=(data+extra).decode()
    latest=register(state,root,updated,peers[1],[1,1,1,1,0,0])
    durable(root,updated,latest)
    published(state,root,expected)
    # The original receipt remains replayable and must not restore old heads.
    assert register(state,root,body,peers[1],[1,1,0,0,0,0])==response
    current=run.catalog(state,root)
    assert [current[f["path"]]["version_id"] for f in body["files"]]==[r["version_id"] for r in latest["versions"]]

    # Mixed historical receipts and new paths may share a capture ID, but new
    # paths require fresh authorized bytes, not an old capture's bound pack.
    extra=b"Independent calibration fixture bytes.\n"
    extra_source=packed(state,root,[extra],parts=1)[0]
    mixed={"capture_id":body["capture_id"],"files":body["files"][:2]+[{"path":"extra.txt","source":extra_source}]}
    accepted=register(state,root,mixed,peers[0],[1,1,1,1,0,0])
    assert accepted["versions"][:2]==response["versions"][:2]
    durable(root,mixed,accepted)
    expected["extra.txt"]=extra.decode()
    forged={"capture_id":body["capture_id"],"files":[{"path":"grafted.txt","source":sources[0]}]}
    denied=run.request("POST",f"/roots/{root}/versions",forged,key=state["key"],server=peers[1],statuses=(400,))
    assert "source extent" in denied["error"]
    assert not run.sql("SELECT id FROM file_catalog WHERE root_id=%s AND path=%s",(root,"grafted.txt"))
    bad=json.loads(json.dumps(body))
    bad["files"][0]["previous_version_id"]=str(uuid.uuid4())
    rejected=run.request("POST",f"/roots/{root}/versions",bad,key=state["key"],statuses=(500,))
    assert rejected["error"].startswith("capture ID reused with different metadata")
    # Empty files and tombstones need no source-object validation; their
    # extraction/work records still commit with the captured heads.
    deleted={"capture_id":str(uuid.uuid4()),"files":[{"path":updated["files"][0]["path"],
        "previous_version_id":latest["versions"][0]["version_id"],"deleted":True},
        {"path":"empty.txt","source":manifest(b"",[])}]}
    durable(root,deleted,register(state,root,deleted,peers[0],[1,1,0,1,0,0]))
    expected[deleted["files"][0]["path"]]=None
    expected["empty.txt"]=""
    published(state,root,expected)

    race=run.new_root(state,"Concurrent capture","/state/capture-race",True)
    race_source=packed(state,race,[b"Initial calibration concurrent capture.\n"],parts=1)[0]
    same={"capture_id":str(uuid.uuid4()),"files":[{"path":"shared.txt","source":race_source}]}
    responses=parallel(peers,8,lambda peer,_:run.request("POST",f"/roots/{race}/versions",same,key=state["key"],server=peer,statuses=(202,)))
    assert all(r==responses[0] for r in responses)
    durable(race,same,responses[0])
    contenders=[]
    for i in range(2):
        data=f"Winning calibration candidate {i}.\n".encode()
        source=packed(state,race,[data],parts=1)[0]
        contenders.append({"capture_id":str(uuid.uuid4()),"files":[{"path":"shared.txt",
            "previous_version_id":responses[0]["versions"][0]["version_id"],"source":source}]})
    outcomes=parallel(peers,2,lambda peer,i:run.request("POST",f"/roots/{race}/versions",contenders[i],key=state["key"],server=peer,statuses=(202,409)))
    winner,= [i for i,r in enumerate(outcomes) if "versions" in r]
    loser=1-winner
    assert outcomes[loser]["code"]=="capture_version_conflict"
    key=contenders[loser]["files"][0]["source"]["extents"][0]["object_key"]
    assert run.sql("SELECT capture_id FROM source_objects WHERE object_key=%s",(key,))[0]["capture_id"] is None
    published(state,race,{"shared.txt":f"Winning calibration candidate {winner}.\n"})
    assert run.sql("SELECT count(*) AS n FROM file_versions v JOIN file_catalog f ON f.id=v.file_id WHERE f.root_id=%s",(race,))[0]["n"]==2
    print("128-file creation/append, constant SQL calls, immutable/mixed retries, empty/deleted files and two-server capture races passed.",flush=True)

    check_tenant_uploads(state)
    check_packed_isolation(state)
    check_capture_acl_race(state)
    check_capture_revocations(state)
    state["capture_expected"]=expected
    run.save(state)
    finish_retention(state)
    print("Capture provenance, revocations during S3 IO, actual source expiry, historical retry and retained-byte re-upload passed.",flush=True)


def restarted():
    state=json.loads(run.STATE.read_text())
    published(state,state["root"],state["capture_expected"])
    print("Captured batch contents, tombstones and search/read survived both API restarts.",flush=True)


if __name__=="__main__":
    {"verify":verify,"restarted":restarted}[sys.argv[1]]()
