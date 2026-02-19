#!/usr/bin/env bash
set -euo pipefail

root=$(git rev-parse --show-toplevel 2>/dev/null || pwd)
cd "$root"

strict=0
staged=0
range=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --strict)
      strict=1
      shift
      ;;
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

if [[ $staged -eq 1 ]]; then
  files=$(git diff --cached --name-only)
elif [[ -n "$range" ]]; then
  files=$(git diff --name-only "$range")
elif git rev-parse --verify HEAD~1 >/dev/null 2>&1; then
  files=$(git diff --name-only HEAD~1..HEAD)
else
  files=$(git diff --name-only)
fi

if [[ -z "${files:-}" ]]; then
  echo "No changed files found."
  exit 0
fi

warnings=0

check_rule() {
  local file_pattern="$1"
  local doc_pattern="$2"
  local message="$3"

  if printf '%s\n' "$files" | rg -q "$file_pattern"; then
    if ! printf '%s\n' "$files" | rg -q "$doc_pattern"; then
      echo "WARN: $message"
      warnings=$((warnings + 1))
    fi
  fi
}

check_rule '^(erpc/|architecture/|clients/|upstream/|consensus/)' '^(review/contracts\.md|review/checklist\.md|docs/design/|docs/plans/|AGENTS\.md|review/autonomy\.md)' "Core behavior changed; review/update contracts/checklist/docs."
check_rule '^(data/|scylla/)' '^(review/data-models\.md|review/checklist\.md|docs/pages/config/database/|docs/plans/|AGENTS\.md|review/autonomy\.md)' "State/cache/storage changed; update data model docs."
check_rule '^(auth/)' '^(review/contracts\.md|review/checklist\.md|docs/pages/config/auth\.mdx|AGENTS\.md|review/autonomy\.md)' "Auth behavior changed; update auth contract docs."
check_rule '^(erpc\.dist\.yaml|erpc\.dist\.ts|erpc\.yaml|prometheus\.yaml|docker-compose\.yml|kube/)' '^(review/contracts\.md|review/repo-map\.md|docs/pages/config/|docs/pages/operation/|AGENTS\.md|review/autonomy\.md)' "Config/ops surface changed; update config docs."
check_rule '^(\.github/workflows/|Makefile|scripts/)' '^(AGENTS\.md|review/README\.md|review/autonomy\.md|docs/plans/)' "Automation/runtime changed; update harness docs."

if [[ $warnings -eq 0 ]]; then
  echo "Doc update hints: none"
else
  echo "Doc update hints: $warnings warning(s)"
fi

if [[ $strict -eq 1 && $warnings -gt 0 ]]; then
  exit 1
fi
