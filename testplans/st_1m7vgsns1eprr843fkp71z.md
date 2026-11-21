# Test plan: st_1m7vgsns1eprr843fkp71z

The tree is gofmt-clean: `gofmt -l` lists zero non-testdata files. Before the fix this fails on the 10 files named in F6; after `gofmt -w` it passes.

```sh produces=gofmt-all.txt
set -e
gofmt -l . > "$OUT/gofmt-all.txt" 2>&1
out=$(grep -v -e '^testdata/' -e '/testdata/' "$OUT/gofmt-all.txt" || true)
if [ -n "$out" ]; then
  echo "gofmt -l lists non-testdata files:"
  echo "$out" | sort
  exit 1
fi
echo "clean: gofmt -l lists zero non-testdata files"
```

Apply `gofmt -w` to every non-testdata file `gofmt -l` lists, then re-verify zero remain. This is the fix: the 10 files named in F6 plus any others base picked up since a46cd79.

```sh produces=before.txt,after.txt
set -e
gofmt -l . > "$OUT/before.txt"
files=$(grep -v -e '^testdata/' -e '/testdata/' "$OUT/before.txt" || true)
if [ -z "$files" ]; then
  echo "already clean"
  exit 0
fi
echo "formatting:"
echo "$files" | sort
echo "$files" | sort | while read -r f; do
  gofmt -w "$f"
done
gofmt -l . > "$OUT/after.txt"
out=$(grep -v -e '^testdata/' -e '/testdata/' "$OUT/after.txt" || true)
if [ -n "$out" ]; then
  echo "still unformatted after gofmt -w:"
  echo "$out" | sort
  exit 1
fi
echo "formatted and clean"
```

Re-verify after the `gofmt -w` pass: zero non-testdata files listed.

```sh produces=final.txt
set -e
gofmt -l . > "$OUT/final.txt"
out=$(grep -v -e '^testdata/' -e '/testdata/' "$OUT/final.txt" || true)
if [ -n "$out" ]; then
  echo "gofmt -l lists non-testdata files:"
  echo "$out" | sort
  exit 1
fi
echo "clean: gofmt -l lists zero non-testdata files"
```

Re-verify after the `gofmt -w` pass: zero non-testdata files listed.

```sh produces=verdict.txt
set -e
gofmt -l . > "$OUT/final2.txt"
out=$(grep -v -e '^testdata/' -e '/testdata/' "$OUT/final2.txt" || true)
if [ -n "$out" ]; then
  echo "gofmt -l lists non-testdata files:"
  echo "$out" | sort
  exit 1
fi
echo "clean: gofmt -l lists zero non-testdata files" > "$OUT/verdict.txt"
cat "$OUT/verdict.txt"
```
