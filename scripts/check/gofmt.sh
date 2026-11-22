#!/usr/bin/env bash
#
# Refuse a run whose tree is not gofmt-clean, naming every offending file.
# `gofmt -l` prints the files whose formatting differs from gofmt's; an
# empty list means clean. Files under a testdata/ directory are excluded:
# they are fixtures, hand-written and sometimes deliberately malformed, so
# they must not be reformatted or gated on.
#
# This is the project-check step: it runs in `make check`, so unformatted
# Go fails the gate instead of accumulating commit over commit.
set -u

list=$(mktemp) || exit 1
trap 'rm -f "$list"' EXIT

gofmt -l . > "$list" 2>&1 || {
  echo "FAIL: gofmt -l itself failed:" >&2
  cat "$list" >&2
  exit 1
}

bad=$(grep -v -e '^testdata/' -e '/testdata/' "$list" || true)
if [ -z "$bad" ]; then
  echo "gofmt: OK — tree is gofmt-clean"
  exit 0
fi

{
  echo "FAIL: tree is not gofmt-clean; the following files need formatting:"
  echo "$bad" | sort | sed 's/^/  /'
  echo "Fix with: gofmt -w <file>"
} >&2
exit 1
