"""Authentication shared by the work-ID endpoints, never by parsing helpers."""

import hmac
import os


def require_endpoint_secret(item: dict) -> None:
    from fastapi import HTTPException

    expected = os.environ.get("PUFFERFS_MODAL_ENDPOINT_AUTH_KEY", "")
    if not expected:
        raise HTTPException(status_code=503, detail="worker authentication is not configured")
    provided = item.get("secret_key")
    if not isinstance(provided, str) or not hmac.compare_digest(
        provided.encode("utf-8", errors="surrogatepass"), expected.encode("utf-8")
    ):
        raise HTTPException(status_code=401, detail="invalid worker authentication")


def require_work_request(item: dict) -> tuple[str, str]:
    from fastapi import HTTPException

    require_endpoint_secret(item)
    work, token = item.get("work_id"), item.get("attempt_token")
    if not isinstance(work, str) or not work or not isinstance(token, str) or not token:
        raise HTTPException(status_code=400, detail="work_id and attempt_token are required")
    return work, token
