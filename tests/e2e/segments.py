"""Large-file turns and append reuse through real processes and network faults."""
import hashlib
import json
from pathlib import Path
import sys
import time
import urllib.request

from api_access import servers
from index_recovery import arm, held, published, relay
import run

DIRECTORY = Path('/state/segments')


def initial_text():
    lines = [f'segmentprefix calibration {i:05d} ' + 'observatory 測定 ' * 240 + '\n' for i in range(2300)]
    return ''.join(lines) + 'appendboundary data:image/png;base64,QUJDRA=='


def source_events():
    request = urllib.request.Request('http://source-relay:8080/status', headers={'X-E2E-Control': 'e2e-upload-fault-only'})
    with urllib.request.urlopen(request, timeout=10) as response:
        return json.load(response)['events']


def source_bytes(state, since=0):
    prefix = f"sources/{state['org']}/{state['root']}/"
    return sum(e.get('bytes', 0) for e in source_events() if e.get('operation') == 'get'
        and e.get('status') == 206 and e.get('at', 0) >= since
        and e['key'].startswith(prefix) and ('/packs/' in e['key'] or '/multipart/' in e['key']))


def work(state, version=None):
    rows = run.sql("""SELECT w.*,e.chunk_count,e.chunks_ref,e.row_format,e.source_verified,
        e.status AS extraction_status,e.append_chunk_count,e.append_checkpoint_ref,v.size_bytes
        FROM file_catalog f JOIN file_versions v ON v.id=COALESCE(%s,f.captured_version_id)
        JOIN file_extractions e ON e.version_id=v.id JOIN file_work w ON w.extraction_id=e.id
        WHERE f.root_id=%s AND f.path='record.txt' ORDER BY e.sequence DESC LIMIT 1""", (version, state['root']))
    assert len(rows) == 1
    return rows[0]


def capture():
    state = run.provision()
    DIRECTORY.mkdir()
    raw = initial_text()
    assert len(raw.encode()) > 8 * 1024 * 1024
    (DIRECTORY/'record.txt').write_text(raw)
    state['root'] = run.new_root(state, 'Segmented capture', DIRECTORY, True)
    namespace = run.sql('SELECT namespace FROM root_index_namespaces WHERE root_id=%s AND retired_at IS NULL', (state['root'],))[0]['namespace']
    fault = relay('POST', '/fault', {'namespaces':[namespace], 'mode':'hold_response', 'skip':1})['fault_id']
    run.save(state)
    run.cli(state, 'sync', str(DIRECTORY), '--id', state['root'], '--no-vector')
    held(fault, 'response_held')
    row = work(state)
    assert row['row_format'] == 2 and row['transform_cursor'] == 4 * 1024 * 1024
    assert not row['source_verified'] and row['extraction_status'] == 'pending'
    assert row['index_cursor'] == 512 and row['chunk_count'] > 512
    assert not run.catalog(state)['record.txt']['indexed_version_id']
    for peer in servers():
        assert not run.request('POST', '/query', {'root_id':state['root'], 'query':'segmentprefix', 'mode':'fts'}, key=state['key'], server=peer)['results']
    state.update(initial_version=run.catalog(state)['record.txt']['version_id'], first_checkpoint=row['transform_checkpoint_ref'])
    run.save(state)
    print('First 4 MiB is checkpointed and partly indexed; incomplete file is invisible.', flush=True)


def checkpointed():
    state = json.loads(run.STATE.read_text())
    row = work(state)
    assert row['status'] == 'pending' and row['attempt_token'] is None and row['lease_until'] is None
    assert row['index_cursor'] == min(1024, row['chunk_count'])
    assert row['transform_cursor'] == 4*1024*1024 and row['transform_checkpoint_ref'] == state['first_checkpoint']
    assert source_bytes(state) == 4*1024*1024
    print('Shutdown retained both confirmed index progress and the source/parser checkpoint.', flush=True)


def expected_text(raw):
    # Fixture expectations are supplied explicitly; no production parser imported.
    return raw.replace('QUJDRA==REVG', '[base64 image]').replace('QUJDRA==', '[base64 image]')


