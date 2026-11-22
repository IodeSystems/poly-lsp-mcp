#!/usr/bin/env bash
#
# Refuse a run that left a poly-lsp-mcp-* directory in the OS temp dir,
# naming every leftover. The mp and srv TestMains build the repo binary
# into os.MkdirTemp("", "poly-lsp-mcp-{mp,srv}-test-*"); a run that leaks
# one leaves a ~40 MB repo copy per package run (measured 46 GB over 1163
# dirs before the helpers cleaned up). The Go side of this guard lives in
# each TestMain, proven by TestTempDirRemovedOnFailedRun under multiplex/ and
# server/. This script is the project-check step: it runs after the test targets in
# `make check`, so a leak fails the gate even if a test binary died before
# its own cleanup ran.
#
# Honours TMPDIR, as os.TempDir does in Go.
set -u

tmp="${TMPDIR:-/tmp}"
left=()
while IFS= read -r -d '' d; do
  left+=("$d")
done < <(find "$tmp" -mindepth 1 -maxdepth 1 -type d -name 'poly-lsp-mcp-*' -print0 2>/dev/null)

if [ "${#left[@]}" -eq 0 ]; then
  echo "no-leaked-temp-dirs: OK — no poly-lsp-mcp-* dir in $tmp"
  exit 0
fi

{
  echo "FAIL: a test run left ${#left[@]} poly-lsp-mcp-* dirs in $tmp:"
  for d in "${left[@]}"; do
    echo "  $d"
  done
  echo "Remove them with: rm -rf ${left[*]}"
} >&2
exit 1
