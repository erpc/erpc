#!/usr/bin/env bash
set -euo pipefail

root=$(git rev-parse --show-toplevel 2>/dev/null || pwd)
cd "$root"

missing=0

required=(
  review/skills-shell-tips.md
  artifacts/agent/.gitkeep
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

contains_case_insensitive() {
  local pattern=$1
  local file=$2

  if command -v rg >/dev/null 2>&1; then
    rg -qi -- "$pattern" "$file"
  else
    grep -Eqi -- "$pattern" "$file"
  fi
}

list_skill_files() {
  if command -v rg >/dev/null 2>&1; then
    rg --files -g '**/SKILL.md' || true
  else
    find . -type f -name 'SKILL.md' | sed 's#^\./##' || true
  fi
}

for file in "${required[@]}"; do
  if [[ ! -f "$file" ]]; then
    echo "Missing required skills-shell artifact: $file"
    missing=1
  fi
done

if ! contains_literal 'review/skills-shell-tips.md' AGENTS.md; then
  echo "AGENTS.md missing reference: review/skills-shell-tips.md"
  missing=1
fi

if ! contains_literal 'artifacts/agent/' AGENTS.md; then
  echo "AGENTS.md missing reference: artifacts/agent/"
  missing=1
fi

skill_files=$(list_skill_files)
if [[ -z "${skill_files:-}" ]]; then
  echo "No in-repo SKILL.md files found; skills lint skipped."
else
  while IFS= read -r skill; do
    [[ -z "$skill" ]] && continue
    if ! contains_case_insensitive 'use when|when to use' "$skill"; then
      echo "SKILL missing routing trigger section: $skill"
      missing=1
    fi
    if ! contains_case_insensitive "don't use|do not use|avoid when" "$skill"; then
      echo "SKILL missing anti-trigger section: $skill"
      missing=1
    fi
    if ! contains_case_insensitive 'example|template' "$skill"; then
      echo "SKILL missing examples/templates: $skill"
      missing=1
    fi
  done <<< "$skill_files"
fi

if [[ $missing -ne 0 ]]; then
  exit 1
fi

echo "Skills + shell tips check OK"
