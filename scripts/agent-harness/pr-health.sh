#!/usr/bin/env bash
set -euo pipefail

root=$(git rev-parse --show-toplevel 2>/dev/null || pwd)
cd "$root"

strict=0
limit=50
state="open"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --strict)
      strict=1
      shift
      ;;
    --limit)
      limit="${2:-50}"
      shift 2
      ;;
    --state)
      state="${2:-open}"
      shift 2
      ;;
    *)
      echo "Unknown arg: $1"
      exit 1
      ;;
  esac
done

if ! command -v gh >/dev/null 2>&1; then
  echo "gh CLI not found"
  exit 1
fi

if ! gh auth status >/dev/null 2>&1; then
  echo "gh auth missing. Run: gh auth login"
  exit 1
fi

repo=$(gh repo view --json nameWithOwner --jq '.nameWithOwner')
echo "Repo: $repo"

data=$(gh pr list \
  --state "$state" \
  --limit "$limit" \
  --json number,title,isDraft,mergeStateStatus,statusCheckRollup,headRefName,baseRefName,updatedAt \
  --jq '.[] | [
    (.number|tostring),
    (.mergeStateStatus // "UNKNOWN"),
    (if .isDraft then "draft" else "ready" end),
    ([.statusCheckRollup[]? | select((.conclusion // "") == "FAILURE" or (.conclusion // "") == "TIMED_OUT" or (.conclusion // "") == "CANCELLED" or (.conclusion // "") == "ACTION_REQUIRED" or (.conclusion // "") == "STARTUP_FAILURE")] | length | tostring),
    ([.statusCheckRollup[]? | select((.status // "") == "IN_PROGRESS" or (.status // "") == "QUEUED" or (.status // "") == "PENDING" or (.status // "") == "WAITING")] | length | tostring),
    (.headRefName // "-"),
    (.baseRefName // "-"),
    (.updatedAt // "-"),
    (.title | gsub("[\\t\\n\\r]+"; " "))
  ] | @tsv')

if [[ -z "${data:-}" ]]; then
  echo "No PRs in state=$state"
  exit 0
fi

printf '%-8s %-10s %-8s %-7s %-8s %-28s %-10s %s\n' "PR" "MERGE" "MODE" "FAIL" "PEND" "HEAD" "BASE" "TITLE"

issues=0
while IFS=$'\t' read -r number merge mode failed pending head base updated title; do
  printf '#%-7s %-10s %-8s %-7s %-8s %-28s %-10s %s\n' \
    "$number" "$merge" "$mode" "$failed" "$pending" "$head" "$base" "$title"

  if [[ "$mode" != "draft" ]]; then
    if [[ "$merge" != "CLEAN" && "$merge" != "HAS_HOOKS" ]]; then
      issues=$((issues + 1))
    fi
    if [[ "$failed" =~ ^[0-9]+$ ]] && [[ "$failed" -gt 0 ]]; then
      issues=$((issues + 1))
    fi
  fi
done <<< "$data"

echo
if [[ $issues -eq 0 ]]; then
  echo "PR health: OK"
else
  echo "PR health: $issues issue signal(s)"
fi

if [[ $strict -eq 1 && $issues -gt 0 ]]; then
  exit 1
fi
