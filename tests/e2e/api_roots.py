"""Atomic root/directory creation through two APIs, real CLI and shared services."""

from concurrent.futures import ThreadPoolExecutor
import hashlib
import json
import os
from pathlib import Path
import threading
import uuid

from api_access import servers
from api_keys import held_body
from browser_session import session
import run


def counts():
    patterns = ("WITH actor AS (%INSERT INTO roots%", "INSERT INTO roots%",
                "INSERT INTO root_index_namespaces%", "SELECT u.id, u.email, u.name, u.avatar_url, om.role, om.joined_at%")
    return [run.sql("SELECT COALESCE(sum(calls),0)::bigint AS n FROM pg_stat_statements WHERE query LIKE %s", (p,))[0]["n"] for p in patterns]


def directory(root, count):
    rows = run.sql("SELECT * FROM root_index_namespaces WHERE root_id=%s ORDER BY shard_index", (root['id'],))
    prefix = "pfs_" + hashlib.sha256(root['org_id'].encode()).hexdigest()[:10] + "_" + hashlib.sha256(root['id'].encode()).hexdigest()[:10] + "_s"
    assert len(rows) == count and len({row['id'] for row in rows}) == count
    assert [row['shard_index'] for row in rows] == list(range(count))
    assert all(row['shard_count'] == count and row['org_id'] == root['org_id'] and row['retired_at'] is None for row in rows)
    assert [row['namespace'] for row in rows] == [prefix + f"{i:03d}" for i in range(count)]
    assert all(uuid.UUID(row['id']).version == 4 for row in rows)


