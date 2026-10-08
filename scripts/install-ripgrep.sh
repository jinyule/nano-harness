#!/usr/bin/env bash
# Install the pinned ripgrep release into a directory after verifying the
# official archive's SHA-256. Updating the pin is a manual, reviewed change;
# see docs/development.md#ripgrep.
set -euo pipefail

version="15.2.0"
destination="${1:?usage: install-ripgrep.sh <directory>}"
# The release base is overridable only so tests can serve local fixtures.
release_url="${RIPGREP_RELEASE_URL:-https://github.com/BurntSushi/ripgrep/releases/download}"

case "$(uname -s)/$(uname -m)" in
  Linux/x86_64) target="x86_64-unknown-linux-musl" expected="33e15bcf1624b25cdd2a55813a47a2f95dbe126268203e76aa6a585d1e7b149c" ;;
  Darwin/arm64) target="aarch64-apple-darwin" expected="3750b2e93f37e0c692657da574d7019a101c0084da05a790c83fd335bad973e4" ;;
  Darwin/x86_64) target="x86_64-apple-darwin" expected="af7825fcc69a2afc7a7aea55fc9af90e26421d8f20fe59df32e233c0b8a231c1" ;;
  *) echo "install-ripgrep: no pinned ripgrep $version archive for $(uname -s)/$(uname -m)" >&2; exit 1 ;;
esac

archive="ripgrep-$version-$target.tar.gz"
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT

if ! curl -fsSL --retry 3 -o "$work/$archive" "$release_url/$version/$archive"; then
  echo "install-ripgrep: could not download $archive" >&2
  exit 1
fi
if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "$work/$archive" | cut -d ' ' -f 1)"
else
  actual="$(shasum -a 256 "$work/$archive" | cut -d ' ' -f 1)"
fi
if [[ "$actual" != "$expected" ]]; then
  echo "install-ripgrep: checksum mismatch for $archive: expected $expected, got $actual" >&2
  exit 1
fi

tar -xzf "$work/$archive" -C "$work"
mkdir -p "$destination"
install -m 0755 "$work/ripgrep-$version-$target/rg" "$destination/rg"
reported="$("$destination/rg" --version | head -n 1)"
if [[ "$reported" != "ripgrep $version"* ]]; then
  echo "install-ripgrep: installed binary reports '$reported', expected ripgrep $version" >&2
  exit 1
fi
echo "install-ripgrep: $reported -> $destination/rg"
