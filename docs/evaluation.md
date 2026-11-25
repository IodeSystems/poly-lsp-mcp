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
- Attribute which child serves which root from the root's own
  `Manager.Status()` (state, RSS, stop counters), not a `ps` listing: a
  `ps` line names the binary but not its workspace, so it cannot say which
  gopls is the parent repo's and which is the worktree's. (The first pass
  of the fleet re-measurement below read eviction off a `ps` listing and
  got it wrong; the manager status is authoritative.)

## Finding: two worktrees of one repo under the child-LSP fleet (2026-11-24)

Re-measured the issue's scenario (i_1m5r2t7k3dczk49mtwctmv) on current main
with the child-LSP fleet at the daemon's defaults (`--lsp-max-children 4`,
`--lsp-rss-budget-pct 25`, `--lsp-idle-stop 10m`), which the issue's
pre-fleet numbers do not cover: `TestMeasureWorktreeSharingFleet`
(`-tags measure`), this repo + one detached worktree in one Registry.

- **No eviction at two worktrees.** Both gopls stay running after each root's
  resolved query: gopls A (repo) 500 MiB, gopls B (worktree) 466 MiB, summed
  ~990 MiB — far under the 31 GiB budget (25% of 124 GiB RAM). `capStops` and
  `budgetStops` are 0 on both; re-querying A is 278 ms with no gopls wake.
  The fleet's LRU/eviction path never fires for two worktrees.
- **The default cap is consumed by two worktrees.** `running=4 cap=4` with two
  roots: each spawns a gopls *and* a typescript server, so 2 roots × 2
  languages = 4, exactly the daemon-wide cap. A third worktree (a 5th child)
  is where the LRU stop begins — and, being daemon-wide, it would evict
  across repos, not within one. That cross-repo eviction is the only delta a
  per-repo budget (proposal 1) would change, and it is only reachable with
  ≥3 worktrees under the default cap.
- **Per-gopls RSS is lower here than the issue's 0.8–2.15 GB** (466–500 MiB):
  this repo is small and a single resolved query loads the workspace without
  heavy analysis. The *shape* (one gopls per root, the dominant cost) is
  unchanged; the absolute figure is repo-dependent, so treat 0.8–2 GB as the
  large-repo bound, not the default.
