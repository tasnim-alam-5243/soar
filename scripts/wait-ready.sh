#!/usr/bin/env bash
# Poll a URL until it returns HTTP 200, or fail after TIMEOUT seconds.
# usage: wait-ready.sh URL [TIMEOUT_SECONDS]
set -euo pipefail
url="${1:?usage: wait-ready.sh URL [TIMEOUT_SECONDS]}"
timeout="${2:-120}"
deadline=$((SECONDS + timeout))
printf 'waiting for %s ' "$url"
until curl -fsS -o /dev/null --max-time 3 "$url" 2>/dev/null; do
  if (( SECONDS >= deadline )); then
    printf '\ntimed out after %ss. Try: make ps && make logs\n' "$timeout" >&2
    exit 1
  fi
  printf '.'
  sleep 3
done
printf ' ready\n'
