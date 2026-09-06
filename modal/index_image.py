"""Small index image, independent of Office/media transformation dependencies."""

import os
import modal

index_image = (modal.Image.debian_slim(python_version="3.12")
               .apt_install("ca-certificates")
               .pip_install_from_requirements("requirements-index.txt")
               .env({"SSL_CERT_FILE": "/etc/ssl/certs/ca-certificates.crt"}))
for module in ("source_io", "extraction", "file_runtime", "nomic_model", "embedding_artifacts", "index_mutations", "index_routing", "index_client", "index_prepare", "index_publish", "index_worker", "role_auth"):
    index_image = index_image.add_local_file(f"{module}.py", f"/root/{module}.py", copy=True)
# Modal imports the deployment module again inside the remote container. Its
# top-level build-helper import and requirements file must also be present.
index_image = (index_image
               .add_local_file("index_image.py", "/root/index_image.py", copy=True)
               .add_local_file("requirements-index.txt", "/root/requirements-index.txt", copy=True))

worker_secret = modal.Secret.from_name(os.getenv("PUFFERFS_WORKER_SECRET_NAME", "pufferfs-workers"))
endpoint_secret = modal.Secret.from_name(os.getenv("PUFFERFS_MODAL_ENDPOINT_SECRET_NAME", "pufferfs-endpoint-auth"))
