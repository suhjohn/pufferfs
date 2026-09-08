"""AWS clients with refreshable Modal workload identity credentials."""

from functools import lru_cache
import os
import threading

import boto3
import botocore.session
from botocore.credentials import (
    AssumeRoleWithWebIdentityCredentialFetcher,
    CredentialProvider,
    DeferredRefreshableCredentials,
)


_client_lock = threading.Lock()


class ModalIdentity(CredentialProvider):
    METHOD = "modal-identity"

    def __init__(self, session, role_arn):
        self.fetcher = AssumeRoleWithWebIdentityCredentialFetcher(
            client_creator=session.create_client,
            web_identity_token_loader=lambda: os.environ["MODAL_IDENTITY_TOKEN"],
            role_arn=role_arn,
            extra_args={"RoleSessionName": "pufferfs-worker"},
        )

    def load(self):
        return DeferredRefreshableCredentials(
            refresh_using=self.fetcher.fetch_credentials, method=self.METHOD,
        )


@lru_cache(maxsize=1)
def session():
    sdk = botocore.session.get_session()
    role = os.getenv("PUFFERFS_AWS_ROLE_ARN")
    if role:
        sdk.get_component("credential_provider").insert_before("env", ModalIdentity(sdk, role))
    return boto3.Session(botocore_session=sdk)


def client(service, **options):
    # Boto3 sessions mutate component caches during client construction. Each
    # invocation owns its clients; only construction shares the session lock.
    with _client_lock:
        return session().client(service, **options)
