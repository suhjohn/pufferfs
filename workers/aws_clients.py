"""Thread-local AWS sessions using the platform's standard credential chain."""

import threading
import boto3

_sessions = threading.local()


def client(service, **options):
    if not hasattr(_sessions, "sdk"):
        _sessions.sdk = boto3.Session()
    return _sessions.sdk.client(service, **options)
