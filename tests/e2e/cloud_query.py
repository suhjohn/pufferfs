"""Temporary real Modal query app; never deploys/replaces a production app.

Run from the repository with Modal credentials and MODAL_SECRET_KEY matching
the configured endpoint-auth secret. Only synthetic query strings leave here.
"""

from concurrent.futures import ThreadPoolExecutor
import json
import math
import os
from pathlib import Path
import sys
import urllib.error
import urllib.request
import uuid


def main():
    import modal

    key = os.environ.get("MODAL_SECRET_KEY")
    if not key:
        raise RuntimeError("MODAL_SECRET_KEY must match the configured Modal endpoint-auth secret")
    os.environ["PUFFERFS_QUERY_APP_NAME"] = "pufferfs-query-validation-" + uuid.uuid4().hex[:12]
    os.environ["PUFFERFS_QUERY_ENDPOINT_LABEL"] = os.environ["PUFFERFS_QUERY_APP_NAME"]
    os.environ["PUFFERFS_MODAL_QUERY_EMBED_MIN_CONTAINERS"] = "0"
    os.environ["PUFFERFS_MODAL_QUERY_EMBED_MAX_CONTAINERS"] = "1"
    os.environ["PUFFERFS_EMBEDDING_DEVICE"] = "cuda"
    directory = Path(__file__).resolve().parents[2] / "modal"
    sys.path.insert(0, str(directory))
    os.chdir(directory)
    import query_app

    with modal.enable_output(), query_app.app.run():
        endpoint = query_app.QueryEmbedder().embed_query_endpoint.get_web_url()
        assert endpoint and endpoint.startswith("https://")
        print("Temporary Modal query app:", query_app.app.app_id, flush=True)

        def request(payload):
            req = urllib.request.Request(endpoint, data=json.dumps(payload).encode(),
                                         headers={"Content-Type": "application/json"})
            with urllib.request.urlopen(req, timeout=300) as response:
                return json.load(response)

        for value in (None, "wrong", [], {"bad": "credential"}):
            try:
                request({"texts": ["Synthetic telescope query"], "secret_key": value})
            except urllib.error.HTTPError as error:
                assert error.code == 401
            else:
                raise AssertionError("Cloud endpoint accepted an invalid credential")

        def query(count):
            vectors = request({"texts": ["Observatory " + "telescope " * count], "secret_key": key})["embeddings"]
            assert len(vectors) == 1 and len(vectors[0]) == 768
            assert all(math.isfinite(value) for value in vectors[0])
            assert abs(sum(value * value for value in vectors[0]) - 1) < 0.02

        with ThreadPoolExecutor(max_workers=4) as pool:
            list(pool.map(query, (1, 17, 3, 25, 2, 31, 5, 13)))
        print("Cloud query role passed auth rejection and concurrent normalized-vector HTTP checks.", flush=True)
    print("Temporary app exited; no production endpoint or configuration changed.", flush=True)


if __name__ == "__main__":
    main()
