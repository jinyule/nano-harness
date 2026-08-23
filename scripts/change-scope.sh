#!/usr/bin/env bash
set -euo pipefail

if [[ "$#" -gt 2 ]]; then
  echo "usage: scripts/change-scope.sh [base-ref] [head-ref]" >&2
  exit 2
fi

base_ref="${1:-}"
head_ref="${2:-HEAD}"
repository="$(git rev-parse --show-toplevel)"
cd "$repository"

print_section() {
  local label="$1"
  shift
  local output
  output="$("$@")"
  printf '[%s]\n' "$label"
  if [[ -n "$output" ]]; then
    printf '%s\n' "$output"
  else
    printf '(clean)\n'
  fi
}

head_oid=""
if head_oid="$(git rev-parse --verify "$head_ref^{commit}" 2>/dev/null)"; then
  if [[ -z "$base_ref" ]]; then
    echo "change scope: base-ref is required after the first commit" >&2
    exit 2
  fi
  git cat-file -e "$base_ref^{commit}" 2>/dev/null || {
    echo "change scope: base-ref is not a commit: $base_ref" >&2
    exit 2
  }
  base_oid="$(git rev-parse "$base_ref^{commit}")"
  merge_base="$(git merge-base "$base_oid" "$head_oid")"

  printf 'repository: %s\n' "$repository"
  printf 'base: %s (%s)\n' "$base_ref" "$base_oid"
  printf 'head: %s (%s)\n' "$head_ref" "$head_oid"
  printf 'merge-base: %s\n' "$merge_base"
  print_section committed git diff --name-status "$merge_base" "$head_oid"

  current_head="$(git rev-parse --verify HEAD)"
  if [[ "$head_oid" != "$current_head" ]]; then
    printf '[worktree]\n(not reported for non-HEAD inspection)\n'
    exit 0
  fi

  print_section staged git diff --cached --name-status "$head_oid"
else
  if [[ "$head_ref" != "HEAD" || -n "$base_ref" ]]; then
    echo "change scope: head-ref is not a commit: $head_ref" >&2
    exit 2
  fi
  empty_tree="$(git hash-object -t tree /dev/null)"
  printf 'repository: %s\n' "$repository"
  printf 'base: (empty tree)\n'
  printf 'head: (unborn)\n'
  printf 'merge-base: (none)\n'
  printf '[committed]\n(none before first commit)\n'
  print_section staged git diff --cached --name-status "$empty_tree"
fi

print_section unstaged git diff --name-status
untracked="$(git ls-files --others --exclude-standard)"
printf '[untracked]\n'
if [[ -n "$untracked" ]]; then
  while IFS= read -r path; do
    printf '?\t%s\n' "$path"
  done <<<"$untracked"
else
  printf '(clean)\n'
fi
