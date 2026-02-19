#!/usr/bin/env bash
set -euo pipefail

root=$(git rev-parse --show-toplevel 2>/dev/null || pwd)
cd "$root"

staged=0
range=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --staged)
      staged=1
      shift
      ;;
    *)
      range="$1"
      shift
      ;;
  esac
done

echo "## Repo Map"
scripts/review-repo-map.sh

echo
echo "## Impact Map"
if [[ $staged -eq 1 ]]; then
  scripts/review-impact-map.sh --staged
elif [[ -n "$range" ]]; then
  scripts/review-impact-map.sh "$range"
elif git rev-parse --verify HEAD~1 >/dev/null 2>&1; then
  scripts/review-impact-map.sh HEAD~1..HEAD
else
  echo "No prior commit for default diff range."
fi

echo
echo "## Doc Update Hints"
if [[ $staged -eq 1 ]]; then
  scripts/agent-harness/suggest-doc-updates.sh --staged
elif [[ -n "$range" ]]; then
  scripts/agent-harness/suggest-doc-updates.sh "$range"
else
  scripts/agent-harness/suggest-doc-updates.sh
fi

echo
echo "## Review Load"
if [[ $staged -eq 1 ]]; then
  scripts/agent-harness/review-load.sh --staged
elif [[ -n "$range" ]]; then
  scripts/agent-harness/review-load.sh "$range"
else
  scripts/agent-harness/review-load.sh
fi
