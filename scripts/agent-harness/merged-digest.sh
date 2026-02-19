#!/usr/bin/env bash
set -euo pipefail

root=$(git rev-parse --show-toplevel 2>/dev/null || pwd)
cd "$root"

since="24 hours ago"
output=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --since)
      since="${2:-24 hours ago}"
      shift 2
      ;;
    --output)
      output="${2:-}"
      shift 2
      ;;
    *)
      echo "Unknown arg: $1"
      exit 1
      ;;
  esac
done

repo=$(basename "$root")
now=$(date -u +"%Y-%m-%d %H:%M:%SZ")

commits=$(git log --since="$since" --first-parent --pretty=format:'%H%x09%an%x09%s')

render() {
  echo "# Agent Digest ($repo)"
  echo
  echo "Generated: $now"
  echo "Window: since $since"
  echo

  if [[ -z "${commits:-}" ]]; then
    echo "No commits in range."
    return
  fi

  echo "## Commits"

  themes_tmp=$(mktemp)

  while IFS=$'\t' read -r sha author subject; do
    [[ -z "$sha" ]] && continue
    short=$(printf '%s' "$sha" | cut -c1-8)
    themes=$(git show --pretty='' --name-only "$sha" | awk -F/ 'NF{print $1}' | sort -u | tr '\n' ',' | sed 's/,$//')
    if [[ -z "$themes" ]]; then
      themes="(no-files)"
    fi

    printf -- '- `%s` %s — %s (%s)\n' "$short" "$subject" "$author" "$themes"

    printf '%s\n' "$themes" | tr ',' '\n' | sed '/^$/d' >> "$themes_tmp"
  done <<< "$commits"

  echo
  echo "## Themes"
  sort "$themes_tmp" | uniq -c | sort -nr | awk '{printf "- %s (%s commit(s))\n", $2, $1}'
  rm -f "$themes_tmp"
}

if [[ -n "$output" ]]; then
  mkdir -p "$(dirname "$output")"
  render > "$output"
  echo "Wrote $output"
else
  render
fi
