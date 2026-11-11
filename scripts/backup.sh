#!/usr/bin/env bash
# Backup of the SOAR platform state (spec section 15).
#
#   scripts/backup.sh <name>        # default name: soar-YYYYmmdd-HHMMSS
#
# What it saves:
#   1. OpenSearch: a snapshot of every soar-* index into the snapshot repository $SNAPSHOT_REPO
#      (register it once, e.g. an S3 repository on the dev "s3" service: see docs/runbooks.md).
#   2. NATS JetStream: a backup of the EVENTS, STEPS, RESULTS and DLQ streams (nats-box), so
#      accepted-but-unprocessed work survives a full cluster loss.
#   3. Object storage: a mirror of the soar-evidence bucket (case files are not reproducible).
#      soar-raw / soar-payloads / soar-exports expire after 7 days and are not copied.
#
# Environment: OS_URL (default http://127.0.0.1:9200), OS_AUTH (user:password, optional),
# SNAPSHOT_REPO (default soar-backups), NATS_URL (default nats://127.0.0.1:4222),
# S3_URL / S3_ACCESS_KEY / S3_SECRET_KEY / S3_REGION (evidence copy through rclone; the dev stack:
# S3_URL=http://127.0.0.1:9000 with the keys of deploy/compose/.env; the older MINIO_URL / MINIO_ACCESS_KEY /
# MINIO_SECRET_KEY names still work), BACKUP_DIR (default ./backups).
set -euo pipefail

NAME="${1:-soar-$(date -u +%Y%m%d-%H%M%S)}"
OS_URL="${OS_URL:-http://127.0.0.1:9200}"
SNAPSHOT_REPO="${SNAPSHOT_REPO:-soar-backups}"
NATS_URL="${NATS_URL:-nats://127.0.0.1:4222}"
BACKUP_DIR="${BACKUP_DIR:-./backups}"
OUT="${BACKUP_DIR}/${NAME}"
mkdir -p "${OUT}"

S3_URL="${S3_URL:-${MINIO_URL:-}}"
S3_ACCESS_KEY="${S3_ACCESS_KEY:-${MINIO_ACCESS_KEY:-}}"
S3_SECRET_KEY="${S3_SECRET_KEY:-${MINIO_SECRET_KEY:-}}"

curl_os() {
  if [[ -n "${OS_AUTH:-}" ]]; then curl -fsS -u "${OS_AUTH}" "$@"; else curl -fsS "$@"; fi
}

# s3_rclone <host dir mounted at /backup> <rclone args...>: rclone against the store as remote "soar:".
# The keys travel as environment variables passed by name, so they never appear in a process list.
s3_rclone() {
  local dir="$1"; shift
  export RCLONE_CONFIG_SOAR_TYPE=s3 RCLONE_CONFIG_SOAR_PROVIDER=Other RCLONE_CONFIG_SOAR_FORCE_PATH_STYLE=true
  export RCLONE_CONFIG_SOAR_ENDPOINT="${S3_URL}" RCLONE_CONFIG_SOAR_REGION="${S3_REGION:-us-east-1}"
  export RCLONE_CONFIG_SOAR_ACCESS_KEY_ID="${S3_ACCESS_KEY}" RCLONE_CONFIG_SOAR_SECRET_ACCESS_KEY="${S3_SECRET_KEY}"
  docker run --rm --network host -v "$(realpath "${dir}"):/backup" \
    -e RCLONE_CONFIG_SOAR_TYPE -e RCLONE_CONFIG_SOAR_PROVIDER -e RCLONE_CONFIG_SOAR_FORCE_PATH_STYLE \
    -e RCLONE_CONFIG_SOAR_ENDPOINT -e RCLONE_CONFIG_SOAR_REGION \
    -e RCLONE_CONFIG_SOAR_ACCESS_KEY_ID -e RCLONE_CONFIG_SOAR_SECRET_ACCESS_KEY \
    rclone/rclone:1.76.0 "$@"
}

echo "== 1/3 OpenSearch snapshot ${NAME} into repository ${SNAPSHOT_REPO}"
curl_os -XPUT "${OS_URL}/_snapshot/${SNAPSHOT_REPO}/${NAME}?wait_for_completion=true" \
  -H 'Content-Type: application/json' \
  -d '{"indices": "soar-*", "include_global_state": false, "ignore_unavailable": true}' > "${OUT}/opensearch-snapshot.json"
grep -q '"state":"SUCCESS"' "${OUT}/opensearch-snapshot.json" || { echo "snapshot did not succeed: see ${OUT}/opensearch-snapshot.json"; exit 1; }

echo "== 2/3 NATS JetStream streams"
for s in EVENTS STEPS RESULTS DLQ; do
  docker run --rm --network host -v "$(realpath "${OUT}"):/backup" natsio/nats-box:0.17.0 \
    nats --server "${NATS_URL}" stream backup "${s}" "/backup/nats-${s}" || echo "warning: stream ${s} not backed up"
done

echo "== 3/3 evidence bucket"
if [[ -n "${S3_URL}" ]]; then
  s3_rclone "${OUT}" copy soar:soar-evidence /backup/soar-evidence
else
  echo "S3_URL not set: evidence bucket skipped"
fi

echo "backup ${NAME} written to ${OUT}"
