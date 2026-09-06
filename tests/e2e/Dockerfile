FROM golang:1.25 AS cli
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/pufferfs/ ./cmd/pufferfs/
COPY internal/ ./internal/
COPY pkg/ ./pkg/
COPY migrations/ ./migrations/
RUN CGO_ENABLED=0 go build -o /pufferfs ./cmd/pufferfs

FROM python:3.12-slim-trixie AS worker-base
RUN apt-get update && apt-get install -y --no-install-recommends \
    ffmpeg libreoffice-core libreoffice-writer libreoffice-impress libreoffice-calc \
    fonts-dejavu-core ca-certificates espeak-ng \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app/modal
COPY modal/requirements-transform.txt modal/requirements-index.txt ./
RUN pip install --no-cache-dir -r requirements-transform.txt -r requirements-index.txt 'modal>=0.73.0'
ENV PYTHONUNBUFFERED=1
ENTRYPOINT ["python", "/e2e/runtime.py"]

FROM worker-base AS worker
ENV SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt
COPY modal/ ./
COPY tests/e2e/ /e2e/

FROM worker-base AS vector-deps
RUN pip install --no-cache-dir 'torch>=2' --index-url https://download.pytorch.org/whl/cpu \
    && pip install --no-cache-dir 'sentence-transformers>=3' 'einops>=0.7'
# The same immutable model/code revisions as the deployed bulk index role.
COPY modal/nomic_model.py ./nomic_model.py
RUN python -c 'from nomic_model import cache_model; cache_model()'

FROM vector-deps AS vector
ENV SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt
COPY modal/ ./
COPY tests/e2e/ /e2e/

FROM worker-base AS runner-deps
RUN apt-get update && apt-get install -y --no-install-recommends libopencore-amrnb0 \
    nodejs \
    && rm -rf /var/lib/apt/lists/*
RUN pip install --no-cache-dir python-docx python-pptx xlwt odfpy
# Fixture generation only: a real XLSB writer, pinned to the official release.
# Neither Node nor SheetJS is installed in a production worker image.
ADD --checksum=sha256:cc015130aa8521e7f088f88898eba949ccdcbfb38df0bd129b44b7273c3a6f41 https://cdn.sheetjs.com/xlsx-0.20.3/package/dist/xlsx.full.min.js /opt/fixture-tools/xlsx.js

FROM runner-deps AS runner
ENV SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt
COPY modal/ ./
COPY tests/e2e/ /e2e/
COPY --from=cli /pufferfs /usr/local/bin/pufferfs
WORKDIR /e2e
ENTRYPOINT ["python", "/e2e/run.py"]
