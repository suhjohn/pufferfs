"""Local prepared inputs -> temporary Google files -> durable batch handoff."""

from contextlib import closing

from extraction import file_family
from file_runtime import database, heartbeat, stable_id
from media_prepare import media_clip_seconds, media_inputs
from provider_submission import MAX_BATCH_REQUESTS, finish_preparation, reserve_batch, submit_batch
from provider_refresh import refresh_batch_inputs, upload_prepared
from visual_prepare import visual_inputs


def prepare_provider(job, path, client, *, connect=database):
    # Resume durable batches before opening the renderer/decoder. Source bytes
    # were already hash-verified by transform(); mappings belong to this exact
    # source version and extraction revision. Keep at most 64 mappings in memory.
    count = 0
    previous_batch = None
    while True:
        with connect() as conn:
            recorded = conn.execute("""SELECT ordinal,batch_id FROM provider_requests
                WHERE extraction_id=%s AND ordinal>=%s ORDER BY ordinal LIMIT %s""",
                (job["extraction_id"], count, MAX_BATCH_REQUESTS)).fetchall()
        if not recorded:
            break
        if any(row["ordinal"] != count + i or not row["batch_id"] for i, row in enumerate(recorded)):
            raise ValueError("persisted provider preparation is not a contiguous prefix")
        for row in recorded:
            if row["batch_id"] != previous_batch:
                heartbeat(job, connect=connect)
                refresh_batch_inputs(row["batch_id"], client, path=path, connect=connect)
                submit_batch(row["batch_id"], client, connect=connect)
                previous_batch = row["batch_id"]
        count += len(recorded)

    # A range is constant-size even for a very large recorded prefix. Media
    # still decodes preceding samples for exact timing, but doesn't write WAVs.
    ordinals = range(count, 1 << 63)
    inputs = (media_inputs(path, clip_seconds=media_clip_seconds(job["revision"]), ordinals=ordinals)
              if file_family(path) in {"audio", "video"} else visual_inputs(path, ordinals=ordinals))
    batch = []
    with closing(inputs):
        for item in inputs:
            if type(item["ordinal"]) is not int or item["ordinal"] != count:
                raise ValueError("prepared provider inputs are not contiguous")
            heartbeat(job, connect=connect)
            key = stable_id(job["extraction_id"], str(count))
            uploaded = upload_prepared(client, item, key, job["extraction_id"], connect=connect)
            batch.append(dict(item, input_file_id=uploaded.name, input_uri=uploaded.uri))
            count += 1
            if len(batch) == MAX_BATCH_REQUESTS:
                batch_id = reserve_batch(job, batch, connect=connect)
                submit_batch(batch_id, client, connect=connect)
                batch.clear()
        if batch:
            batch_id = reserve_batch(job, batch, connect=connect)
            submit_batch(batch_id, client, connect=connect)
    if count:
        finish_preparation(job, count, connect=connect)
    return count
