"""Live lease-renewal failure through a stopped Postgres process."""
import json
import sys
from index_checkpoints import capture, recovered
import run


def verify():
    recovered()
    state=json.loads(run.STATE.read_text())
    case=state['checkpoint_cases'][-1]
    row,=run.sql('SELECT error,attempt_count FROM file_work WHERE id=%s',(case['work']['id'],))
    assert row['error'] == 'RuntimeError: index lease renewal failed'
    assert row['attempt_count'] == 2
    print('Live worker stopped before the next provider batch after background renewal failed; the same process retried and published exact source bytes.',flush=True)

if __name__ == '__main__':
    {'capture':capture,'verify':verify}[sys.argv[1]]()
