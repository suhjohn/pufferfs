"""CLI/API-created data across the coordinated v0.8.2 -> current upgrade."""
import json
from pathlib import Path
import sys
import run


def baseline():
    state = run.provision()
    directory = Path('/state/upgrade')
    directory.mkdir()
    initial = {'unchanged.txt':'Orchid stable original.\n',
               'updated.txt':'Orchid original revision.\n', 'deleted.txt':'Orchid delete later.\n'}
    for name, text in initial.items():
        (directory/name).write_text(text)
    state['root'] = run.new_root(state, 'Upgrade continuity', directory, True)
    run.save(state)
    run.cli(state, 'sync', str(directory), '--id', state['root'], '--no-vector')
    state['baseline'] = run.wait_indexed(state)
    state['initial'] = initial
    run.save(state)
    print('Old production processes published three files; sources and catalog retained.', flush=True)


def pending():
    state = json.loads(run.STATE.read_text())
    directory = Path('/state/upgrade')
    (directory/'updated.txt').write_text('Cobalt replacement after upgrade.\n')
    (directory/'deleted.txt').unlink()
    (directory/'added.txt').write_text('Violet new pending file.\n')
    run.cli(state, 'sync', str(directory), '--id', state['root'], '--no-vector')
    def prepared():
        rows = run.sql("""SELECT f.path,w.id,w.stage,w.status,e.id AS extraction_id,e.chunks_ref
            FROM file_catalog f JOIN file_extractions e ON e.version_id=f.captured_version_id
            JOIN file_work w ON w.extraction_id=e.id WHERE f.root_id=%s
            ORDER BY f.path,w.stage""", (state['root'],))
        index = [row for row in rows if row['stage']=='index']
        return rows if len(index)==4 and all(row['status']=='pending' for row in index if row['path']!='unchanged.txt') else None
    state['before_work'] = run.eventually('old ingestion to leave publication pending', prepared, 180)
    state['before_files'] = run.catalog(state, state['root'])
    run.save(state)
    print('Old split-work schema holds published, pending replacement, addition and tombstone work.', flush=True)


def migrated():
    state = json.loads(run.STATE.read_text())
    files = run.catalog(state, state['root'])
    assert {p:f['version_id'] for p,f in files.items()} == {p:f['version_id'] for p,f in state['before_files'].items()}
    assert {p:f['indexed_version_id'] for p,f in files.items()} == {p:f['indexed_version_id'] for p,f in state['before_files'].items()}
    rows = run.sql("""SELECT f.path,w.id,w.stage,w.status,e.id AS extraction_id,e.chunks_ref
        FROM file_catalog f JOIN file_extractions e ON e.version_id=f.captured_version_id
        JOIN file_work w ON w.extraction_id=e.id WHERE f.root_id=%s ORDER BY f.path""", (state['root'],))
    assert len(rows)==4 and all(row['stage']=='index' for row in rows)
    for row in rows:
        old = [r for r in state['before_work'] if r['extraction_id']==row['extraction_id']]
        assert row['id'] == next((r['id'] for r in old if r['stage']=='transform'), old[0]['id'])
        assert row['chunks_ref']==old[0]['chunks_ref']
    print('Production migrations preserved versions, published heads, canonical chunks and one stable work identity.', flush=True)


def verified():
    state = json.loads(run.STATE.read_text())
    files = run.wait_indexed(state)
    expected = {'unchanged.txt':state['initial']['unchanged.txt'],
                'updated.txt':'Cobalt replacement after upgrade.\n','added.txt':'Violet new pending file.\n'}
    for name,text in expected.items():
        run.assert_source_retained(files[name])
        result = run.request('POST',f"/roots/{state['root']}/read",
            {'path':name,'lines':{'start':1,'end':1}},key=state['key'])
        assert result['lines'][0]['content']==text.rstrip('\n')
    assert files['deleted.txt']['deleted']
    hits = run.request('POST','/query',{'root_id':state['root'],'query':'Cobalt','mode':'fts'},key=state['key'])['results']
    assert hits and all(h['file_path']=='updated.txt' for h in hits)
    assert files['unchanged.txt']['indexed_version_id']==state['baseline']['unchanged.txt']['indexed_version_id']
    print('New workers published pending work; exact reads, replacement search and deletion survived upgrade.', flush=True)


if __name__ == '__main__':
    {'baseline':baseline,'pending':pending,'migrated':migrated,'verified':verified}[sys.argv[1]]()
