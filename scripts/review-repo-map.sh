#!/usr/bin/env bash
set -euo pipefail

root=$(git rev-parse --show-toplevel 2>/dev/null || pwd)
cd "$root"

repo_name=$(basename "$root")
in_git=0
if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  in_git=1
fi

echo "Repo: ${repo_name}"
echo

echo "Top-level"
if [[ $in_git -eq 1 ]]; then
  git ls-files | awk -F/ '{print $1}' | sort -u | sed 's/^/- /'
else
  ls -1 | sed 's/^/- /'
fi

echo

echo "Go entrypoints"
if [[ -d cmd ]]; then
  ls -1 cmd | sed 's/^/- cmd\//'
else
  echo "- none"
fi

echo

echo "Core packages"
for dir in erpc common clients auth consensus data health monitoring telemetry upstream util; do
  [[ -d "$dir" ]] || continue
  echo "- $dir/"
done

echo

echo "Config"
for file in erpc.yaml erpc.dist.yaml erpc.dist.ts prometheus.yaml docker-compose.yml; do
  if [[ $in_git -eq 1 ]]; then
    git ls-files --error-unmatch "$file" >/dev/null 2>&1 || continue
  else
    [[ -f "$file" ]] || continue
  fi
  echo "- $file"
done
if [[ -d kube ]]; then
  echo "- kube/"
fi

echo

echo "Docs"
for dir in docs docs/design docs/plans architecture; do
  [[ -d "$dir" ]] || continue
  echo "- $dir/"
done
