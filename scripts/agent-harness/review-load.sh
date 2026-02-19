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
  numstat=$(git diff --cached --numstat)
elif [[ -n "$range" ]]; then
  files=$(git diff --name-only "$range")
  numstat=$(git diff --numstat "$range")
elif git rev-parse --verify HEAD~1 >/dev/null 2>&1; then
  files=$(git diff --name-only HEAD~1..HEAD)
  numstat=$(git diff --numstat HEAD~1..HEAD)
else
  files=$(git diff --name-only)
  numstat=$(git diff --numstat)
fi

if [[ -z "${files:-}" ]]; then
  echo "No changed files found."
  exit 0
fi

file_count=$(printf '%s\n' "$files" | sed '/^$/d' | wc -l | tr -d ' ')
add_total=$(printf '%s\n' "$numstat" | awk '$1 != "-" {s += $1} END {print s + 0}')
del_total=$(printf '%s\n' "$numstat" | awk '$2 != "-" {s += $2} END {print s + 0}')
loc_total=$((add_total + del_total))

warn_files=25
warn_loc=1200
fail_files=40
fail_loc=2500
large_file_threshold=400

echo "Changed files: $file_count"
echo "Diff lines: +$add_total / -$del_total (total $loc_total)"

warns=0
if [[ $file_count -gt $warn_files ]]; then
  echo "WARN: changed files > $warn_files"
  warns=$((warns + 1))
fi
if [[ $loc_total -gt $warn_loc ]]; then
  echo "WARN: diff lines > $warn_loc"
  warns=$((warns + 1))
fi

large_files=$(printf '%s\n' "$numstat" | awk -v t="$large_file_threshold" '$1 != "-" && $2 != "-" && ($1 + $2) >= t {print $3 " (" $1 "+/" $2 "-)"}')
if [[ -n "${large_files:-}" ]]; then
  echo "WARN: large per-file diff(s) >= $large_file_threshold lines"
  printf '%s\n' "$large_files" | sed 's/^/- /'
  warns=$((warns + 1))
fi

high_risk=$(printf '%s\n' "$files" | rg '^(erpc/|architecture/|clients/|upstream/|consensus/|data/|auth/)' || true)
if [[ -n "${high_risk:-}" ]]; then
  count=$(printf '%s\n' "$high_risk" | wc -l | tr -d ' ')
  echo "High-risk files: $count"
fi

echo "Review load warnings: $warns"

if [[ $strict -eq 1 ]]; then
  if [[ $file_count -gt $fail_files || $loc_total -gt $fail_loc ]]; then
    echo "Review load strict fail: split PR (files>$fail_files or diff>$fail_loc)."
    exit 1
  fi
fi