def verify(state, expected):
    file = run.catalog(state)['record.txt']
    row = work(state)
    assert row['status'] == 'complete' and row['source_verified']
    assert row['transform_cursor'] == file['size'] and row['index_cursor'] == row['chunk_count']
    records = list(run.chunks(row['chunks_ref']))
    assert len(records) == row['chunk_count'] and ''.join(r['content'] for r in records) == expected
    assert [r['chunk_index'] for r in records] == list(range(len(records)))
    source = run.assert_source_retained(file)
    assert source['content_hash'] == 'sha256:'+hashlib.sha256((DIRECTORY/'record.txt').read_bytes()).hexdigest()
    segments = run.sql('SELECT s.* FROM extraction_segments m JOIN file_segments s ON s.id=m.segment_id WHERE m.extraction_id=%s ORDER BY m.ordinal_start', (row['extraction_id'],))
    assert all(1 <= s['chunk_count'] <= 64 and s['indexed_at'] and not s['retired_at'] for s in segments)
    lines = expected.splitlines()
    for peer in servers():
        for start, end in ((1, 3), (62, 67), (1022, 1028), (len(lines)-2, len(lines))):
            start = max(1, start)
            end = min(end, len(lines))
            if start > end:
                continue
            result = run.request('POST', f"/roots/{state['root']}/read", {'path':'record.txt', 'lines':{'start':start,'end':end}}, key=state['key'], server=peer)
            assert [line['content'] for line in result['lines']] == lines[start-1:end]
        assert run.request('POST', '/query', {'root_id':state['root'], 'query':'segmentprefix', 'mode':'fts'}, key=state['key'], server=peer)['results']
        error = run.request('POST',f"/roots/{state['root']}/read",{'path':'record.txt',
            'lines':{'start':len(lines)+1,'end':len(lines)+1}},key=state['key'],server=peer,statuses=(400,))
        assert f'indexed line range is 1:{len(lines)}' in error['error']
    run.request('POST', f"/roots/{state['root']}/read", {'path':'record.txt','lines':{'start':1,'end':1}}, key=state['outsider_key'], statuses=(403,404))
    return row, segments


def recovered():
    state = json.loads(run.STATE.read_text())
    published(state, state['root'], state['initial_version'])
    row, segments = verify(state, expected_text(initial_text()))
    assert source_bytes(state) == len(initial_text().encode()), 'source prefix was downloaded again after restart'
    assert row['append_chunk_count'] >= 2048
    state['stable_segments'] = [s['id'] for s in segments if s['ordinal_start'] < row['append_chunk_count']]
    state['initial_extraction'] = row['extraction_id']
    state['initial_prefix_chunks'] = row['append_chunk_count']
    viewer = run.request('POST', '/admin/users', {'email':f"proof-{state['nonce']}@example.invalid",
        'name':'segment-proof-reader'})['id']
    state['users'].append(viewer)
    run.save(state)
    run.request('PUT', f"/admin/orgs/{state['org']}/members/{viewer}", {'role':'viewer'})
    run.request('POST', f"/admin/orgs/{state['org']}/roots/{state['root']}/grants",
        {'principal_type':'user','principal_id':viewer,'permissions':['sync']}, statuses=(201,))
    state['proof_reader'] = run.request('POST', f"/admin/orgs/{state['org']}/users/{viewer}/api-keys",
        {'name':'segment-prefix-reader','scopes':['query','sync']}, statuses=(201,))['key']
    prove_current(state)
    run.save(state)
    print('All source turns resumed without rereading bytes; segment reads/search and atomic publication passed.', flush=True)


def append():
    state = json.loads(run.STATE.read_text())
    state['append_started'] = time.time()
    state['append_before_writes'] = [e['id'] for e in relay('GET','/status')['events']]
    suffix = 'REVG! appended segmenttail\nnew final line 測定\n'
    state['append_suffix'] = suffix
    with (DIRECTORY/'record.txt').open('a') as output:
        output.write(suffix)
    fault = arm(state['root'], 'hold_response')
    run.save(state)
    run.cli(state, 'sync', str(DIRECTORY), '--id', state['root'], '--no-vector')
    held(fault, 'response_held')
    row = work(state)
    assert row['index_cursor'] == state['initial_prefix_chunks']
    assert source_bytes(state, state['append_started']) == len(suffix.encode())
    assert run.catalog(state)['record.txt']['indexed_version_id'] == state['initial_version']
    state['append_version'] = run.catalog(state)['record.txt']['version_id']
    state['append_extraction'] = row['extraction_id']
    run.save(state)
    print('Append downloaded only new bytes and reused all stable segments; old version remains published.', flush=True)


