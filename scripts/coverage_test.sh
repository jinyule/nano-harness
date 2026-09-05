#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
test_root="$(mktemp -d)"
trap 'rm -rf -- "$test_root"' EXIT
export COVERAGE_TEST_GO="$(command -v go)"
export COVERAGE_TEST_PROFILE="$test_root/input.out"
mkdir "$test_root/bin"
cat > "$test_root/go.mod" <<'EOF'
module example.test/coverage

go 1.26
EOF
cat > "$test_root/value.go" <<'EOF'
package coverage

func Value() int {
  value := 1
  return value
}
EOF
# Substitute only test execution; Go's real coverage formatter demonstrates
# that a positive uncovered count can still be displayed as 100.0%.
cat > "$test_root/bin/go" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  list) printf 'example.test/coverage\n' ;;
  test)
    for argument in "$@"; do
      case "$argument" in
        -coverprofile=*) cp "$COVERAGE_TEST_PROFILE" "${argument#-coverprofile=}"; exit 0 ;;
      esac
    done
    exit 1
    ;;
  tool) exec "$COVERAGE_TEST_GO" "$@" ;;
  *) exit 1 ;;
esac
EOF
chmod +x "$test_root/bin/go"

for count in 0 1; do
  cat > "$COVERAGE_TEST_PROFILE" <<EOF
mode: atomic
example.test/coverage/value.go:4.3,4.13 20000 1
example.test/coverage/value.go:5.3,5.15 1 $count
EOF
  report="$(cd "$test_root" && "$COVERAGE_TEST_GO" tool cover -func=input.out)"
  [[ "$report" == *100.0%* ]] || { echo "coverage test: rounding fixture did not round to 100.0%" >&2; exit 1; }
  result=0
  (cd "$test_root" && PATH="$test_root/bin:$PATH" "$script_dir/coverage.sh") > "$test_root/output" 2>&1 || result=$?
  if [[ "$count" == 0 ]]; then
    [[ "$result" != 0 ]] || { echo "coverage test: rounded 100.0% hid an uncovered statement" >&2; exit 1; }
    grep -Fq 'coverage: uncovered statements in raw profile' "$test_root/output"
  else
    [[ "$result" == 0 ]] || { cat "$test_root/output"; exit 1; }
  fi
done

echo "coverage test: pass"
