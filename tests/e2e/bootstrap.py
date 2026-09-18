"""Create isolated S3 storage through the AWS API."""
import os
import json

import boto3

s3 = boto3.client("s3")
s3.create_bucket(Bucket=os.environ["AWS_BUCKET_NAME"])
s3.put_bucket_lifecycle_configuration(Bucket=os.environ["AWS_BUCKET_NAME"], LifecycleConfiguration={
    "Rules": [{"ID": "abort-incomplete-multipart-uploads", "Status": "Enabled", "Filter": {"Prefix": ""},
               "AbortIncompleteMultipartUpload": {"DaysAfterInitiation": 1}}],
})
print("Created disposable source bucket.")
s3.put_object(Bucket=os.environ["AWS_BUCKET_NAME"], Key="releases/manifest.json",
    Body=json.dumps({"latest":"dev", "minimum":"", "protocol_min":2, "protocol_max":2, "downloads":{}}).encode(),
    ContentType="application/json")
