"""Create isolated S3 + FIFO SQS resources through their AWS APIs."""
import json
import os

import boto3

s3 = boto3.client("s3")
s3.create_bucket(Bucket=os.environ["AWS_BUCKET_NAME"])
s3.put_bucket_lifecycle_configuration(Bucket=os.environ["AWS_BUCKET_NAME"], LifecycleConfiguration={
    "Rules": [{"ID": "abort-incomplete-multipart-uploads", "Status": "Enabled", "Filter": {"Prefix": ""},
               "AbortIncompleteMultipartUpload": {"DaysAfterInitiation": 1}}],
})
sqs = boto3.client("sqs")
for stage in ("transform", "index"):
    dlq = sqs.create_queue(QueueName=f"file-{stage}-dlq.fifo", Attributes={"FifoQueue": "true"})["QueueUrl"]
    arn = sqs.get_queue_attributes(QueueUrl=dlq, AttributeNames=["QueueArn"])["Attributes"]["QueueArn"]
    queue = sqs.create_queue(QueueName=f"file-{stage}.fifo", Attributes={
        "FifoQueue": "true",
        "ContentBasedDeduplication": "false",
        "DeduplicationScope": "messageGroup",
        "FifoThroughputLimit": "perMessageGroupId",
        "VisibilityTimeout": "300",
        "ReceiveMessageWaitTimeSeconds": "20",
        "MessageRetentionPeriod": "1209600",
        "RedrivePolicy": json.dumps({"deadLetterTargetArn": arn, "maxReceiveCount": "5"}),
    })["QueueUrl"]
    if queue != os.environ[f"PUFFERFS_SQS_{stage.upper()}_QUEUE_URL"]:
        raise RuntimeError(f"{stage} queue URL does not match Compose configuration")
print("Created disposable source bucket and transform/index FIFO queues with DLQs.")
