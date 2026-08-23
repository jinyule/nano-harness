#!/usr/bin/env bash
set -euo pipefail

notes_root=".agents/notes"
failed=0

while IFS= read -r note; do
  filename="$(basename "$note")"
  if [[ ! "$filename" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}-[a-z0-9]+(-[a-z0-9]+)*\.md$ ]]; then
    echo "agent notes: invalid filename $note" >&2
    failed=1
  fi
  for heading in "## Context" "## Decision" "## Consequences" "## Verification"; do
    if ! grep -Fxq "$heading" "$note"; then
      echo "agent notes: $note is missing $heading" >&2
      failed=1
    fi
  done
  if ! grep -Eq '^- Status: (proposed|implemented|rejected)$' "$note"; then
    echo "agent notes: $note has an invalid or missing Status" >&2
    failed=1
  fi
  if ! grep -Eq '^- Date: [0-9]{4}-[0-9]{2}-[0-9]{2}$' "$note"; then
    echo "agent notes: $note has an invalid or missing Date" >&2
    failed=1
  fi
done < <(find \
  "$notes_root/proposed" \
  "$notes_root/implemented" \
  "$notes_root/rejected" \
  "$notes_root/archived" \
  -type f -name '*.md' | sort)

if [[ "$failed" != 0 ]]; then
  exit 1
fi

base="${AGENT_NOTE_BASE_REF:-}"
if [[ -z "$base" ]]; then
  echo "agent notes: format valid; no base ref supplied, change requirement not evaluated"
  exit 0
fi

if [[ "$base" =~ ^0+$ ]]; then
  base="$(git hash-object -t tree /dev/null)"
  diff_range="$base HEAD"
else
  git cat-file -e "$base^{commit}"
  diff_range="$base...HEAD"
fi

# Intentional word splitting turns the trusted, internally composed range into
# the two/one arguments expected by git diff.
# shellcheck disable=SC2086
changed="$(git diff --name-only $diff_range)"
# shellcheck disable=SC2086
archived_edits="$(git diff --diff-filter=MDR --name-only $diff_range -- "$notes_root/archived")"
if [[ -n "$archived_edits" ]]; then
  echo "agent notes: archived notes are frozen:" >&2
  printf '%s\n' "$archived_edits" >&2
  exit 1
fi

material="$(printf '%s\n' "$changed" | grep -Ev '^\.agents/notes/' || true)"
if [[ -z "$material" ]]; then
  echo "agent notes: note-only change"
  exit 0
fi

if [[ "${AGENT_NOTE_EXEMPT:-}" == "1" ]]; then
  [[ -n "${AGENT_NOTE_EXEMPT_REASON:-}" ]] || {
    echo "agent notes: exemption requires AGENT_NOTE_EXEMPT_REASON" >&2
    exit 1
  }
  echo "agent notes: mechanical exemption accepted: $AGENT_NOTE_EXEMPT_REASON"
  exit 0
fi

# shellcheck disable=SC2086
changed_notes="$(git diff --diff-filter=AM --name-only $diff_range -- "$notes_root/proposed" "$notes_root/implemented" "$notes_root/rejected")"
if [[ -z "$changed_notes" ]]; then
  echo "agent notes: non-trivial change must add or update an Agent Note" >&2
  echo "agent notes: use $notes_root/TEMPLATE.md or obtain a maintainer-owned mechanical exemption" >&2
  exit 1
fi

echo "agent notes: change carries $(printf '%s\n' "$changed_notes" | wc -l | tr -d ' ') note(s)"
