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
    records = list(run.chunks(row['mutation_ref']))
    assert len(records) == row['mutation_batch_count'] == 3
    assert [len(r['write']['upsert_rows']) for r in records] == [512,512,1]
    assert row['status'] == 'running' and row['attempt_count'] == 1
    assert row['acknowledged_batches'] == file['processing']['acknowledged_batches'] == 0
    assert not file['indexed_version_id']
    events = [e for e in relay('GET','/status')['events'] if e['namespace']==event['namespace']]
    assert len(events) == 2 and all(e.get('upstream_status') == 200 for e in events)
    assert events[0]['state'] == 'response_released' and events[1]['state'] == 'response_held'
    for peer in servers():
        assert not run.request('POST','/query',{'root_id':root,'query':'calibration','mode':'fts'},key=state['key'],server=peer)['results']
    case = {'root':root,'version':file['version_id'],'work':row,
        'namespace':event['namespace'],'initial_hashes':[e['payload_sha256'] for e in events],
        'stamp':object_stamp(row['mutation_ref'])}
    state.setdefault('checkpoint_cases',[]).append(case)
    run.save(state)
    print(f"Three-batch artifact durable; two provider writes accepted, acknowledgment=0, nothing published.",flush=True)


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
    assert row['status'] == 'complete' and row['attempt_count'] == 2
    assert row['attempt_token'] != case['work']['attempt_token']
    assert row['acknowledged_batches'] == row['mutation_batch_count'] == 3
    assert row['mutation_ref'] == case['work']['mutation_ref'] and object_stamp(row['mutation_ref']) == case['stamp']
    events = [e for e in relay('GET','/status')['events'] if e['namespace']==case['namespace'] and e.get('upstream_status') == 200]
    replayed = events[2:]
    assert len(events) == 5
    repeated = case['initial_hashes']
    assert [e['payload_sha256'] for e in replayed[:len(repeated)]] == repeated
    assert len({e['payload_sha256'] for e in events}) == 3
    verify_reads(state,case)
    run.wait_queue_empty('index')
    print(f"Recovered full artifact: replayed {len(replayed)} batches, exact bytes and all 1025 lines verified on both APIs.",flush=True)


def restarted():
    state = json.loads(run.STATE.read_text())
    for case in state['checkpoint_cases']:
        verify_reads(state,case)
    print('Full-replay results survived both API restarts.',flush=True)


if __name__ == '__main__':
    phase=sys.argv[1]
    phases={'capture':capture,
        'recovered':recovered,'restarted':restarted,'release':lambda:relay('POST','/release')}
    started,status=time.monotonic(),'failed'
    try:
        phases[phase]()
        status='passed'
    finally:
        state=json.loads(run.STATE.read_text()) if run.STATE.exists() else {}
        with run.REPORT.open('a') as output:
            output.write(json.dumps({'run_id':state.get('nonce'),'phase':'checkpoint-'+phase,'status':status,'seconds':round(time.monotonic()-started,2)})+'\n')
        print(f'checkpoint-{phase}: {status}',flush=True)
