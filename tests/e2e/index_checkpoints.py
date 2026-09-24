"""Three-batch indexing interrupted through a real process/network boundary."""
import json
from pathlib import Path
import sys
import time

from api_access import servers
from index_recovery import relay, held, work, published, object_stamp
import run


def fixture_lines():
    return [f"Calibration record {i}: " + "Orchid observatory tracks telescope alignment and temperature. "*60+"\n" for i in range(1025)]


def capture():
    state = json.loads(run.STATE.read_text()) if run.STATE.exists() else run.provision()
    directory = Path('/state/checkpoint-'+str(len(state.get('checkpoint_cases',[]))))
    directory.mkdir()
    lines = fixture_lines()
    assert all(3000 < len(line.encode()) < 6000 for line in lines)
    (directory/'record.txt').write_text(''.join(lines))
    root = run.new_root(state,'Interrupted multi-batch index',directory,True)
    state['root'] = root
    names = run.sql('SELECT namespace FROM root_index_namespaces WHERE root_id=%s AND retired_at IS NULL',(root,))
    fault = relay('POST','/fault',{'namespaces':[n['namespace'] for n in names],'mode':'hold_response','skip':1})['fault_id']
    run.save(state)
    run.cli(state,'sync',str(directory),'--id',root,'--no-vector')
    event = held(fault,'response_held')
    assert event['upstream_status'] == 200
    file = run.catalog(state,root)['record.txt']
    row = work(root,file['version_id'])
    records = list(run.chunks(row['chunks_ref']))
    assert len(records) == 1025
    assert [r['chunk_index'] for r in records] == list(range(1025))
    assert row['status'] == 'running' and row['attempt_count'] == 1
    assert row['index_cursor'] == 512
    assert not file['indexed_version_id']
    events = [e for e in relay('GET','/status')['events'] if e['namespace']==event['namespace'] and e['operation']=='write']
    assert len(events) == 2 and all(e.get('upstream_status') == 200 for e in events)
    assert events[0]['state'] == 'response_released' and events[1]['state'] == 'response_held'
    for peer in servers():
        assert not run.request('POST','/query',{'root_id':root,'query':'calibration','mode':'fts'},key=state['key'],server=peer)['results']
    case = {'root':root,'version':file['version_id'],'work':row,
        'namespace':event['namespace'],'initial_hashes':[e['payload_sha256'] for e in events],
        'stamp':object_stamp(row['chunks_ref'])}
    state.setdefault('checkpoint_cases',[]).append(case)
    run.save(state)
    print(f"Canonical chunks durable; two provider writes accepted, nothing published.",flush=True)


def verify_reads(state,case):
    file = run.catalog(state,case['root'])['record.txt']
    run.assert_source_retained(file)
    expected = [line.rstrip('\n') for line in fixture_lines()]
    for peer in servers():
        actual = []
        for offset in range(0,len(expected),1000):
            result = run.request('POST',f"/roots/{case['root']}/read",
                {'path':'record.txt','lines':{'start':offset+1,'end':min(offset+1000,len(expected))}},key=state['key'],server=peer)
            actual.extend(line['content'] for line in result['lines'])
        assert actual == expected
        assert run.request('POST','/query',{'root_id':case['root'],'query':'calibration','mode':'fts'},key=state['key'],server=peer)['results']


def recovered():
    state = json.loads(run.STATE.read_text())
    case = state['checkpoint_cases'][-1]
    published(state,case['root'],case['version'])
    row = work(case['root'],case['version'])
    # Successful bounded turns reset retry attempts before the final segment.
    assert row['status'] == 'complete' and row['attempt_count'] == case.get('final_attempt_count', 1)
    assert row['index_cursor'] == 1025
    assert row['attempt_token'] != case['work']['attempt_token']
    assert row['chunks_ref'] == case['work']['chunks_ref'] and object_stamp(row['chunks_ref']) == case['stamp']
    events = [e for e in relay('GET','/status')['events'] if e['namespace']==case['namespace'] and e['operation']=='write' and e.get('upstream_status') == 200]
    replayed = events[2:]
    second_confirmed = case.get('drained') or case.get('confirmed_second')
    assert len(events) == (3 if second_confirmed else 4)
    assert sum(e['payload_sha256'] == case['initial_hashes'][0] for e in events) == 1
    if not second_confirmed:
        assert replayed[0]['payload_sha256'] == case['initial_hashes'][1]
    assert len({e['payload_sha256'] for e in events}) == 3
    verify_reads(state,case)
    run.wait_work_idle('index')
    print(f"Resumed from confirmed chunks: {len(replayed)} remaining writes, exact bytes and all 1025 lines verified on both APIs.",flush=True)


def drained():
    state = json.loads(run.STATE.read_text())
    case = state['checkpoint_cases'][-1]
    row = work(case['root'],case['version'])
    assert row['status'] == 'pending' and row['index_cursor'] == 1024, row
    assert row['attempt_count'] == 0 and row['lease_until'] is None and row['attempt_token'] is None
    assert not run.catalog(state,case['root'])['record.txt']['indexed_version_id']
    case['drained'] = True
    run.save(state)
    print('SIGTERM saved both confirmed writes and released ownership without consuming an attempt.',flush=True)


def restarted():
    state = json.loads(run.STATE.read_text())
    for case in state['checkpoint_cases']:
        verify_reads(state,case)
    print('Checkpointed results survived both API restarts.',flush=True)


if __name__ == '__main__':
    phase=sys.argv[1]
    phases={'capture':capture,
        'recovered':recovered,'drained':drained,'restarted':restarted,'release':lambda:relay('POST','/release')}
    started,status=time.monotonic(),'failed'
    try:
        phases[phase]()
        status='passed'
    finally:
        state=json.loads(run.STATE.read_text()) if run.STATE.exists() else {}
        with run.REPORT.open('a') as output:
            output.write(json.dumps({'run_id':state.get('nonce'),'phase':'checkpoint-'+phase,'status':status,'seconds':round(time.monotonic()-started,2)})+'\n')
        print(f'checkpoint-{phase}: {status}',flush=True)
