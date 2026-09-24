"""Shared native embedding admission, including retries and ambiguous writes."""
import math
import os
import uuid

from file_runtime import database
from index_client import EMBEDDING_MODEL
from worker_metrics import count


class ProviderDeferred(Exception):
    def __init__(self, delay, available_tokens=0):
        self.delay = max(0.1, min(float(delay), 3600))
        self.available_tokens = available_tokens
        super().__init__("shared embedding capacity unavailable")


def limits():
    requests = int(os.getenv("PUFFERFS_EMBEDDING_REQUESTS_PER_MINUTE", "1024"))
    tokens = int(os.getenv("PUFFERFS_EMBEDDING_TOKENS_PER_MINUTE", "2000000"))
    if not 1 <= requests <= 1000000 or not 32768 <= tokens <= 1000000000000:
        raise ValueError("invalid shared embedding budget")
    return requests, tokens


def reserve(org_id, tokens):
    requests, limit = limits()
    reservation = uuid.uuid4().hex
    with database() as conn:
        result = conn.execute("SELECT * FROM reserve_provider_capacity(%s,%s,%s,%s,%s,%s,90)",
            (EMBEDDING_MODEL, org_id, reservation, tokens, requests, limit)).fetchone()
    if result["delay"]:
        count("embedding_admission_deferred")
        raise ProviderDeferred(result["delay"], result["available_tokens"])
    count("embedding_provider_attempts")
    return reservation


def settle(reservation, tokens):
    # Missing or changed provider metrics keep the conservative reservation.
    if type(tokens) is not int or tokens < 0:
        return
    with database() as conn:
        conn.execute("UPDATE provider_reservations SET tokens=%s WHERE id=%s", (tokens, reservation))


def cool_down(delay):
    with database() as conn:
        conn.execute("""UPDATE provider_capacity SET blocked_until=GREATEST(blocked_until,
            clock_timestamp()+make_interval(secs=>%s)) WHERE model=%s""", (delay, EMBEDDING_MODEL))


def write_embedding(job, tp, namespace, mutation, options):
    # UTF-8 bytes conservatively reserve text tokens plus prompt overhead.
    # Successful responses reconcile to the provider's measured token count.
    rows = mutation["upsert_rows"]
    costs = [len(row["content"].encode()) + 128 for row in rows]
    tokens = sum(costs)
    while True:
        try:
            reservation = reserve(job["org_id"], tokens)
            break
        except ProviderDeferred as capacity:
            # The next batch can use remaining token capacity without waiting
            # for enough room for its original size. Every smaller attempt is
            # atomically reserved again; this hint does not grant permission.
            remaining, length = capacity.available_tokens, 0
            for cost in costs:
                if cost > remaining:
                    break
                remaining -= cost
                length += 1
            if not 0 < length < len(rows):
                raise
            rows, costs = rows[:length], costs[:length]
            tokens = sum(costs)
    mutation = {"upsert_rows": rows}
    try:
        response = tp.namespace(namespace).write(**mutation, **options)
    except Exception as error:
        if getattr(error, "status_code", None) == 429:
            count("embedding_provider_throttled")
            try:
                retry = float(error.response.headers.get("retry-after", "5"))
                delay = max(1, min(retry, 3600)) if math.isfinite(retry) else 5
            except (ValueError, AttributeError):
                delay = 5
            # Turbopuffer rejects a throttled request in full. Keep the request
            # debit, but no embedding tokens were accepted for this attempt.
            settle(reservation, 0)
            cool_down(delay)
            raise ProviderDeferred(delay) from error
        raise
    # A bookkeeping failure must not hide a confirmed write/checkpoint. Its
    # original reservation remains, so failing closed costs capacity only.
    try:
        measured = (response.model_dump().get("performance") or {}).get("embedding_tokens")
        settle(reservation, measured)
    except Exception:
        count("embedding_settlement_deferred")
    return len(rows)