def verify():
    state = json.loads(run.STATE.read_text())
    peers = servers()
    org, viewer = state['org'], state['users'][1]
    member = f"/admin/orgs/{org}/members/{viewer}"
    endpoint = f"/admin/orgs/{org}/roots"
    shard_count = int(os.environ['PUFFERFS_TP_NAMESPACE_SHARDS'])
    created, keep = [], None
    mutex = threading.Lock()

    def remember(root):
        with mutex:
            created.append(root['id'])
            state['roots'].append(root['id'])
            run.save(state)
        return root

    def create(body, *, key=None, cookie=None, admin=False, peer=peers[0], status=201, statements=1):
        before = counts()
        result = run.request('POST', endpoint if admin else '/roots', body, key=key, cookie=cookie, server=peer, statuses=(status,))
        assert [b-a for a,b in zip(before, counts())] == [statements, 0, 0, 0], 'root creation used extra SQL round trips'
        if status == 201:
            remember(result)
            directory(result, shard_count)
        else:
            assert not run.sql('SELECT id FROM roots WHERE org_id=%s AND name=%s', (org, body['name']))
        return result

    def mint(scopes):
        return run.request('POST', f"/admin/orgs/{org}/users/{viewer}/api-keys", {'name':'root-create','scopes':scopes}, statuses=(201,))['key']

    try:
        for peer in peers:
            for scope in ('org','restricted','user'):
                body = {'name':'Provisioned '+scope, 'scope':scope, 'owner_user_id':viewer, 'disable_vector':True}
                root = create(body, admin=True, peer=peer)
                assert root.get('owner_user_id','') == (viewer if scope == 'user' else '') and root['vector_disabled']
            create({'name':'Invalid owner','scope':'user','owner_user_id':'missing-member'}, admin=True, peer=peer, status=400)
            create({'name':'Missing owner','scope':'user'}, admin=True, peer=peer, status=400, statements=0)
            create({'name':'Invalid scope','scope':'unrecognized'}, admin=True, peer=peer, status=400, statements=0)
            for scope in ('sync','write','*'):
                root = create({'name':'Self '+scope,'scope':'user'}, key=mint([scope]), peer=peer)
                assert root['owner_user_id'] == viewer and not root['vector_disabled']
            create({'name':'Viewer org','scope':'org'}, key=mint(['sync']), peer=peer, status=403, statements=0)
            create({'name':'Admin alias alone','scope':'user'}, key=mint(['admin']), peer=peer, status=403, statements=0)
        cookie = session(viewer, org, role='viewer')
        create({'name':'Session self','scope':'user'}, cookie=cookie)
        run.request('PUT', member, {'role':'editor'}, server=peers[1])
        create({'name':'Editor org'}, key=mint(['sync']))
        create({'name':'Editor restricted','scope':'restricted'}, key=mint(['sync','org:admin']), status=403, statements=0)
        run.request('PUT', member, {'role':'admin'}, server=peers[1])
        create({'name':'Scoped restricted','scope':'restricted'}, key=mint(['sync','org:admin']))
        create({'name':'Missing admin scope','scope':'restricted'}, key=mint(['sync']), status=403, statements=0)
        create({'name':'Another owner','scope':'user','owner_user_id':state['users'][0]}, key=mint(['sync']))

        # Changes committed after middleware auth but before the body arrives
        # must be observed by the creation statement on the other API process.
        run.request('PUT', member, {'role':'editor'}, server=peers[1])
        with held_body(peers[0], '/roots', {'name':'After demotion','scope':'org'}, cookie=cookie) as finish:
            run.request('PUT', member, {'role':'viewer'}, server=peers[1])
            assert finish(403)['error'] == 'root creation is no longer authorized'
        key = mint(['sync'])
        key_id = run.sql('SELECT id FROM api_keys WHERE key_hash=%s', (hashlib.sha256(key.encode()).hexdigest(),))[0]['id']
        with held_body(peers[0], '/roots', {'name':'After revocation','scope':'user'}, key=key) as finish:
            run.request('DELETE', '/auth/api-keys/'+key_id, key=state['key'], server=peers[1])
            finish(403)
        for path, body, credential, expected in (
            ('/roots', {'name':'Removed creator','scope':'user'}, {'cookie':cookie}, 403),
            (endpoint, {'name':'Removed owner','scope':'user','owner_user_id':viewer}, {'key':os.environ['PUFFERFS_ADMIN_KEY']}, 400)):
            try:
                with held_body(peers[0], path, body, **credential) as finish:
                    run.request('DELETE', f'/org/members/{viewer}', key=state['key'], server=peers[1])
                    finish(expected)
                assert not run.sql('SELECT id FROM roots WHERE org_id=%s AND name=%s', (org,body['name']))
            finally:
                run.request('PUT', member, {'role':'viewer'}, server=peers[1])
        assert not run.sql("SELECT id FROM roots WHERE org_id=%s AND name=ANY(%s)", (org,['After demotion','After revocation']))

        # Both replicas create roots concurrently. A read-only observer must
        # never see a root with only part of its namespace directory committed.
        before = counts()
        barrier = threading.Barrier(13)
        def concurrent(index):
            barrier.wait(timeout=20)
            root = run.request('POST', '/roots', {'name':'Concurrent root','scope':'user'}, key=state['key'], server=peers[index%2], statuses=(201,))
            remember(root)
            return root
        with ThreadPoolExecutor(max_workers=12) as pool:
            futures = [pool.submit(concurrent,i) for i in range(12)]
            barrier.wait(timeout=20)
            while not all(f.done() for f in futures):
                assert not run.sql("SELECT r.id FROM roots r WHERE r.org_id=%s AND r.name=%s AND (SELECT count(*) FROM root_index_namespaces n WHERE n.root_id=r.id)<>%s", (org,'Concurrent root',shard_count))
            roots = [future.result() for future in futures]
        assert [b-a for a,b in zip(before,counts())] == [12,0,0,0]
        assert len({root['id'] for root in roots}) == 12
        for root in roots:
            directory(root,shard_count)

        for delayed in (True, False):
            other = run.request('POST','/admin/orgs',{'name':'Root creation race','slug':'root-'+uuid.uuid4().hex})['id']
            state.setdefault('other_orgs',[]).append(other)
            run.save(state)
            other_roots = f'/admin/orgs/{other}/roots'
            if delayed:
                with held_body(peers[0],other_roots,{'name':'Deleted org'},key=os.environ['PUFFERFS_ADMIN_KEY']) as finish:
                    run.request('DELETE',f'/admin/orgs/{other}',server=peers[1])
                    assert finish(404)['error'] == 'org not found'
            else:
                barrier = threading.Barrier(8)
                def org_race(index):
                    barrier.wait(timeout=20)
                    if index == 7:
                        return run.request('DELETE',f'/admin/orgs/{other}',server=peers[1])
                    return run.request('POST',other_roots,{'name':'Concurrent org root'},server=peers[index%2],statuses=(201,404))
                with ThreadPoolExecutor(max_workers=8) as pool:
                    list(pool.map(org_race,range(8)))
            assert not run.sql('SELECT id FROM roots WHERE org_id=%s UNION ALL SELECT id FROM root_index_namespaces WHERE org_id=%s',(other,other))

        path = Path('/state/root-creation')
        path.mkdir()
        (path/'record.txt').write_text('Marigold root creation remains searchable.\n')
        root = create({'name':'Durable root creation','scope':'restricted','source_path':str(path),'vector_disabled':True}, admin=True)
        run.cli(state,'sync',str(path),'--id',root['id'],'--no-vector')
        run.wait_indexed(state,root['id'])
        keep = root['id']
        state['created_root'] = root
        run.save(state)
        restarted()
    finally:
        run.request('PUT',member,{'role':'viewer'},server=peers[1])
        for root in created:
            if root != keep:
                run.request('DELETE',f'/roots/{root}',key=state['key'],statuses=(200,404))
                state['roots'].remove(root)
        run.save(state)
    print('One-statement root/directory creation passed scopes, current roles/keys/memberships, twelve concurrent creations and CLI publication across two APIs.',flush=True)


def restarted():
    state = json.loads(run.STATE.read_text())
    root = state['created_root']
    directory(root,int(os.environ['PUFFERFS_TP_NAMESPACE_SHARDS']))
    for peer in servers():
        result = run.request('POST',f"/roots/{root['id']}/read",{'path':'record.txt','lines':{'start':1,'end':1}},key=state['key'],server=peer)
        assert result['lines'][0]['content'] == 'Marigold root creation remains searchable.'
        assert run.request('POST','/query',{'root_id':root['id'],'query':'Marigold','mode':'fts'},key=state['key'],server=peer)['results']
    print('Complete namespace directory and CLI-published content are readable through both API processes.',flush=True)


if __name__=='__main__':
    import sys
    {'verify':verify,'restarted':restarted}[sys.argv[1]]()
