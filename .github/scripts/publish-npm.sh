#!/usr/bin/env bash
set -euo pipefail

# Publishes the npm package in the current directory, optionally under an
# alias name (e.g. "erpc", "start-rpc", "start-erpc").
#
# Exits 0 when this exact version is already on the registry, so release
# re-runs stay idempotent. Any other failure (expired token shows up as
# E404 on the PUT, network errors, etc.) fails the step — do NOT swallow
# errors here, that is how the 0.1.0 npm release silently went missing.

ALIAS_NAME="${1:-}"

if [[ -n "$ALIAS_NAME" ]]; then
  cp package.json package.json.bak
  trap 'mv package.json.bak package.json' EXIT
  npm pkg set name="$ALIAS_NAME"
fi

set +e
OUTPUT=$(pnpm publish --access public --no-git-checks 2>&1)
CODE=$?
set -e

echo "$OUTPUT"

if [[ $CODE -ne 0 ]]; then
  if grep -qiE "previously published|cannot publish over|EPUBLISHCONFLICT" <<<"$OUTPUT"; then
    echo "Version already on the registry; nothing to publish."
    exit 0
  fi
  echo "::error::npm publish failed for ${ALIAS_NAME:-$(npm pkg get name | tr -d '\"')}"
  exit "$CODE"
fi
