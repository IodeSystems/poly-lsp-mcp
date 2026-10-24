# Evaluation: how changes to the surface are judged

Three instruments, each answering a question the others cannot. Findings
from any of them go to aw4 issues in project `poly-lsp-mcp`.

## 1. Transcript scan (the loop for surface changes)

Read real agent sessions: pair `tool_call` / `tool_result` out of
`~/.dun/sessions/*.jsonl` and count both the errors and the corrective
notes, which is where a surface tells on itself. It measures the model that
actually ran, on real work, against the surface as shipped.

Loop for any description or grammar change: ship, scan the next sessions,
keep or revert. It caught the `|` semantics (0 boolean ORs vs 19 regex
alternations, reverted) and confirmed the space-rule change broke nothing
(0 selectors used the old form).

## 2. Recipe sweep

Run every recipe the tool description advertises and read the OUTPUT, not
the exit code. A test asserts what the code does; the output is where you
see what it says. Defects found this way: the `file@line` page-1 read, the
`|` semantics, `{m,n}` reading as containment on an edge walk, and
`totalMatches: 0` for a walk that never finished. Twice the "bug" was a grep
of the sweeper's own output, so check the display filter first.

## 3. Benches

- **llm-bench** (harness in corrallm; probes here): `bench/llm-bench.yaml`
  (the `polylsp*` toolsets + `full`) and `bench/probes/`. Run:
  `llm-bench run --config bench/llm-bench.yaml`. A box names
  `.../poly-lsp-mcp/bench/probes` once in its corrallm config; probe dirs
  are references, not copies. The probes that ask "did the model reach for
  node_query" are statements about poly-lsp, so they live here (Carl,
  2026-08-02).
  - Limit: in the harness, grep is just another advertised tool. It measures
    node_query as a peer on neutral ground, where it already wins (reach and
    graph-first saturated). It cannot answer whether an agent with its own
    native grep/read reaches for node_query. That needs a real agent: dun,
    driven through a generic external-agent subject in corrallm (decided
    2026-08-01/02).
  - The `find-render-entrypoints` fixture is a pinned snapshot of `mcp/`.
    Refreshing it moves the numbers, so it is its own change. `_fixture`
    dirs are skipped by the index walk.
- **volcycle** (real agent, real Android app): lives at
  `~/local/src/termux/` (durable, not a scratchpad). `run-arms.sh` drives
  dun with and without poly-lsp on one task; `score-volcycle.sh` scores KEEP
  and WORK separately and never sums them. Every number is n=1 per arm, one
  model, one task; the scorer's textual tiers have passed three things that
  did not work.

## Measurement hygiene

- Check that a corpus is live (git log dates, not file mtimes) before
  claiming a measurement on it; label thin samples as indicative.
- Check test runs by exit code: the suite logs to stdout and can hide a
  FAIL.
- `-tags measure` tests (`TestMeasureRootResident`,
  `TestMeasureWorktreeSharing`, `daemon/measure_test.go`) produce the
  numbers quoted in `docs/design.md`; rerun them rather than trusting a
  quoted figure on a different repo.
