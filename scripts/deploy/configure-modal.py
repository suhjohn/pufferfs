"""Install worker configuration from the selected Pulumi stack and CI secrets."""

import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile


def main():
    required = ("DATABASE_URL", "GEMINI_API_KEY", "TURBOPUFFER_API_KEY", "MODAL_SECRET_KEY", "MODAL_ENVIRONMENT")
    for name in required:
        if not os.environ.get(name, "").strip():
            raise RuntimeError(f"Missing {name}")
    infra = Path(__file__).resolve().parents[2] / "infra/pulumi"
    outputs = json.loads(subprocess.check_output(["pulumi", "stack", "output", "--json"], cwd=infra))
    region = os.environ.get("AWS_REGION", "us-west-2")
    values = {name: os.environ[name] for name in required[:3]}
    # Workers use short transactions and can share a transaction pooler. Keep
    # API/consumer startup migrations on the independently configured URL.
    values["DATABASE_URL"] = os.environ.get("PUFFERFS_WORKER_DATABASE_URL", "").strip() or values["DATABASE_URL"]
    values.update({
        "AWS_REGION": region, "AWS_DEFAULT_REGION": region,
        "AWS_BUCKET_NAME": outputs["artifactBucket"],
        "PUFFERFS_AWS_ROLE_ARN": outputs["modalWorkerRoleArn"],
        "PUFFERFS_SQS_TRANSFORM_QUEUE_URL": outputs["syncQueueUrls"]["transform"],
        "PUFFERFS_SQS_INDEX_QUEUE_URL": outputs["syncQueueUrls"]["index"],
    })
    for name in ("TURBOPUFFER_API_URL", "TURBOPUFFER_REGION", "PUFFERFS_TP_NAMESPACE_SHARDS"):
        if os.environ.get(name):
            values[name] = os.environ[name]
    for name, contents in (
        (os.getenv("PUFFERFS_WORKER_SECRET_NAME", "pufferfs-workers"), values),
        (os.getenv("PUFFERFS_MODAL_ENDPOINT_SECRET_NAME", "pufferfs-endpoint-auth"),
         {"PUFFERFS_MODAL_ENDPOINT_AUTH_KEY": os.environ["MODAL_SECRET_KEY"]}),
    ):
        # Modal requires a regular file. NamedTemporaryFile creates it with
        # mode 0600 and removes it after the CLI returns, including failures.
        with tempfile.NamedTemporaryFile(mode="w", suffix=".json") as config:
            json.dump(contents, config)
            config.flush()
            subprocess.run([sys.executable, "-m", "modal", "secret", "create", "--force", name,
                            "--env", os.environ["MODAL_ENVIRONMENT"], "--from-json", config.name],
                           check=True)


if __name__ == "__main__":
    main()
