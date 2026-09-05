#!/usr/bin/env bash
set -euo pipefail

artifact_dir="${1:?usage: verify-release.sh <payload-directory> [expected-version]}"
expected_version="${2:-}"

fail() {
  echo "release verification: $*" >&2
  exit 1
}

[[ -d "$artifact_dir" ]] || fail "payload directory is missing"
manifest="$artifact_dir/checksums.txt"
[[ -f "$manifest" && ! -L "$manifest" ]] || fail "checksums.txt must be a regular file"

# Accept only the six archive names produced by .goreleaser.yml. A checksum
# listing alone neither rejects unlisted files nor proves target completeness.
pattern='^([0-9a-f]{64})  (nano-harness_([0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?)_(Darwin|Linux|Windows)_(x86_64|arm64)\.(tar\.gz|zip))$'
count=0
seen="/"
while IFS= read -r line || [[ -n "$line" ]]; do
  [[ "$line" =~ $pattern ]] || fail "invalid checksum entry"
  name="${BASH_REMATCH[2]}"
  version="${BASH_REMATCH[3]}"
  os="${BASH_REMATCH[5]}"
  extension="${BASH_REMATCH[7]}"
  if [[ -z "$expected_version" ]]; then
    expected_version="$version"
  fi
  [[ "$version" == "$expected_version" ]] || fail "archive version does not match $expected_version"
  case "$os/$extension" in
    Darwin/tar.gz | Linux/tar.gz | Windows/zip) ;;
    *) fail "wrong archive format for $os" ;;
  esac
  case "$seen" in
    *"/$name/"*) fail "duplicate checksum entry for $name" ;;
  esac
  seen+="$name/"
  [[ -f "$artifact_dir/$name" && ! -L "$artifact_dir/$name" && -s "$artifact_dir/$name" ]] || fail "$name must be a nonempty regular file"
  count=$((count + 1))
  [[ "$count" -le 6 ]] || fail "too many checksum entries"
done < "$manifest"
[[ "$count" == 6 ]] || fail "checksums.txt must list all six platform archives"

shopt -s nullglob dotglob
entries=("$artifact_dir"/*)
[[ "${#entries[@]}" == 7 ]] || fail "payload must contain exactly six archives and checksums.txt"

if ! (
  cd "$artifact_dir"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum -c checksums.txt
  else
    shasum -a 256 -c checksums.txt
  fi
); then
  fail "archive checksum mismatch"
fi

echo "release verification: complete $expected_version payload verified"
