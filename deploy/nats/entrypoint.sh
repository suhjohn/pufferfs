#!/bin/sh
set -eu

: "${NATS_SERVER_NAME:?NATS_SERVER_NAME is required}"
: "${NATS_CLUSTER_NAME:=pufferfs}"
: "${NATS_STORE_DIR:=/data}"

args="
  -js
  -sd ${NATS_STORE_DIR}
  --server_name ${NATS_SERVER_NAME}
  --cluster_name ${NATS_CLUSTER_NAME}
  -p 4222
  -m 8222
  -cluster nats://0.0.0.0:6222
"

if [ -n "${NATS_ROUTES:-}" ]; then
  args="${args} -routes ${NATS_ROUTES}"
fi

exec nats-server ${args}
