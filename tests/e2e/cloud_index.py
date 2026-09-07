"""Provision an isolated cloud E2E, never replace a production deployment.

Requires real AWS IAM-user credentials (STS federation), Modal authentication,
provider keys, and a TLS-verified DATABASE_URL whose role can create databases
and roles. Creates a disposable database/login, bucket, four FIFO queues, and
two temporary Modal secrets/apps. Only the host keeps provisioning credentials;
runtime workers receive a dedicated DB login and resource-scoped STS session.

Run: uv run --with boto3 --with 'psycopg[binary]' --with modal tests/e2e/cloud_index.py
"""

from contextlib import ExitStack
import argparse
import json
import os
from pathlib import Path
import secrets
import ssl
import subprocess
import sys
import tempfile
from urllib.parse import parse_qs, quote, urlencode, urlsplit, urlunsplit
import uuid

import boto3
from botocore.config import Config
import modal
import psycopg
from psycopg import sql


REPOSITORY = Path(__file__).resolve().parents[2]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--scenario", choices=("cloud-index", "worker-throughput"), default="cloud-index")
    parser.add_argument("--workers", type=int, choices=range(1, 17), default=1,
                        help="Independent transform/consumer processes and maximum bulk GPU containers")
    parser.add_argument("--shards", type=int, choices=(1, 2), default=2,
                        help="Exercise single- or multiple-namespace root routing")
    args = parser.parse_args()
    for name in ("DATABASE_URL", "GEMINI_API_KEY", "TURBOPUFFER_API_KEY", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"):
        if not os.environ.get(name):
            raise RuntimeError(f"{name} is required")
    original = urlsplit(os.environ["DATABASE_URL"])
    query = parse_qs(original.query)
    if query.get("sslmode") != ["verify-full"]:
        raise RuntimeError("Provisioning DATABASE_URL must use sslmode=verify-full")
    # Host libpq must also use an installed system trust store; do not weaken
    # certificate verification to make provisioning work.
    ca = ssl.get_default_verify_paths().cafile
    if ca:
        os.environ.setdefault("SSL_CERT_FILE", ca)
    identifier = "pufferfs-cloud-" + uuid.uuid4().hex[:12]
    db_name = identifier.replace("-", "_")
    role_name = db_name + "_login"
    region = os.environ.get("AWS_REGION") or os.environ.get("AWS_DEFAULT_REGION") or "us-west-2"
    credentials = boto3.Session(region_name=region)
    config = Config(connect_timeout=10, read_timeout=30, retries={"total_max_attempts": 2})
    s3, sqs, sts = (credentials.client(service, config=config) for service in ("s3", "sqs", "sts"))
    if any(not client.meta.endpoint_url.endswith(".amazonaws.com") for client in (s3, sqs, sts)):
        raise RuntimeError("Cloud E2E refuses emulator endpoints")
    identity = sts.get_caller_identity()
    if ":user/" not in identity["Arn"]:
        raise RuntimeError("STS federation requires an IAM-user provisioning identity")
    print(json.dumps({"cloud_run": identifier, "account": identity["Account"], "region": region, "workers": args.workers, "shards": args.shards}), flush=True)
    recovery = Path(tempfile.mkdtemp(prefix=identifier + "-"))
    state_path = recovery / "resources.json"
    state = {"identifier": identifier, "database": None, "role": None, "bucket": None,
             "queues": [], "secrets": [], "compose_started": False, "application_cleaned": False}
    runtime = None

    def save():
        # Restrictive mode at creation, not a later chmod window. Retained only
        # if cleanup fails; contains temporary runtime credentials for recovery.
        descriptor = os.open(state_path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with os.fdopen(descriptor, "w") as output:
            json.dump(state, output)

    def compose(*args, check=True, log=None):
        command = ["docker", "compose", "--env-file", "/dev/null", "--profile", "test", "-p", identifier,
                   "-f", str(REPOSITORY / "compose.e2e-cloud.yml"), *args]
        with subprocess.Popen(command, cwd=REPOSITORY, env=runtime, stdout=subprocess.PIPE, stderr=subprocess.STDOUT) as process:
            # Redaction runs with these temporary secrets, not only the host's
            # original .env values. Never print a rendered Compose configuration.
            with subprocess.Popen([sys.executable, "-u", str(REPOSITORY / "tests/e2e/redact.py")],
                                  env=runtime, stdin=process.stdout, stdout=log) as redactor:
                process.stdout.close()
                redactor.wait()
            result = process.wait()
        if check and (result or redactor.returncode):
            raise RuntimeError("Cloud Compose operation failed; inspect sanitized diagnostics")
        return result

    save()
    failure = None
    try:
        password = secrets.token_urlsafe(36)
        with psycopg.connect(os.environ["DATABASE_URL"], autocommit=True, connect_timeout=15) as admin:
            admin.execute(sql.SQL("CREATE ROLE {} LOGIN PASSWORD {}").format(sql.Identifier(role_name), sql.Literal(password)))
            state["role"] = role_name
            save()
            admin.execute(sql.SQL("GRANT {} TO CURRENT_USER").format(sql.Identifier(role_name)))
            admin.execute(sql.SQL("CREATE DATABASE {} OWNER {} TEMPLATE template0").format(sql.Identifier(db_name), sql.Identifier(role_name)))
            state["database"] = db_name
            save()
            admin.execute(sql.SQL("REVOKE CONNECT ON DATABASE {} FROM PUBLIC").format(sql.Identifier(db_name)))
        # One portable explicit CA filename shared by Debian/distroless images.
        # Preserve all remaining transport options from the supplied connection.
        query["sslrootcert"] = ["/etc/ssl/certs/ca-certificates.crt"]
        hostname = f"[{original.hostname}]" if ":" in original.hostname else original.hostname
        # Managed Postgres routers may require a branch/tenant suffix on login
        # usernames even though the SQL role itself has no suffix. Supply that
        # transport setting explicitly; never recognize a developer's account.
        login = role_name + os.environ.get("PUFFERFS_CLOUD_DB_LOGIN_SUFFIX", "")
        authority = f"{quote(login)}:{quote(password)}@{hostname}:{original.port or 5432}"
        database_url = urlunsplit((original.scheme, authority, "/" + db_name, urlencode(query, doseq=True), ""))
        with psycopg.connect(database_url, sslrootcert=ca or "system", connect_timeout=15,
                             options="-c default_transaction_read_only=on") as probe:
            assert probe.execute("SELECT current_database(),current_user").fetchone() == (db_name, role_name)
        options = {"Bucket": identifier}
        if region != "us-east-1":
            options["CreateBucketConfiguration"] = {"LocationConstraint": region}
        s3.create_bucket(**options)
        state["bucket"] = identifier
        save()
        s3.put_public_access_block(Bucket=identifier, PublicAccessBlockConfiguration={name: True for name in
            ("BlockPublicAcls", "IgnorePublicAcls", "BlockPublicPolicy", "RestrictPublicBuckets")})
        s3.put_bucket_tagging(Bucket=identifier, Tagging={"TagSet": [{"Key": "purpose", "Value": "pufferfs-isolated-e2e"}]})
        s3.put_bucket_encryption(Bucket=identifier, ServerSideEncryptionConfiguration={"Rules": [{"ApplyServerSideEncryptionByDefault": {"SSEAlgorithm": "AES256"}}]})
        s3.put_bucket_policy(Bucket=identifier, Policy=json.dumps({"Version": "2012-10-17", "Statement": [{
            "Effect": "Deny", "Principal": "*", "Action": "s3:*", "Resource": [f"arn:aws:s3:::{identifier}", f"arn:aws:s3:::{identifier}/*"],
            "Condition": {"Bool": {"aws:SecureTransport": "false"}}}]}))
        urls, arns = {}, []
        for stage in ("transform", "index"):
            dlq = sqs.create_queue(QueueName=f"{identifier}-{stage}-dlq.fifo", Attributes={"FifoQueue": "true"})["QueueUrl"]
            state["queues"].append(dlq)
            save()
            arn = sqs.get_queue_attributes(QueueUrl=dlq, AttributeNames=["QueueArn"])["Attributes"]["QueueArn"]
            url = sqs.create_queue(QueueName=f"{identifier}-{stage}.fifo", Attributes={
                "FifoQueue": "true", "ContentBasedDeduplication": "false", "DeduplicationScope": "messageGroup",
                "FifoThroughputLimit": "perMessageGroupId", "VisibilityTimeout": "300", "ReceiveMessageWaitTimeSeconds": "20",
                "MessageRetentionPeriod": "1209600", "RedrivePolicy": json.dumps({"deadLetterTargetArn": arn, "maxReceiveCount": "5"}),
            })["QueueUrl"]
            state["queues"].append(url)
            save()
            urls[stage] = url
            arns.append(sqs.get_queue_attributes(QueueUrl=url, AttributeNames=["QueueArn"])["Attributes"]["QueueArn"])
        policy = {"Version": "2012-10-17", "Statement": [
            {"Effect": "Allow", "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:AbortMultipartUpload", "s3:ListMultipartUploadParts", "s3:ListBucket", "s3:ListBucketMultipartUploads"],
             "Resource": [f"arn:aws:s3:::{identifier}", f"arn:aws:s3:::{identifier}/*"]},
            {"Effect": "Allow", "Action": ["sqs:SendMessage", "sqs:ReceiveMessage", "sqs:DeleteMessage", "sqs:ChangeMessageVisibility", "sqs:GetQueueAttributes"], "Resource": arns}]}
        temporary = sts.get_federation_token(Name=identifier, Policy=json.dumps(policy), DurationSeconds=7200)["Credentials"]
        auth_key = secrets.token_urlsafe(36)
        worker_env = {"DATABASE_URL": database_url, "AWS_REGION": region, "AWS_DEFAULT_REGION": region,
            "AWS_BUCKET_NAME": identifier, "AWS_ACCESS_KEY_ID": temporary["AccessKeyId"],
            "AWS_SECRET_ACCESS_KEY": temporary["SecretAccessKey"], "AWS_SESSION_TOKEN": temporary["SessionToken"],
            "TURBOPUFFER_API_KEY": os.environ["TURBOPUFFER_API_KEY"],
            "TURBOPUFFER_API_URL": os.environ.get("TURBOPUFFER_API_URL") or f"https://{os.environ.get('TURBOPUFFER_REGION', 'gcp-us-central1')}.turbopuffer.com",
            "PUFFERFS_SQS_TRANSFORM_QUEUE_URL": urls["transform"], "PUFFERFS_SQS_INDEX_QUEUE_URL": urls["index"],
            "PUFFERFS_TP_NAMESPACE_SHARDS": str(args.shards), "PUFFERFS_EMBEDDING_DEVICE": "cuda"}
        worker_name, auth_name = identifier + "-worker", identifier + "-auth"
        for name, values in ((worker_name, worker_env), (auth_name, {"PUFFERFS_MODAL_ENDPOINT_AUTH_KEY": auth_key})):
            modal.Secret.objects.create(name, values)
            state["secrets"].append(name)
            save()
        # Isolated application and URL names: no stable production label is
        # reused even while the temporary application is running.
        os.environ.update(PUFFERFS_WORKER_SECRET_NAME=worker_name, PUFFERFS_MODAL_ENDPOINT_SECRET_NAME=auth_name,
            PUFFERFS_INDEX_GPU_APP_NAME=identifier + "-index", PUFFERFS_INDEX_GPU_ENDPOINT_LABEL=identifier + "-index",
            PUFFERFS_QUERY_APP_NAME=identifier + "-query", PUFFERFS_QUERY_ENDPOINT_LABEL=identifier + "-query", PUFFERFS_MODAL_INDEX_MAX_CONTAINERS=str(args.workers),
            PUFFERFS_MODAL_QUERY_EMBED_MIN_CONTAINERS="0", PUFFERFS_MODAL_QUERY_EMBED_MAX_CONTAINERS="1")
        sys.path.insert(0, str(REPOSITORY / "modal"))
        os.chdir(REPOSITORY / "modal")
        import index_gpu_app
        import query_app

        with ExitStack() as apps:
            apps.enter_context(modal.enable_output())
            apps.enter_context(index_gpu_app.app.run())
            apps.enter_context(query_app.app.run())
            runtime = dict(os.environ, **worker_env)
            runtime.update(MODAL_FILE_INDEX_ENDPOINT=index_gpu_app.Indexer().index.get_web_url(),
                MODAL_QUERY_EMBED_ENDPOINT=query_app.QueryEmbedder().embed_query_endpoint.get_web_url(),
                MODAL_SECRET_KEY=auth_key, PUFFERFS_ADMIN_KEY=secrets.token_urlsafe(36), JWT_SECRET=secrets.token_urlsafe(36),
                PUFFERFS_CLOUD_DB_PASSWORD=password, COMPOSE_PROJECT_NAME=identifier)
            state["runtime"] = {key: runtime[key] for key in (*worker_env, "MODAL_SECRET_KEY", "PUFFERFS_ADMIN_KEY", "JWT_SECRET", "MODAL_FILE_INDEX_ENDPOINT", "MODAL_QUERY_EMBED_ENDPOINT")}
            save()
            print(json.dumps({"bulk_app": index_gpu_app.app.app_id, "query_app": query_app.app.app_id}), flush=True)
            try:
                compose("build", "api", "transform", "e2e")
                state["compose_started"] = True
                save()
                compose("up", "-d", "--wait", "--scale", f"transform={args.workers}",
                        "--scale", f"transform-consumer={args.workers}", "--scale", f"index-consumer={args.workers}", "api", "api-ready", "transform", "index-cpu", "reconciler", "transform-consumer", "index-consumer")
                compose("run", "--rm", "--no-deps", "e2e", args.scenario)
            finally:
                if state["compose_started"]:
                    compose("stop", "transform-consumer", "index-consumer", "transform", "index-cpu", "reconciler", check=False)
                    state["application_cleaned"] = compose("run", "--rm", "--no-deps", "e2e", "cleanup", check=False) == 0
                    save()
                    log_path = REPOSITORY / "tests/e2e/artifacts" / f"{identifier}.log"
                    with log_path.open("w") as output:
                        compose("logs", "--no-color", check=False, log=output)
                    for line in log_path.read_text().splitlines():
                        if '"event":"file_work_metrics"' in line:
                            print(line, flush=True)
                    if not state["application_cleaned"]:
                        raise RuntimeError("Application cleanup failed; cloud resources retained for recovery")
                    compose("down", "--volumes", "--remove-orphans")
    except BaseException as error:
        failure = error
        print(json.dumps({"cloud_e2e": "failed", "error_type": type(error).__name__}), flush=True)
    finally:
        errors = []
        # Never discard the catalog needed to clean an external index if the
        # ordinary API cleanup failed. Recovery file has the dedicated login.
        if state["compose_started"] and not state["application_cleaned"]:
            errors.append("application cleanup")
        else:
            def remove(name, operation):
                try:
                    operation()
                except Exception as error:
                    errors.append(name)
                    print(json.dumps({"cleanup": name, "error_type": type(error).__name__}), flush=True)
            for name in state["secrets"]:
                remove("Modal secret " + name, lambda name=name: modal.Secret.objects.delete(name, allow_missing=True))
            for url in state["queues"]:
                remove("SQS queue", lambda url=url: sqs.delete_queue(QueueUrl=url))
            if state["bucket"]:
                def bucket_cleanup():
                    for page in s3.get_paginator("list_multipart_uploads").paginate(Bucket=identifier):
                        for upload in page.get("Uploads", []):
                            s3.abort_multipart_upload(Bucket=identifier, Key=upload["Key"], UploadId=upload["UploadId"])
                    for page in s3.get_paginator("list_objects_v2").paginate(Bucket=identifier):
                        if page.get("Contents"):
                            result = s3.delete_objects(Bucket=identifier, Delete={"Objects": [{"Key": row["Key"]} for row in page["Contents"]]})
                            if result.get("Errors"):
                                raise RuntimeError("Isolated bucket object cleanup incomplete")
                    s3.delete_bucket(Bucket=identifier)
                remove("isolated bucket", bucket_cleanup)
            def database_cleanup():
                with psycopg.connect(os.environ["DATABASE_URL"], autocommit=True, connect_timeout=15) as admin:
                    if state["database"]:
                        admin.execute(sql.SQL("DROP DATABASE {} WITH (FORCE)").format(sql.Identifier(db_name)))
                    if state["role"]:
                        admin.execute(sql.SQL("DROP ROLE {}").format(sql.Identifier(role_name)))
            remove("isolated database/login", database_cleanup)
        for client in (s3, sqs, sts):
            client.close()
        if errors:
            print(f"Recovery required; protected resource/credential file: {state_path}", flush=True)
            raise RuntimeError("Cloud resource cleanup incomplete") from None
        state_path.unlink()
        recovery.rmdir()
        print("Removed this run's temporary cloud resources, secrets and recovery credentials.", flush=True)
    if failure is not None:
        raise RuntimeError("Cloud E2E failed; see sanitized diagnostics") from None
    print("Cloud GPU index E2E passed. Production endpoints and IAM were unchanged.", flush=True)


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        # Database/provider exception messages can contain connection details.
        print(f"Cloud E2E stopped: {type(error).__name__}", file=sys.stderr)
        raise SystemExit(1)
