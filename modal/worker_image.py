"""Shared build inputs; deployment roles remain separate Modal applications."""

import os

import modal

cpu_image = (
    modal.Image.from_registry("python:3.12-slim-trixie")
    .apt_install("ca-certificates", "ffmpeg", "libreoffice-core", "libreoffice-writer", "libreoffice-impress", "libreoffice-calc", "fonts-dejavu-core")
    .pip_install_from_requirements("requirements-transform.txt")
    # Binary libpq's OpenSSL defaults need not match the base image's paths.
    # Keep sslrootcert=system / verify-full using the installed trust store.
    .env({"SSL_CERT_FILE": "/etc/ssl/certs/ca-certificates.crt"})
    .add_local_file("aws_clients.py", "/root/aws_clients.py", copy=True)
    .add_local_file("source_io.py", "/root/source_io.py", copy=True)
    .add_local_file("extraction.py", "/root/extraction.py", copy=True)
    .add_local_file("spreadsheet_extract.py", "/root/spreadsheet_extract.py", copy=True)
    .add_local_file("fods_extract.py", "/root/fods_extract.py", copy=True)
    .add_local_file("structured_extract.py", "/root/structured_extract.py", copy=True)
    .add_local_file("visual_prepare.py", "/root/visual_prepare.py", copy=True)
    .add_local_file("media_prepare.py", "/root/media_prepare.py", copy=True)
    .add_local_file("gemini_contract.py", "/root/gemini_contract.py", copy=True)
    .add_local_file("provider_submission.py", "/root/provider_submission.py", copy=True)
    .add_local_file("provider_retry.py", "/root/provider_retry.py", copy=True)
    .add_local_file("provider_prepare.py", "/root/provider_prepare.py", copy=True)
    .add_local_file("provider_refresh.py", "/root/provider_refresh.py", copy=True)
    .add_local_file("provider_cleanup.py", "/root/provider_cleanup.py", copy=True)
    .add_local_file("batch_collector.py", "/root/batch_collector.py", copy=True)
    .add_local_file("embedding_artifacts.py", "/root/embedding_artifacts.py", copy=True)
    .add_local_file("nomic_model.py", "/root/nomic_model.py", copy=True)
    .add_local_file("index_mutations.py", "/root/index_mutations.py", copy=True)
    .add_local_file("index_prepare.py", "/root/index_prepare.py", copy=True)
    .add_local_file("index_publish.py", "/root/index_publish.py", copy=True)
    .add_local_file("file_runtime.py", "/root/file_runtime.py", copy=True)
    .add_local_file("transform_worker.py", "/root/transform_worker.py", copy=True)
    .add_local_file("role_auth.py", "/root/role_auth.py", copy=True)
    .add_local_file("worker_image.py", "/root/worker_image.py", copy=True)
    .add_local_file("requirements-transform.txt", "/root/requirements-transform.txt", copy=True)
)

worker_secret = modal.Secret.from_name(os.getenv("PUFFERFS_WORKER_SECRET_NAME", "pufferfs-workers"))
endpoint_secret = modal.Secret.from_name(os.getenv("PUFFERFS_MODAL_ENDPOINT_SECRET_NAME", "pufferfs-endpoint-auth"))
