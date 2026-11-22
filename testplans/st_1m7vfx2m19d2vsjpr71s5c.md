# F4: check step refuses a leaked poly-lsp-mcp-* temp dir

## 1. A leaked dir is refused and named

With a fake TMPDIR holding `poly-lsp-mcp-mp-test-abc123`, `poly-lsp-mcp-srv-test-xyz789` and an unrelated dir, the script exits 1, names both leftovers, and does not report the unrelated dir.

```sh
set -e
work=$(mktemp -d)
fake_tmp="$work/faketmp"
mkdir -p "$fake_tmp/poly-lsp-mcp-mp-test-abc123" "$fake_tmp/poly-lsp-mcp-srv-test-xyz789" "$fake_tmp/unrelated-dir"
touch "$fake_tmp/poly-lsp-mcp-mp-test-abc123/poly-lsp-mcp"
out=$(TMPDIR="$fake_tmp" bash scripts/check/no-leaked-temp-dirs.sh 2>&1) && { echo "script unexpectedly succeeded: $out"; exit 1; }
echo "$out" | tee "$OUT/leak-run.log"
echo "$out" | grep -q "poly-lsp-mcp-mp-test-abc123" || { echo "leftover mp dir not named"; exit 1; }
echo "$out" | grep -q "poly-lsp-mcp-srv-test-xyz789" || { echo "leftover srv dir not named"; exit 1; }
echo "$out" | grep -q "2 poly-lsp-mcp-\* dirs" || { echo "count of 2 not reported"; exit 1; }
echo "$out" | grep -q "unrelated-dir" && { echo "unrelated dir wrongly reported"; exit 1; }
echo "leak path: OK — refused, both leftovers named, unrelated dir ignored"
```

## 2. A clean run passes; leftovers are named by full path

An empty temp dir exits 0 with an OK line. A single leftover is refused and named by its full path, so the "Remove them with" line is copy-pasteable from any CWD.

```sh
set -e
work=$(mktemp -d)
fake_tmp="$work/faketmp"
mkdir -p "$fake_tmp"
out=$(TMPDIR="$fake_tmp" bash scripts/check/no-leaked-temp-dirs.sh) || { echo "clean run refused: $out"; exit 1; }
echo "$out" | tee "$OUT/clean-run.log"
echo "$out" | grep -q "OK" || { echo "no OK line"; exit 1; }

# Leak again and check the full paths are named.
mkdir -p "$fake_tmp/poly-lsp-mcp-mp-test-abc123"
out=$(TMPDIR="$fake_tmp" bash scripts/check/no-leaked-temp-dirs.sh 2>&1) && { echo "leak not refused: $out"; exit 1; }
echo "$out" | tee "$OUT/leak-run2.log"
echo "$out" | grep -q "  $fake_tmp/poly-lsp-mcp-mp-test-abc123" || { echo "full path not named"; exit 1; }
echo "clean path + full-path naming: OK"
```

## 3. The Makefile target refuses a leak and passes clean

`make no-leaked-temp-dirs` (a prerequisite of `check`) fails over a leaked dir, naming it, and passes when the temp dir is clean.

```sh
set -e
work=$(mktemp -d)
fake_tmp="$work/faketmp"
mkdir -p "$fake_tmp/poly-lsp-mcp-mp-test-abc123"
# The Makefile target, with TMPDIR pointed at a dir holding a leak: must fail.
if TMPDIR="$fake_tmp" make no-leaked-temp-dirs 2>"$OUT/make-fail.log"; then
  echo "make target passed over a leak"; exit 1
fi
cat "$OUT/make-fail.log"
grep -q "poly-lsp-mcp-mp-test-abc123" "$OUT/make-fail.log" || { echo "make output does not name the leftover"; exit 1; }
# Clean: must pass.
rm -rf "$fake_tmp/poly-lsp-mcp-mp-test-abc123"
TMPDIR="$fake_tmp" make no-leaked-temp-dirs | tee "$OUT/make-ok.log"
grep -q "OK" "$OUT/make-ok.log" || { echo "make target did not report OK"; exit 1; }
echo "make no-leaked-temp-dirs: OK — wired into check, refuses a leak, passes clean"
```
