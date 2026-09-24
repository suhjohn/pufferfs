"""Live lease-renewal failure through a stopped Postgres process."""
import json
import sys
from index_checkpoints import capture, recovered
from index_recovery import relay
import run


def verify():
    state=json.loads(run.STATE.read_text())
    case=state['checkpoint_cases'][-1]
    def failed_attempt():
        row, = run.sql('SELECT status,error,index_cursor,attempt_count FROM file_work WHERE id=%s',
            (case['work']['id'],))
        return row if row['status'] == 'pending' and row['error'] == 'RuntimeError' else None
    row = run.eventually('failed renewal stops before the next provider write', failed_attempt, 20)
    assert row['index_cursor'] == 1024 and row['attempt_count'] == 1
    events = [e for e in relay('GET','/status')['events'] if e['namespace']==case['namespace'] and e['operation']=='write']
    assert len(events) == 2
    case['confirmed_second'] = True
    case['final_attempt_count'] = 2
    run.save(state)
    recovered()
    row,=run.sql('SELECT error,attempt_count FROM file_work WHERE id=%s',(case['work']['id'],))
    assert row['error'] == 'RuntimeError'
    assert row['attempt_count'] == 2
    print('Live worker stopped before the next provider batch after background renewal failed; the same process retried and published exact source bytes.',flush=True)

if __name__ == '__main__':
    {'capture':capture,'verify':verify}[sys.argv[1]]()
