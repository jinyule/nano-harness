#!/usr/bin/env bash
set -euo pipefail

profile="${COVERAGE_PROFILE:-coverage.out}"
packages="$(go list ./... | grep -v '/internal/tools/' || true)"

if [[ -z "$packages" ]]; then
  echo "coverage: no product packages found"
  exit 0
fi

# Package paths contain no shell whitespace by Go's import-path rules.
# shellcheck disable=SC2086
go test -count=1 -covermode=atomic -coverprofile="$profile" $packages

report="$(go tool cover -func="$profile")"
printf '%s\n' "$report"

# The human-readable percentages round to one decimal place. Inspect counters
# as well so an uncovered statement cannot disappear into a displayed 100.0%.
if ! awk 'NR > 1 && $NF == 0 && $(NF - 1) > 0 { print; missing = 1 } END { exit missing }' "$profile"; then
  echo "coverage: uncovered statements in raw profile" >&2
  exit 1
fi

uncovered="$(printf '%s\n' "$report" | awk '$1 != "total:" && $3 != "100.0%" {print}')"
total="$(printf '%s\n' "$report" | awk '$1 == "total:" {print $3}')"
if [[ "$total" != "100.0%" || -n "$uncovered" ]]; then
  echo "coverage: every function in every product source file must be 100.0%" >&2
  if [[ -n "$uncovered" ]]; then
    printf '%s\n' "$uncovered" >&2
  fi
  exit 1
fi

echo "coverage: every product source file is 100.0%"
