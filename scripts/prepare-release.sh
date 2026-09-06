#!/usr/bin/env bash
set -euo pipefail

source_dir="${1:?usage: prepare-release.sh <goreleaser-directory> <payload-directory>}"
payload_dir="${2:?usage: prepare-release.sh <goreleaser-directory> <payload-directory>}"
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# GoReleaser also writes build directories and metadata. Only publication
# candidates enter the payload; verification owns the exact archive inventory.
mkdir "$payload_dir"
shopt -s nullglob
for artifact in "$source_dir"/*.tar.gz "$source_dir"/*.zip "$source_dir"/checksums.txt; do
  cp -P "$artifact" "$payload_dir/"
done
"$script_dir/verify-release.sh" "$payload_dir"
