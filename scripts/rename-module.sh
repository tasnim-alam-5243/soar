#!/usr/bin/env bash
# Replace the placeholder module path everywhere. usage: scripts/rename-module.sh github.com/you/soar
set -euo pipefail
new="${1:?usage: rename-module.sh github.com/<you>/soar}"
old="github.com/yourorg/soar"
cd "$(dirname "$0")/.."
grep -rl --exclude-dir=.git "$old" . | xargs sed -i "s#${old}#${new}#g"
echo "module is now ${new}"
