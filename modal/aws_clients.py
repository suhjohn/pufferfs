"""AWS clients with refreshable Modal workload identity credentials."""

import os
import threading

import boto3
import botocore.session
from botocore.credentials import (
    AssumeRoleWithWebIdentityCredentialFetcher,
    CredentialProvider,
    DeferredRefreshableCredentials,
)


_sessions = threading.local()


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


def session():
    # Sessions and the workload-identity fetcher's client factory are mutable.
    # Keep both on the request thread, including later credential refreshes.
    if not hasattr(_sessions, "sdk"):
        sdk = botocore.session.get_session()
        role = os.getenv("PUFFERFS_AWS_ROLE_ARN")
        if role:
            sdk.get_component("credential_provider").insert_before("env", ModalIdentity(sdk, role))
        _sessions.sdk = boto3.Session(botocore_session=sdk)
    return _sessions.sdk


def client(service, **options):
    return session().client(service, **options)