def appended():
    state = json.loads(run.STATE.read_text())
    published(state, state['root'], state['append_version'])
    _, segments = verify(state, expected_text(initial_text()+state['append_suffix']))
    assert {s['id'] for s in segments if s['owner_extraction_id'] != state['append_extraction']} == set(state['stable_segments'])
    assert source_bytes(state, state['append_started']) == len(state['append_suffix'].encode())
    tail_chunks = work(state)['chunk_count'] - state['initial_prefix_chunks']
    writes = [e for e in relay('GET','/status')['events'] if e['id'] not in state['append_before_writes'] and e['operation']=='write']
    assert len(writes) == 2 and all(e['upsert_count'] == tail_chunks for e in writes)
    assert writes[0]['payload_sha256'] == writes[1]['payload_sha256']
    for peer in servers():
        result = run.request('POST',f"/roots/{state['root']}/read",{'path':'record.txt','lines':{'start':1,'end':1}},
            key=state['proof_reader'],server=peer,statuses=(400,))
        assert 'no indexed chunks' in result['error'], 'reused rows accepted a stale version proof'
        assert not run.request('POST','/query',{'root_id':state['root'],'query':'segmentprefix','mode':'fts'},
            key=state['proof_reader'],server=peer)['results']
    prove_current(state)
    def cleaned_tail():
        rows = run.sql('SELECT retired_at FROM file_segments WHERE owner_extraction_id=%s AND NOT (id=ANY(%s))', (state['initial_extraction'],state['stable_segments']))
        return rows and all(r['retired_at'] for r in rows)
    run.eventually('obsolete tail retirement without deleting shared prefix segments', cleaned_tail, 180)
    verify(state, expected_text(initial_text()+state['append_suffix']))
    print('Crash recovery replayed only the ambiguous tail; shared-prefix search/read survives segment cleanup.', flush=True)


def prove_current(state):
    file = run.catalog(state)['record.txt']
    run.request('POST',f"/roots/{state['root']}/captured-proofs",{'files':[{'path':'record.txt',
        'version_id':file['version_id'],'content_hash':file['content_hash']}]},key=state['proof_reader'])
    for peer in servers():
        result = run.request('POST',f"/roots/{state['root']}/read",{'path':'record.txt','lines':{'start':1,'end':1}},
            key=state['proof_reader'],server=peer)
        assert result['lines'][0]['content'] == initial_text().splitlines()[0]


def rewrite():
    state = json.loads(run.STATE.read_text())
    state['replacement'] = 'segmentprefix replacement 測定\nrewritten tail\n'
    (DIRECTORY/'record.txt').write_text(state['replacement'])
    run.save(state)
    run.cli(state, 'sync', str(DIRECTORY), '--id', state['root'], '--no-vector')
    run.wait_indexed(state)
    row, segments = verify(state, state['replacement'])
    assert all(s['owner_extraction_id'] == row['extraction_id'] for s in segments)
    print('Rewrite/truncation generated fresh segments and retained exact source bytes.', flush=True)


def restarted():
    state = json.loads(run.STATE.read_text())
    verify(state, state['replacement'])
    (DIRECTORY/'record.txt').unlink()
    run.cli(state, 'sync', str(DIRECTORY), '--id', state['root'], '--no-vector')
    run.wait_indexed(state)
    for peer in servers():
        run.request('POST', f"/roots/{state['root']}/read", {'path':'record.txt','lines':{'start':1,'end':1}}, key=state['key'], server=peer, statuses=(404,))
        assert not run.request('POST','/query',{'root_id':state['root'],'query':'segmentprefix','mode':'fts'},key=state['key'],server=peer)['results']
    print('Segment publications survived service restarts; deletion is hidden from both APIs.', flush=True)


if __name__ == '__main__':
    phases = {name:globals()[name] for name in ('capture','checkpointed','recovered','append','appended','rewrite','restarted')}
    phases['release'] = lambda: relay('POST','/release')
    phases[sys.argv[1]]()
