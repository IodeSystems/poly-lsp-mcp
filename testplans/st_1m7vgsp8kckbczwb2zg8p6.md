# Test plan: st_1m7vgsp8kckbczwb2zg8p6

The tree is gofmt-clean before the gate exists: `gofmt -l` lists zero non-testdata files. If any of the 10 files named in F6 (or any others since base a46cd79) are unformatted, apply `gofmt -w` to them first, then re-verify.

```sh produces=verdict.txt inputs=daemon/daemon_test.go,daemon/measure_test.go,daemon/registry.go,daemon/trust_test.go,internal/git/repo_test.go,mcp/query.go,symbols/comments.go,symbols/gat_fixture_test.go,symbols/prewarm_test.go,symbols/refactor_test.go
set -e
before=$(gofmt -l . | grep -v -e '^testdata/' -e '/testdata/' || true)
if [ -n "$before" ]; then
  echo "unformatted non-testdata files before:"
  echo "$before" | sort
  echo "$before" | sort | while read -r f; do gofmt -w "$f"; done
fi
after=$(gofmt -l . | grep -v -e '^testdata/' -e '/testdata/' || true)
if [ -n "$after" ]; then
  echo "FAIL: still unformatted after gofmt -w:"
  echo "$after" | sort
  exit 1
fi
echo "clean: gofmt -l lists zero non-testdata files" > "$OUT/verdict.txt"
cat "$OUT/verdict.txt"
```

The gate passes on a clean tree: `bash scripts/check/gofmt.sh` exits 0.

```sh produces=pass.log,pass.rc inputs=scripts/check/gofmt.sh
set -e
bash scripts/check/gofmt.sh > "$OUT/pass.log" 2>&1 && rc=0 || rc=$?
[ "$rc" -eq 0 ] || { echo "FAIL: gofmt.sh exited $rc on a clean tree:"; cat "$OUT/pass.log"; exit 1; }
cat "$OUT/pass.log"
echo "$rc" > "$OUT/pass.rc"
```

The gate fails a dirty tree and names the file (scratch tree only): one unformatted `bad.go` makes the script exit 1 with `bad.go` in the failure output.

```sh produces=fail-stderr.log inputs=scripts/check/gofmt.sh
set -e
mkdir -p "$OUT/failtree3"
cp scripts/check/gofmt.sh "$OUT/failtree3/gofmt.sh"
printf 'package x\nfunc\t f()  { x:=1 ; _ =x }\n' > "$OUT/failtree3/bad.go"
rc=0
( cd "$OUT/failtree3" && bash gofmt.sh > stdout.log 2> stderr.log ) || rc=$?
[ "$rc" -eq 1 ] || { echo "FAIL: expected exit 1, got $rc:"; cat "$OUT/failtree3/stdout.log" "$OUT/failtree3/stderr.log"; exit 1; }
grep -q 'bad.go' "$OUT/failtree3/stderr.log" || { echo "FAIL: offending file not named:"; cat "$OUT/failtree3/stderr.log"; exit 1; }
cp "$OUT/failtree3/stderr.log" "$OUT/fail-stderr.log"
echo "gate failed with exit 1 and named bad.go"
```

testdata is excluded: a scratch tree whose only unformatted Go file is under `testdata/` passes the gate (exit 0), so fixtures are not gated on.

```sh produces=testdata-pass.log inputs=scripts/check/gofmt.sh
set -e
mkdir -p "$OUT/failtree4/testdata"
cp scripts/check/gofmt.sh "$OUT/failtree4/gofmt.sh"
printf 'package x\nfunc\t f()  { x:=1 ; _ =x }\n' > "$OUT/failtree4/testdata/bad.go"
rc=0
( cd "$OUT/failtree4" && bash gofmt.sh > stdout.log 2> stderr.log ) || rc=$?
[ "$rc" -eq 0 ] || { echo "FAIL: testdata-only unformatted tree must pass, got $rc:"; cat "$OUT/failtree4/stdout.log" "$OUT/failtree4/stderr.log"; exit 1; }
cat "$OUT/failtree4/stdout.log" > "$OUT/testdata-pass.log"
```
