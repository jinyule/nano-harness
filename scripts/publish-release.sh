#!/usr/bin/env bash
set -euo pipefail

artifact_dir="${1:?usage: publish-release.sh <payload-directory> <tag>}"
tag="${2:?release tag is required}"
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
[[ "$tag" == v* ]] || { echo 'release publication: tag must start with v' >&2; exit 1; }
"$script_dir/verify-release.sh" "$artifact_dir" "${tag#v}"

remote_dir="$(mktemp -d)"
trap 'rm -rf -- "$remote_dir"' EXIT
for artifact in "$artifact_dir"/*; do basename "$artifact"; done | LC_ALL=C sort > "$remote_dir/expected"

fail() { echo "release publication: $*" >&2; exit 1; }

# A partial remote set is valid only before resuming uploads. Every listed
# name is checked before it becomes a download path or a glob argument.
check_remote() {
  gh release view "$tag" --json assets --jq '.assets[].name' > "$remote_dir/names" || fail 'cannot read remote assets'
  LC_ALL=C sort "$remote_dir/names" > "$remote_dir/sorted"
  LC_ALL=C sort -u "$remote_dir/names" > "$remote_dir/unique"
  cmp -s "$remote_dir/sorted" "$remote_dir/unique" || fail 'duplicate remote asset'
  while IFS= read -r name; do
    grep -Fxq -- "$name" "$remote_dir/expected" || fail 'unexpected remote asset'
  done < "$remote_dir/names"
}

if ! gh release view "$tag" >/dev/null 2>&1; then
  gh release create "$tag" --draft --verify-tag --generate-notes --title "$tag"
fi
check_remote
for artifact in "$artifact_dir"/*; do
  name="$(basename "$artifact")"
  if grep -Fxq -- "$name" "$remote_dir/names"; then
    gh release download "$tag" --pattern "$name" --dir "$remote_dir"
    cmp -s "$artifact" "$remote_dir/$name" || fail "asset $name exists with different content"
    rm -- "$remote_dir/$name"
  else
    gh release upload "$tag" "$artifact"
  fi
done

# Re-read and download all assets, including newly uploaded ones, before
# publishing. Successful upload calls alone do not prove the final payload.
check_remote
cmp -s "$remote_dir/expected" "$remote_dir/sorted" || fail 'incomplete remote asset set'
for artifact in "$artifact_dir"/*; do
  name="$(basename "$artifact")"
  gh release download "$tag" --pattern "$name" --dir "$remote_dir"
  cmp -s "$artifact" "$remote_dir/$name" || fail "asset $name differs after upload"
done
gh release edit "$tag" --draft=false
