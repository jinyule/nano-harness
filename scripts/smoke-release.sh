#!/usr/bin/env bash
set -euo pipefail

artifact_dir="${1:-dist}"
case "$(uname -s)" in
  Darwin) archive_os="Darwin" ;;
  Linux) archive_os="Linux" ;;
  *) echo "release smoke: unsupported host OS $(uname -s)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  arm64 | aarch64) archive_arch="arm64" ;;
  x86_64 | amd64) archive_arch="x86_64" ;;
  *) echo "release smoke: unsupported host architecture $(uname -m)" >&2; exit 1 ;;
esac

archive="$(find "$artifact_dir" -maxdepth 1 -type f -name "*_${archive_os}_${archive_arch}.tar.gz" -print -quit)"
if [[ -z "$archive" ]]; then
  echo "release smoke: ${archive_os} ${archive_arch} archive not found in $artifact_dir" >&2
  exit 1
fi

smoke_dir="$(mktemp -d)"
cleanup() {
  status=$?
  trap - EXIT
  rm -rf -- "$smoke_dir"
  exit "$status"
}
trap cleanup EXIT
tar -xzf "$archive" -C "$smoke_dir"
"$smoke_dir/nano-harness" version

if [[ ! -f "$artifact_dir/checksums.txt" ]]; then
  echo "release smoke: checksums.txt not found" >&2
  exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
  (cd "$artifact_dir" && sha256sum -c checksums.txt)
else
  (cd "$artifact_dir" && shasum -a 256 -c checksums.txt)
fi
