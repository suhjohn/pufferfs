"""Fenced segment preparation; one durable file job yields between bounded turns."""
from file_runtime import database
from segment_io import extraction_prefix


def lock_transform(conn, job):
    row = conn.execute("""SELECT w.transform_cursor,w.transform_checkpoint_ref,w.index_cursor,
            e.chunk_count,e.row_format,e.status AS extraction_status
        FROM file_work w JOIN file_extractions e ON e.id=w.extraction_id
        WHERE w.id=%s AND w.attempt_token=%s AND w.status='running'
          AND w.stage='transform' AND w.lease_until>NOW()
        FOR UPDATE OF w,e""", (job["id"], job["attempt_token"])).fetchone()
    if row is None:
        raise RuntimeError("segment preparation lost ownership")
    if row["transform_cursor"] != job["transform_cursor"] or row["chunk_count"] != job["chunk_count"]:
        raise RuntimeError("segment preparation checkpoint changed")
    return row


def begin_segmented_extraction(job):
    if job["row_format"] == 2:
        return
    with database() as conn:
        row = lock_transform(conn, job)
        if row["row_format"] != 1 or row["extraction_status"] != "pending" or row["chunk_count"] or row["transform_cursor"]:
            raise ValueError("only a fresh extraction can select segmented format")
        conn.execute("UPDATE file_extractions SET row_format=2 WHERE id=%s", (job["extraction_id"],))
    job["row_format"] = 2


def store_segments(conn, job, segments):
    position = job["chunk_count"]
    for segment in segments:
        if (segment["ordinal_start"] != position or not 1 <= segment["chunk_count"] <= 64
                or not segment["chunks_ref"].startswith(extraction_prefix(job) + "segments/")):
            raise ValueError("prepared segments do not extend the extraction prefix")
        expected = {**segment, "file_id": job["file_id"], "owner_extraction_id": job["extraction_id"]}
        columns = ("id", "file_id", "owner_extraction_id", "ordinal_start", "chunk_count", "chunks_ref",
            "line_start", "line_end", "page_start", "page_end")
        conn.execute("""INSERT INTO file_segments(id,file_id,owner_extraction_id,ordinal_start,chunk_count,
                chunks_ref,line_start,line_end,page_start,page_end)
            VALUES(%s,%s,%s,%s,%s,%s,%s,%s,%s,%s) ON CONFLICT(id) DO NOTHING""",
            tuple(expected[key] for key in columns))
        saved = conn.execute("SELECT * FROM file_segments WHERE id=%s FOR SHARE", (segment["id"],)).fetchone()
        if saved is None or saved["retired_at"] or any(saved[key] != expected[key] for key in columns):
            raise ValueError("immutable segment identity conflict")
        # A committed turn advances its input cursor in this transaction, so
        # recovery never inserts memberships for an already committed turn.
        conn.execute("INSERT INTO extraction_segments(extraction_id,ordinal_start,segment_id) VALUES(%s,%s,%s)",
            (job["extraction_id"], position, segment["id"]))
        position += segment["chunk_count"]
    return position


def save_segment_turn(job, segments, checkpoint_ref, source_cursor, *, complete=False,
                      chunks_ref="", append_checkpoint_ref="", append_chunk_count=0):
    prefix = extraction_prefix(job)
    if (not checkpoint_ref.startswith(prefix + "checkpoints/")
            or not job["transform_cursor"] <= source_cursor <= job["size_bytes"]
            or (complete and (source_cursor != job["size_bytes"] or not chunks_ref.startswith(prefix)) )
            or (append_checkpoint_ref and not append_checkpoint_ref.startswith(prefix + "checkpoints/"))):
        raise ValueError("invalid segmented extraction checkpoint")
    with database() as conn:
        row = lock_transform(conn, job)
        if row["row_format"] != 2 or row["extraction_status"] != "pending":
            raise ValueError("invalid segmented extraction phase")
        count = store_segments(conn, job, segments)
        if not 0 <= append_chunk_count <= count:
            raise ValueError("invalid stable append prefix")
        conn.execute("""UPDATE file_extractions SET chunk_count=%s,
                status=CASE WHEN %s THEN 'complete' ELSE 'pending' END,source_verified=%s,
                chunks_ref=%s,append_checkpoint_ref=%s,append_chunk_count=%s,error='',updated_at=NOW()
            WHERE id=%s""", (count, complete, complete, chunks_ref, append_checkpoint_ref,
                append_chunk_count, job["extraction_id"]))
        next_stage = "index" if complete or count > row["index_cursor"] else "transform"
        conn.execute("""UPDATE file_work SET stage=%s,status='pending',attempt_count=0,
                transform_cursor=%s,transform_checkpoint_ref=%s,attempt_token=NULL,lease_until=NULL,
                next_attempt_at=NOW(),error='',updated_at=NOW() WHERE id=%s""",
            (next_stage, source_cursor, checkpoint_ref, job["id"]))
    return {"status": "yielded", "stage": next_stage}


def prepared_segments(job, limit=8):
    if not 1 <= limit <= 8:
        raise ValueError("invalid segment index turn bound")
    with database() as conn:
        return conn.execute("""SELECT s.* FROM extraction_segments m JOIN file_segments s ON s.id=m.segment_id
            WHERE m.extraction_id=%s AND m.ordinal_start>=GREATEST(0,%s-63)
              AND m.ordinal_start+s.chunk_count>%s
              AND s.file_id=%s AND s.retired_at IS NULL
            ORDER BY m.ordinal_start LIMIT %s""",
            (job["extraction_id"], job["index_cursor"], job["index_cursor"], job["file_id"], limit)).fetchall()


def finish_index_turn(job, cursor):
    with database() as conn:
        row = conn.execute("""SELECT w.index_cursor,e.chunk_count,e.source_verified,e.status AS extraction_status
            FROM file_work w JOIN file_extractions e ON e.id=w.extraction_id
            WHERE w.id=%s AND w.attempt_token=%s AND w.stage='index' AND w.status='running'
              AND w.lease_until>NOW() AND e.row_format=2 FOR UPDATE OF w""",
            (job["id"], job["attempt_token"])).fetchone()
        if row is None or row["index_cursor"] != cursor:
            raise RuntimeError("segment index turn lost ownership")
        # The final turn keeps its lease for atomic whole-file publication.
        if cursor == row["chunk_count"] and row["source_verified"] and row["extraction_status"] == "complete":
            return "publish"
        stage = "transform" if cursor == row["chunk_count"] else "index"
        status = "waiting_provider" if stage == "transform" and row["extraction_status"] == "waiting_provider" else "pending"
        conn.execute("""UPDATE file_work SET stage=%s,status=%s,attempt_count=0,
            attempt_token=NULL,lease_until=NULL,next_attempt_at=NOW(),updated_at=NOW() WHERE id=%s""", (stage, status, job["id"]))
    return "yielded"
