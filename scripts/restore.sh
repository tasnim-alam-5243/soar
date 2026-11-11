#!/usr/bin/env bash
# Restore of a backup made by scripts/backup.sh (spec section 15). Stop the SOAR roles first
# (`docker compose stop api ingest processor collector orchestrator worker scheduler`): the
# restore replaces the live indices and streams.
#
#   scripts/restore.sh <name>
#
# Uses the same environment variables as backup.sh.
set -euo pipefail

NAME="${1:?usage: scripts/restore.sh <backup name>}"
OS_URL="${OS_URL:-http://127.0.0.1:9200}"
SNAPSHOT_REPO="${SNAPSHOT_REPO:-soar-backups}"
NATS_URL="${NATS_URL:-nats://127.0.0.1:4222}"
BACKUP_DIR="${BACKUP_DIR:-./backups}"
IN="${BACKUP_DIR}/${NAME}"

S3_URL="${S3_URL:-${MINIO_URL:-}}"
S3_ACCESS_KEY="${S3_ACCESS_KEY:-${MINIO_ACCESS_KEY:-}}"
S3_SECRET_KEY="${S3_SECRET_KEY:-${MINIO_SECRET_KEY:-}}"

curl_os() {
  if [[ -n "${OS_AUTH:-}" ]]; then curl -fsS -u "${OS_AUTH}" "$@"; else curl -fsS "$@"; fi
}

# Same as in backup.sh: rclone with the store as remote "soar:", keys passed by name.
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

read -r -p "This replaces all soar-* indices and the NATS streams with backup ${NAME}. Type the backup name to continue: " confirm
[[ "${confirm}" == "${NAME}" ]] || { echo "aborted"; exit 1; }

echo "== 1/3 OpenSearch: close soar-* indices and restore the snapshot"
curl_os -XPOST "${OS_URL}/soar-*/_close?ignore_unavailable=true" > /dev/null
curl_os -XPOST "${OS_URL}/_snapshot/${SNAPSHOT_REPO}/${NAME}/_restore?wait_for_completion=true" \
  -H 'Content-Type: application/json' -d '{"indices": "soar-*", "include_global_state": false, "include_aliases": true}' > /dev/null

echo "== 2/3 NATS streams"
for s in EVENTS STEPS RESULTS DLQ; do
  if [[ -d "${IN}/nats-${s}" ]]; then
    docker run --rm --network host -v "$(realpath "${IN}"):/backup" natsio/nats-box:0.17.0 \
      sh -c "nats --server ${NATS_URL} stream rm -f ${s} >/dev/null 2>&1; nats --server ${NATS_URL} stream restore /backup/nats-${s}"
  fi
done

echo "== 3/3 evidence bucket"
if [[ -n "${S3_URL}" && -d "${IN}/soar-evidence" ]]; then
  s3_rclone "${IN}" copy /backup/soar-evidence soar:soar-evidence
fi

echo "restored ${NAME}; run 'make migrate-check', then start the roles again"
