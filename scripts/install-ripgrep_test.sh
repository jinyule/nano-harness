#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
test_root="$(mktemp -d)"
trap 'rm -rf -- "$test_root"' EXIT

fail() {
  echo "install-ripgrep test: $*" >&2
  exit 1
}

case "$(uname -s)/$(uname -m)" in
  Linux/x86_64) target="x86_64-unknown-linux-musl" ;;
  Darwin/arm64) target="aarch64-apple-darwin" ;;
  Darwin/x86_64) target="x86_64-apple-darwin" ;;
  *) fail "unsupported test host $(uname -s)/$(uname -m)" ;;
esac
version="15.2.0"
archive="ripgrep-$version-$target.tar.gz"

# A local release tree with a lookalike archive: it extracts and runs, so
# only the pinned checksum can stop the installer.
mkdir -p "$test_root/package/ripgrep-$version-$target" "$test_root/release/$version"
cat > "$test_root/package/ripgrep-$version-$target/rg" <<EOF
#!/usr/bin/env bash
echo "ripgrep $version"
EOF
chmod +x "$test_root/package/ripgrep-$version-$target/rg"
tar -czf "$test_root/release/$version/$archive" -C "$test_root/package" "ripgrep-$version-$target"

if RIPGREP_RELEASE_URL="file://$test_root/release" "$script_dir/install-ripgrep.sh" "$test_root/tampered" 2> "$test_root/tampered.err"; then
  fail "a checksum mismatch was accepted"
fi
grep -Fq "checksum mismatch for $archive" "$test_root/tampered.err" || fail "unexpected tampered error: $(cat "$test_root/tampered.err")"
[[ ! -e "$test_root/tampered/rg" ]] || fail "a rejected archive was installed"

if RIPGREP_RELEASE_URL="file://$test_root/missing" "$script_dir/install-ripgrep.sh" "$test_root/absent" 2> "$test_root/absent.err"; then
  fail "a missing archive was accepted"
fi
grep -Fq "could not download $archive" "$test_root/absent.err" || fail "unexpected download error: $(cat "$test_root/absent.err")"
[[ ! -e "$test_root/absent/rg" ]] || fail "a missing archive produced a binary"

if "$script_dir/install-ripgrep.sh" 2> "$test_root/usage.err"; then
  fail "a missing destination was accepted"
fi
grep -Fq "usage: install-ripgrep.sh <directory>" "$test_root/usage.err" || fail "unexpected usage error"

echo "install-ripgrep test: checksum, download, and usage failures rejected"
