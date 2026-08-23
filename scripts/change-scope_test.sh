#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
change_scope="$script_dir/change-scope.sh"
test_root="$(mktemp -d)"
trap 'rm -rf -- "$test_root"' EXIT

git -C "$test_root" init -q
git -C "$test_root" config user.email test@example.invalid
git -C "$test_root" config user.name "Change Scope Test"

printf 'base\n' >"$test_root/base.txt"
git -C "$test_root" add base.txt
initial="$(cd "$test_root" && "$change_scope")"
grep -Fq 'head: (unborn)' <<<"$initial"
grep -Fq $'A\tbase.txt' <<<"$initial"

git -C "$test_root" commit -qm 'initial'
base_oid="$(git -C "$test_root" rev-parse HEAD)"
printf 'committed\n' >"$test_root/committed.txt"
git -C "$test_root" add committed.txt
git -C "$test_root" commit -qm 'second'

printf 'changed\n' >>"$test_root/base.txt"
printf 'staged\n' >"$test_root/staged.txt"
git -C "$test_root" add staged.txt
printf 'untracked\n' >"$test_root/untracked.txt"

scope="$(cd "$test_root" && "$change_scope" "$base_oid" HEAD)"
grep -Fq "merge-base: $base_oid" <<<"$scope"
grep -Fq $'A\tcommitted.txt' <<<"$scope"
grep -Fq $'A\tstaged.txt' <<<"$scope"
grep -Fq $'M\tbase.txt' <<<"$scope"
grep -Fq $'?\tuntracked.txt' <<<"$scope"

if (cd "$test_root" && "$change_scope" does-not-exist HEAD >/dev/null 2>&1); then
  echo "change scope test: invalid base unexpectedly succeeded" >&2
  exit 1
fi

echo "change scope test: pass"
