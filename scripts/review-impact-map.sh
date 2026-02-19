#!/usr/bin/env bash
set -euo pipefail

root=$(git rev-parse --show-toplevel 2>/dev/null || pwd)
cd "$root"

if [[ "${1:-}" == "--staged" ]]; then
  shift
  files=$(git diff --cached --name-only)
elif [[ "$#" -gt 0 ]]; then
  files=$(git diff --name-only "$@")
else
  files=$(git diff --name-only HEAD~1..HEAD)
fi

if [[ -z "${files:-}" ]]; then
  echo "No changed files found."
  exit 0
fi

echo "Changed files"
printf '%s\n' "$files" | sed 's/^/- /'

echo

echo "Go packages changed"
packages=$(printf '%s\n' "$files" | awk -F/ '/\.go$/ {OFS="/"; $NF=""; sub("/$","",$0); print $0}' | sort -u)
if [[ -n "$packages" ]]; then
  printf '%s\n' "$packages" | sed 's/^/- /'
else
  echo "- none"
fi

echo

echo "Entrypoints changed"
if printf '%s\n' "$files" | rg -q '^cmd/'; then
  printf '%s\n' "$files" | rg '^cmd/' | sed 's/^/- /'
else
  echo "- none"
fi

echo

echo "Config changed"
config=$(printf '%s\n' "$files" | rg '^(erpc\.yaml|erpc\.dist\.(yaml|ts)|prometheus\.yaml|docker-compose\.yml|kube/)' || true)
if [[ -n "$config" ]]; then
  printf '%s\n' "$config" | sed 's/^/- /'
else
  echo "- none"
fi

echo

echo "Docs changed"
if printf '%s\n' "$files" | rg '^(README\.md|docs/|architecture/)' ; then
  printf '%s\n' "$files" | rg '^(README\.md|docs/|architecture/)' | sed 's/^/- /'
else
  echo "- none"
fi
