#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
test_root="$(mktemp -d)"
trap 'rm -rf -- "$test_root"' EXIT
export RELEASE_TEST_MARKER="$test_root/executed"

fail() {
  echo "release verification test: $*" >&2
  exit 1
}

checksums() {
  (
    cd "$1"
    if command -v sha256sum >/dev/null 2>&1; then
      sha256sum nano-harness_* > checksums.txt
    else
      shasum -a 256 nano-harness_* > checksums.txt
    fi
  )
}

mkdir "$test_root/base" "$test_root/bin"
for os in Darwin Linux Windows; do
  for arch in arm64 x86_64; do
    extension=tar.gz
    [[ "$os" != Windows ]] || extension=zip
    printf 'fixture %s %s\n' "$os" "$arch" > "$test_root/base/nano-harness_1.2.3-test_${os}_${arch}.$extension"
  done
done

case "$(uname -s)" in
  Darwin) host_os=Darwin ;;
  Linux) host_os=Linux ;;
  *) fail "unsupported test host" ;;
esac
case "$(uname -m)" in
  arm64 | aarch64) host_arch=arm64 ;;
  x86_64 | amd64) host_arch=x86_64 ;;
  *) fail "unsupported test architecture" ;;
esac
host_archive="nano-harness_1.2.3-test_${host_os}_${host_arch}.tar.gz"
cat > "$test_root/bin/nano-harness" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[[ "$1" == version ]]
printf 'executed\n' > "$RELEASE_TEST_MARKER"
printf 'nano-harness 1.2.3-test\n'
EOF
chmod +x "$test_root/bin/nano-harness"
tar -czf "$test_root/base/$host_archive" -C "$test_root/bin" nano-harness
checksums "$test_root/base"

# Keep the changed archive valid on BSD and GNU tar so checksum rejection,
# rather than a platform-specific extraction error, must prevent execution.
cp -R "$test_root/base" "$test_root/corrupt"
printf '# changed after packaging\n' >> "$test_root/bin/nano-harness"
tar -czf "$test_root/corrupt/$host_archive" -C "$test_root/bin" nano-harness
if "$script_dir/smoke-release.sh" "$test_root/corrupt" > "$test_root/output" 2>&1; then
  fail "corrupted archive was accepted"
fi
[[ ! -e "$RELEASE_TEST_MARKER" ]] || fail "unverified executable ran before checksum rejection"

"$script_dir/verify-release.sh" "$test_root/base" 1.2.3-test > "$test_root/output"
"$script_dir/smoke-release.sh" "$test_root/base" > "$test_root/output"
[[ -f "$RELEASE_TEST_MARKER" ]] || fail "valid packaged executable did not run"

# Build metadata stays outside the publication payload.
cp -R "$test_root/base" "$test_root/dist"
mkdir "$test_root/dist/build"
printf '{}\n' > "$test_root/dist/artifacts.json"
"$script_dir/prepare-release.sh" "$test_root/dist" "$test_root/prepared" > "$test_root/output"
diff -r "$test_root/base" "$test_root/prepared"
if "$script_dir/prepare-release.sh" "$test_root/dist" "$test_root/prepared" > "$test_root/output" 2>&1; then
  fail "existing payload directory was reused"
fi

for scenario in extra hidden missing duplicate omitted traversal mixed-version wrong-extension archive-link manifest-link directory empty bad-hash wrong-version; do
  fixture="$test_root/$scenario"
  cp -R "$test_root/base" "$fixture"
  expected_version=1.2.3-test
  case "$scenario" in
    extra) printf 'extra\n' > "$fixture/unlisted.zip" ;;
    hidden) printf 'extra\n' > "$fixture/.unlisted" ;;
    missing) rm "$fixture/$host_archive" ;;
    duplicate) head -n 1 "$fixture/checksums.txt" >> "$fixture/checksums.txt" ;;
    omitted) tail -n +2 "$fixture/checksums.txt" > "$test_root/partial"; mv "$test_root/partial" "$fixture/checksums.txt" ;;
    traversal) printf '%064d  ../outside.tar.gz\n' 0 > "$fixture/checksums.txt" ;;
    mixed-version)
      mv "$fixture/$host_archive" "$fixture/${host_archive/1.2.3-test/1.2.4}"
      checksums "$fixture"
      ;;
    wrong-extension)
      mv "$fixture/nano-harness_1.2.3-test_Windows_arm64.zip" "$fixture/nano-harness_1.2.3-test_Windows_arm64.tar.gz"
      checksums "$fixture"
      ;;
    archive-link) rm "$fixture/$host_archive"; ln -s "$test_root/base/$host_archive" "$fixture/$host_archive" ;;
    manifest-link) rm "$fixture/checksums.txt"; ln -s "$test_root/base/checksums.txt" "$fixture/checksums.txt" ;;
    directory) mkdir "$fixture/unlisted" ;;
    empty) : > "$fixture/$host_archive"; checksums "$fixture" ;;
    bad-hash) printf 'invalid manifest\n' > "$fixture/checksums.txt" ;;
    wrong-version) expected_version=9.9.9 ;;
  esac
  if "$script_dir/verify-release.sh" "$fixture" "$expected_version" > "$test_root/output" 2>&1; then
    fail "$scenario payload was accepted"
  fi
  grep -Fq 'release verification:' "$test_root/output" || fail "$scenario failed outside the validator"
done

if "$script_dir/prepare-release.sh" "$test_root/extra" "$test_root/invalid-payload" > "$test_root/output" 2>&1; then
  fail "preparation accepted an extra archive"
fi

echo "release verification test: pass"
