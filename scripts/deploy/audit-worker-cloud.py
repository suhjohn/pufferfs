"""Read-only AWS checks using the actual configured Modal worker secret.

This is a deployment diagnostic, not an end-to-end test or a permission grant.
It creates a temporary CPU sandbox, never deploys/replaces an application, never
reads source bodies and never receives queue messages. No secret values print.
S3 write/abort/delete and bulk GPU execution still need staging E2E validation.
"""

import os
from pathlib import Path
import uuid

import modal


PROBE = r'''
import json, os, uuid
from contextlib import closing
from aws_clients import client
from botocore.config import Config

config = Config(connect_timeout=5,read_timeout=10,retries={"total_max_attempts":1})
region = os.environ.get("AWS_REGION") or os.environ.get("AWS_DEFAULT_REGION")
results = []

def check(name, operation):
    try:
        details = operation()
        results.append({"check":name,"status":"passed",**details})
    except Exception as error:
        code = getattr(error,"response",{}).get("Error",{}).get("Code",type(error).__name__)
        results.append({"check":name,"status":"failed","error_code":str(code)})

with closing(client("sts",config=config,region_name=region)) as sts:
    def identity():
        caller=sts.get_caller_identity()
        return {"account":caller["Account"],"principal":caller["Arn"]}
    check("worker AWS identity",identity)
bucket=os.environ.get("AWS_BUCKET_NAME")
if not bucket:
    results.append({"check":"worker S3 configuration","status":"failed","error_code":"missing_bucket"})
else:
    with closing(client("s3",config=config,region_name=region)) as s3:
        if not s3.meta.endpoint_url.endswith(".amazonaws.com"):
            raise RuntimeError("cloud audit requires an actual AWS S3 endpoint")
        prefix="sources/cloud-readiness-"+uuid.uuid4().hex+"/"
        def objects():
            page=s3.list_objects_v2(Bucket=bucket,Prefix=prefix,MaxKeys=1)
            return {"bucket":bucket,"count":len(page.get("Contents",[]))}
        def multipart():
            page=s3.list_multipart_uploads(Bucket=bucket,Prefix=prefix,MaxUploads=1)
            return {"bucket":bucket,"count":len(page.get("Uploads",[]))}
        check("s3:ListBucket",objects)
        check("s3:ListBucketMultipartUploads",multipart)
with closing(client("sqs",config=config,region_name=region)) as sqs:
    for stage in ("TRANSFORM","INDEX"):
        url=os.environ.get("PUFFERFS_SQS_"+stage+"_QUEUE_URL")
        if not url:
            results.append({"check":stage+" SQS configuration","status":"failed","error_code":"missing_queue_url"})
            continue
        def queue():
            attributes=sqs.get_queue_attributes(QueueUrl=url,AttributeNames=["QueueArn","VisibilityTimeout","RedrivePolicy"])["Attributes"]
            return {"queue_arn":attributes.get("QueueArn"),"visibility_seconds":attributes.get("VisibilityTimeout"),
                    "has_dead_letter_policy":bool(attributes.get("RedrivePolicy"))}
        check(stage+" sqs:GetQueueAttributes",queue)
for result in results:
    print(json.dumps(result),flush=True)
raise SystemExit(0 if all(item["status"]=="passed" for item in results) else 1)
'''


def main():
    app = modal.App("pufferfs-worker-audit")
    image = (modal.Image.debian_slim(python_version="3.12").pip_install("boto3>=1.34")
             .add_local_file(Path(__file__).resolve().parents[2] / "modal/aws_clients.py", "/root/aws_clients.py", copy=True))
    secret = modal.Secret.from_name(os.getenv("PUFFERFS_WORKER_SECRET_NAME", "pufferfs-workers"))
    with modal.enable_output(), app.run():
        # Setup errors print only their class, never a credentials-provider
        # traceback, URL or environment value from the remote worker secret.
        command = "import json\ntry:\n    exec(" + repr(PROBE) + ")\nexcept Exception as error:\n    print(json.dumps({'check':'probe setup','status':'failed','error_code':type(error).__name__}),flush=True)\n    raise SystemExit(1)"
        sandbox = modal.Sandbox.create("python", "-c", command, app=app, image=image,
            secrets=[secret], timeout=180, cpu=0.25, memory=256, include_oidc_identity_token=True)
        try:
            print("Temporary worker-credential audit:", sandbox.object_id, flush=True)
            # The remote program emits only selected nonsecret fields and error
            # codes, not environment values, URLs, response bodies or tracebacks.
            for line in sandbox.stdout:
                print(line, end="", flush=True)
            sandbox.wait()
            if sandbox.returncode:
                raise RuntimeError("worker cloud audit failed; inspect the reported checks and secret availability")
        finally:
            sandbox.terminate()


if __name__ == "__main__":
    main()
