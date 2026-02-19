#!/usr/bin/env bash
set -euo pipefail

root=$(git rev-parse --show-toplevel 2>/dev/null || pwd)
cd "$root"

required_files=(
  AGENTS.md
  review/README.md
  review/checklist.md
  review/contracts.md
  review/data-models.md
  review/skills-shell-tips.md
  review/repo-map.md
  review/autonomy.md
  review/runbooks/bugfix.md
  review/runbooks/feature.md
  review/runbooks/review.md
  .github/workflows/agent-harness.yml
  .github/workflows/agent-automations.yml
  scripts/review-repo-map.sh
  scripts/review-impact-map.sh
  scripts/agent-harness/update-repo-map.sh
  scripts/agent-harness/suggest-doc-updates.sh
  scripts/agent-harness/review-load.sh
  scripts/agent-harness/skills-shell-check.sh
  scripts/agent-harness/pr-health.sh
  scripts/agent-harness/random-bug-scan.sh
  scripts/agent-harness/merged-digest.sh
  scripts/agent-harness/context.sh
  scripts/agent-harness/check.sh
)

missing=0
for file in "${required_files[@]}"; do
  if [[ ! -f "$file" ]]; then
    echo "Missing required file: $file"
    missing=1
  fi
done

if [[ $missing -ne 0 ]]; then
  exit 1
fi

required_exec=(
  scripts/review-repo-map.sh
  scripts/review-impact-map.sh
  scripts/agent-harness/update-repo-map.sh
  scripts/agent-harness/suggest-doc-updates.sh
  scripts/agent-harness/review-load.sh
  scripts/agent-harness/skills-shell-check.sh
  scripts/agent-harness/pr-health.sh
  scripts/agent-harness/random-bug-scan.sh
  scripts/agent-harness/merged-digest.sh
  scripts/agent-harness/context.sh
  scripts/agent-harness/check.sh
)

for file in "${required_exec[@]}"; do
  if [[ ! -x "$file" ]]; then
    echo "Script is not executable: $file"
    missing=1
  fi
done

if [[ $missing -ne 0 ]]; then
  exit 1
fi

required_refs=(
  review/README.md
  review/checklist.md
  review/contracts.md
  review/data-models.md
  review/skills-shell-tips.md
  review/repo-map.md
  review/autonomy.md
  scripts/agent-harness/review-load.sh
  scripts/agent-harness/skills-shell-check.sh
  scripts/agent-harness/context.sh
  scripts/agent-harness/check.sh
)

contains_literal() {
  local needle=$1
  local file=$2

  if command -v rg >/dev/null 2>&1; then
    rg -Fq -- "$needle" "$file"
  else
    grep -Fq -- "$needle" "$file"
  fi
}

for ref in "${required_refs[@]}"; do
  if ! contains_literal "$ref" AGENTS.md; then
    echo "AGENTS.md missing harness reference: $ref"
    missing=1
  fi
done

if [[ $missing -ne 0 ]]; then
  exit 1
fi

tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
scripts/agent-harness/update-repo-map.sh "$tmp" >/dev/null

if ! cmp -s "$tmp" review/repo-map.md; then
  echo "review/repo-map.md stale. Run: scripts/agent-harness/update-repo-map.sh"
  diff -u review/repo-map.md "$tmp" || true
  exit 1
fi

scripts/agent-harness/skills-shell-check.sh

echo "Harness check OK"
