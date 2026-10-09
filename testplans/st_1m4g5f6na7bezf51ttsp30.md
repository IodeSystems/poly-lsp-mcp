# Test plan: st_1m4g5f6na7bezf51ttsp30

Probe the session environment: is `go` present, does the local Qwen model endpoint at 127.0.0.1:11434/v1 respond, and does the graphq harness build? This decides whether the model rounds can be run from this session at all.

```sh inputs=tools/graphq/main.go
echo "=== go ==="; go version 2>&1 | head -2
echo "=== model endpoint probe ==="
curl -s -m 6 http://127.0.0.1:11434/v1/models 2>&1 | head -c 400
echo ""
echo "CURL_EXIT: $?"
echo "=== harness build ==="
go build ./tools/graphq 2>&1 | head -20
echo "BUILD_EXIT: $?"
```

Definitive probe of the local Qwen model endpoint: real curl exit codes, the Ollama-native version endpoint, and whether anything is listening on 11434. Determines if this session can run the model rounds at all.

```sh
echo "=== direct curl, no pipe (real exit code) ==="
resp=$(curl -sS -m 8 http://127.0.0.1:11434/v1/models 2>&1); rc=$?
echo "CURL_EXIT: $rc"
echo "BODY: $resp"
echo ""
echo "=== try /api/version (ollama native) ==="
resp2=$(curl -sS -m 8 http://127.0.0.1:11434/api/version 2>&1); rc2=$?
echo "CURL_EXIT: $rc2"
echo "BODY: $resp2"
echo ""
echo "=== is anything listening on 11434? ==="
(ss -ltn 2>/dev/null || netstat -ltn 2>/dev/null) | grep -E '11434|LISTEN' | head -10
echo "LISTEN_DONE"
```

The harness compiles cleanly and all 22 unit tests (canned fixtures, no model, no network) pass. These tests cover bank parsing (both array and object shapes), the GSL/Cypher/SQL executors, and the scorer — the full preconditions for the operator run.

```sh inputs=tools/graphq/bank.go,tools/graphq/main.go,tools/graphq/model.go,tools/graphq/exec_gsl.go,tools/graphq/exec_cypher.go,tools/graphq/exec_sql.go,tools/graphq/score.go
echo "=== build ==="
go build ./tools/graphq 2>&1
echo "BUILD_EXIT: $?"
echo ""
echo "=== unit tests (canned fixtures, no model, no network) ==="
go test -v -count=1 ./tools/graphq 2>&1 | tail -30
echo "TEST_EXIT: ${PIPESTATUS[0]}"
```

The committed question bank (graphq/bank.json) is valid JSON in the array shape that parseBank accepts, has ~20 questions with id/question/expected fields, and all three language card files exist and are non-empty. These are the exact inputs the operator will pass to the harness.

```sh inputs=graphq/bank.json,graphq/cards/gsl.md,graphq/cards/cypher.md,graphq/cards/sqlite.md
echo "=== bank.json shape ==="
python3 -c "
import json, sys
with open('graphq/bank.json') as f:
    bank = json.load(f)
assert isinstance(bank, list), f'expected array, got {type(bank)}'
print(f'questions: {len(bank)}')
for q in bank:
    for field in ('id','question','expected'):
        assert field in q, f'q {q.get(\"id\",\"?\")} missing {field}'
    print(f'  {q[\"id\"]:8s} {q[\"question\"][:60]}...')
print('BANK OK')
"
echo ""
echo "=== card files ==="
for f in graphq/cards/gsl.md graphq/cards/cypher.md graphq/cards/sqlite.md; do
    lines=$(wc -l < "$f")
    echo "  $f: $lines lines"
done
echo "CARDS OK"
```
