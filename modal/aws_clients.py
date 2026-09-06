"""AWS clients with refreshable Modal workload identity credentials."""

from functools import lru_cache
import os

import boto3
import botocore.session
from botocore.credentials import (
    AssumeRoleWithWebIdentityCredentialFetcher,
    CredentialProvider,
    DeferredRefreshableCredentials,
)


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
    return session().client(service, **options)
