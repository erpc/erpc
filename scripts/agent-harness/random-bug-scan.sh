#!/usr/bin/env bash
set -euo pipefail

root=$(git rev-parse --show-toplevel 2>/dev/null || pwd)
cd "$root"

count=1
seed=""
race=1
timeout="8m"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --count)
      count="${2:-1}"
      shift 2
      ;;
    --seed)
      seed="${2:-}"
      shift 2
      ;;
    --no-race)
      race=0
      shift
      ;;
    --timeout)
      timeout="${2:-8m}"
      shift 2
      ;;
    *)
      echo "Unknown arg: $1"
      exit 1
      ;;
  esac
done

if [[ -z "$seed" ]]; then
  seed=$(date +%s)
fi

echo "Random bug scan seed: $seed"

tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT

go list ./... \
  | rg -v '^github\.com/erpc/erpc/cmd($|/)' \
  | rg -v '^github\.com/erpc/erpc/test($|/)' \
  > "$tmp"

if [[ ! -s "$tmp" ]]; then
  echo "No packages found for scan"
  exit 1
fi

mapfile_tmp=$(mktemp)
trap 'rm -f "$tmp" "$mapfile_tmp"' EXIT

awk -v seed="$seed" 'BEGIN{srand(seed)} {print rand() "\t" $0}' "$tmp" | sort -k1,1n | cut -f2- > "$mapfile_tmp"

selected=$(head -n "$count" "$mapfile_tmp")
if [[ -z "${selected:-}" ]]; then
  echo "No selected packages"
  exit 1
fi

echo "Selected packages:"
printf '%s\n' "$selected" | sed 's/^/- /'

fail=0
while IFS= read -r pkg; do
  [[ -z "$pkg" ]] && continue
  echo
  echo "Running: $pkg"
  if [[ $race -eq 1 ]]; then
    if ! go test "$pkg" -count 1 -race -timeout "$timeout"; then
      fail=1
    fi
  else
    if ! go test "$pkg" -count 1 -timeout "$timeout"; then
      fail=1
    fi
  fi
done <<< "$selected"

if [[ $fail -ne 0 ]]; then
  echo "Random bug scan: FAIL"
  exit 1
fi

echo "Random bug scan: OK"
