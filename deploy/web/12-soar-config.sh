#!/bin/sh
# Runs before 20-envsubst-on-templates.sh: creates its output directory and writes the
# UI runtime configuration from environment variables.
set -eu
mkdir -p /tmp/nginx
cat > /tmp/soar-config.json <<JSON
{"apiBase": "/api/v1", "oidc": {"authority": "${SOAR_OIDC_AUTHORITY}", "clientId": "${SOAR_OIDC_CLIENT_ID}"}}
JSON
