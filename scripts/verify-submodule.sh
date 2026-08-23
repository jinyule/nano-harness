#!/usr/bin/env bash
set -euo pipefail

path="third_party/deepseek-harness"
expected_url="https://github.com/deepseek-ai/deepseek-harness.git"

actual_url="$(git config -f .gitmodules --get "submodule.$path.url")"
if [[ "$actual_url" != "$expected_url" ]]; then
  echo "submodule: $path URL is $actual_url, expected $expected_url" >&2
  exit 1
fi

status="$(git submodule status -- "$path")"
case "${status:0:1}" in
  -) echo "submodule: $path is not initialized; run git submodule update --init --recursive" >&2; exit 1 ;;
  +) echo "submodule: $path is not at the commit recorded by the parent repository" >&2; exit 1 ;;
  U) echo "submodule: $path has an unresolved merge conflict" >&2; exit 1 ;;
esac

if [[ -n "$(git -C "$path" status --short)" ]]; then
  echo "submodule: $path contains local changes" >&2
  exit 1
fi

echo "submodule: $status"
