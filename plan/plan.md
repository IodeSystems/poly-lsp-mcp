# poly-lsp-mcp — roadmap

> How this plan works: this file = current state + active work + decisions
> ONLY. `plan/done.md` = archive of completed work. `plan/icebox.md` =
> deferred, opt-in next-steps. Status marks: ◻ todo · ◐ in progress · ✅ done ·
> ⏸ parked · ❓ blocked. Move rules: a finished tree → done.md (leave a
> one-line pointer); a deferred next-step → icebox.md — in the same pass as
> the work.

Fused multi-language LSP + MCP server, one binary. The LSP side multiplexes
child LSPs (gopls/tsserver/pylsp) over a tree-sitter symbol index that crosses
languages (lexical / declared / schema-anchored tiers). `poly-lsp-mcp mcp --root
<dir>` boots the MCP surface on the same index.

## North Star — what "world-class" means for us

The owned quadrant, empty of incumbents: a **live, no-build, multi-language,
reference-aware, edit-capable code querier with a predictable, explainable cost
model** — driven by an LLM agent, live, mid-task. Every priority is judged
against holding THIS position, not against beating CodeQL at its own game.

- **The consumer is a MODEL, not a security engineer** — that inverts the
  weights. A grammar the model already knows (CSS). Loud, honest partiality
  (`conf` labels, budget-blow-that-says-so, `:explain`, `>x` floors) as
  HALLUCINATION-PREVENTION, not polish — a human supplies the skepticism a
  model doesn't. Token-lean, composable output over rich reports.
- **The unoccupied intersection:** {live + no-build} × {cross-file reference
  edges with a precision LADDER} × {mutation} × {predictable cost}.
  CodeQL/Glean/Kythe/Sourcegraph buy the reference graph with a
  build/batch/per-language extractor and cannot rewrite; ast-grep/Comby/Semgrep
  rewrite live but have NO cross-file graph. Nobody sits in the middle.
- **The moat is the precision LADDER:** lexical → tree-sitter-scoped →
  LSP-resolved, each LABELED. "Resolved or lexical, and it says which" is a
  category property no interactive querier ships.

Non-goals — forfeiting these is what KEEPS the quadrant:
- **Dataflow / taint / points-to** (CodeQL's turf) needs the build+batch we
  refuse. Explicit non-goal, not a gap.
- **We do not out-scale Sourcegraph/Glean on multi-repo.** A single owned root
  is home.

**The known chasm — ownership domains.** The precision pass does not ASK to
leave the workspace; it crosses by definition — gopls resolves into the stdlib,
tsserver into `node_modules`, pylsp into `site-packages`. The moment it works,
a `refFar` can land OUTSIDE the git root, where `fileByRel` has no node and the
edge silently falls back to a false local match (the `Write`/`Read` collision
the icebox flags). So the single-`.project` assumption is already leaking, and
"lib linking" is not a feature to add but a boundary already being crossed.
Bridge is staged — Stage 0 (owed NOW by the active Edges work): a resolved
far end outside the root becomes an honest EXTERNAL STUB (`module@version#sym`,
`domain: external`, read-only, `[not indexed]`) — nameable, never a false
local. Stages 1–2 (content-addressed lib partitions, on-demand, evictable) are
deferred design in icebox ("Ownership domains"). **Rule until then: nothing new
may hard-code the single-root assumption deeper** — a node's `domain` (owned rw
/ vendored ro / external ro) is the axis that will gate mutation and budget.
**Decided: crossing into libs is opt-IN** (`:with(libs)` / drill into a stub),
the default stays workspace-bounded — revisit only if adoption data shows
agents actually want the cross-lib answer unprompted.

## Current state

- Phases 0–6.1 (scaffold, multiplex, cross-language index + rename,
  stacked-branch parse cache, MCP server, diagnostics-in-edit-responses,
  tool ergonomics) — ✅ all in `done.md`.
- MCP default surface: **3 tools** — node_query / node_read / node_edit —
  over one unified node tree (project > dir > file > symbols > argument),
  addressed as `<file>#<sym>`, queried by CSS selector. Legacy 9-tool surface
  behind `--legacy-tools`. Sandbox jail + read-only mode: commit `0fbeb02`.
- Selector language: CSS containment + the graph as NODES. References are
  reified edge pseudo-elements — `::in`/`::out` on TWO orthogonal class axes,
  KIND (.call/.type/.import) × POSITION (.return/.param/.field/.var), composed
  CSS-style (`::in.return.type`); far end (via `>`) is the SOURCE symbol, the
  ref IS the occurrence (address = site file@line), invisible to `*`. `{m,n}` =
  regex repetition; edge hops on an edge element. `:parents(sel)` = the one
  inverse. Bare `:any/:all/:empty` = position claims. Language classes
  (file.go), `:first`/`:last` per anchor. Attribute axes: `[name]` (called) vs
  `[path]` (lives), ops `= ^= $= *=` literal, `~=` regex (bracket-aware).
  Shipped 2026-07-17 → done.md ("Graph selector language" + the per-feature
  entries) for the full record.
- Trivia/metadata as NODES (this session, → done.md): `annotation` (decorators
  py/ts + struct-tag keys go, a CHILD of its symbol, leaf + virtual-FQN alias),
  `::comment` (joined doc block, a GENERATED pseudo-element invisible to `*`),
  `argument` (params). `:annotated('pat')`/`:contains('pat')` are the text
  fallbacks (Go comment directives have no AST node).

## Active work

**Shipped this session (all → done.md):** query CLI + `bin/dev`; deterministic
truncation; `[path]` axis + de-leaked `[name]`; `~=` regex (bracket-aware);
edge-cost fixes (direction split + once-per-query inversion → `func::out` fits
the 200k default); child-LSP precision pass (`conf: lsp|lexical`); local-scope
fix (99% far-end noise gone); leading-ref pushdown + containment attribution;
`:annotated`; `annotation` node; `::comment` pseudo-element; `::in.return.type`
position axis. Common dev queries are NOT pathological at the default budget.

Open frontier:

✅ **`--validate` (revert-on-new-diagnostics) + the safe-edit-loop thesis,
shipped, tested, and MEASURED.** The whole arc — reframe → build → benchmark →
tune → measure with error bars.

**Thesis (why):** LLMs run a grep→read→edit loop and reach for grep by habit.
Don't fight it — ABSORB it: keep the loop, make edits *safe*. node_edit is the
edit; `--validate` makes it un-break-able.

**Built (poly-lsp side, all in `mcp/validate.go`):**
- Write paths (range/whole-file/diff) run through `applyBytes`: fingerprint the
  workspace's pre-edit errors, write, re-collect, and if the edit introduces a
  NEW error, atomically restore + report `rejected` (isError=true; `newErrors`).
- Multi-file rename/signature via `validationTxn` — records every touched file's
  pre-edit bytes before writing, reverts them ALL as one unit on any new error
  (nested rename inside signature shares the outer txn: all-or-nothing).
- **CROSS-FILE**: the fingerprint is WORKSPACE-WIDE (`errorFingerprintAll` over
  the store snapshot), so an edit that breaks an IMPORTER (rename a type its
  callers use) is caught — gopls publishes package-level, `settleErrorFingerprint`
  waits for the sibling republish to land before the diff. This was the binding
  constraint the benchmark exposed.
- Server flag `--validate` (or per-call `validate:true`); no-op-but-flagged
  without a child LSP (`validated:false`).
- **Sharpened `node_edit` rename description** (modern.go, shipped default):
  leads with "renaming? use the rename op — one atomic call, don't hand-edit".
- Tests (gopls-backed, stable): `TestNodeEditValidateReverts`,
  `TestRefactorRenameValidateRevertsAllFiles`, `TestNodeEditValidateCrossFileRevert`;
  full mcp suite green.

**Measured (corrallm llm-bench, Qwen3-6-27B-MPT via llm.iodesystems.com, n=5):**
on a cross-file rename (`edit-safety-rename`, type used across 4 files):
| arm | rename-op | broken-intermediates | pass | tokens |
|---|---|---|---|---|
| baseline (shell/read/edit) | n/a | **2 [2–2]** | 5/5 | 9k |
| polylsp / polylsp-validate | **5/5** | **0 [0–0]** | 5/5 | ~30k |
**Net benefit, with error bars:** poly-lsp reliably completes the refactor with
ZERO broken intermediate states (baseline lands 2 every time) — structural
safety grep+sed can't match — at ~3× the token cost. **The lever was
PRESENTATION**: the sharpened description gets Qwen onto the atomic rename op
10/10 runs; that op is safe by construction, so `broken=0` even WITHOUT
validation. Validation is the untested insurance for when presentation doesn't
land (weaker model / harder refactor / hand-editing).

**Lessons the runs taught (each cost a wrong turn until measured):** (1) on
tasks a capable model passes, pass/fail is BLIND to the offering's value — the
`broken_intermediates` safety metric is what separates the arms. (2) `--validate`
is redundant for a diligent model WITH a build tool (it self-heals); its value
needs the no-self-check path (`--run-tool=false`) or a task the model breaks.
(3) single runs LIE — the validate arm "hand-edited (64k tokens)" was n=1 noise;
at n=5 it's 5/5 atomic rename. Always `--runs`.

**Remaining (opt-in):** `validate:"strict"` (refuse, not fail-open, when no LSP);
pre-touch baseline for never-analyzed files (an unanalyzed file with prior
errors could false-revert its first edit — documented limitation); the ~3×
token premium is the thing to shave if this goes wide.

**⚑ corrallm-side changes (their repo, uncommitted — flag for review):**
`services/corrallm` gained, to make the above measurable: the
`broken_intermediates` metric + task `safetyCheck` field (run after each mutating
call); `--run-tool` gate + toolset `baseArgs` (argument the base llm-bench-mcp,
generalizing `cedeFileTools`); `--runs N` + per-run artifact naming (`_rN`); the
`edit-safety-{pop,import,rename}` probes + `polylsp-validate`/`polylsp-norun*`
toolsets. Also: the `scheduler.go` tech_lead user-turn fix (bonsai template) is a
genuine agentkit-host bug fix, unrelated to poly-lsp.

◐ **Adoption measurement — the existential question, now instrumented.**
`llm-bench/` (own nested module, uses `agentkit` — `mcpmgr` spawns the server,
`agent.Session` drives the model on llm.iodesystems.com) poses relationship-
shaped code-nav tasks with BOTH the graph tools AND a strong grep/read/list
baseline present, and tallies **reach-rate** — the fraction of tasks that call
`node_query` at all. Compiles + the server binary spawns (verified); NOT yet
run (needs `AGENTKIT_API_KEY` + network). **next**:
  - ◻ **Run the `asis` baseline.** Reach-rate high → adoption isn't the wall,
    engine work is justified. Low → the icebox's "0 calls / 8 runs" reproduces
    and everything below is premature.
  - ✅ **First A/B run (asis vs pattern vs inspired, 3 direct-question tasks,
    n=1).** Reach-rate SATURATED — 3/3 for all three variants; even spec-first
    `asis` gets node_query reached for (callers answered correctly). **Finding:
    on direct relational questions the docs are NOT the wall** — which means the
    icebox "0 calls / 8 runs" was the IN-FLOW case (model mid-task, grep
    momentum, unprompted), not the wording. The direct-question set can't
    measure the thing that matters; I measured the easy case. Only flicker:
    `pattern` reached graph grep-free most often (weak, n=1).
  - ✅ **In-flow grep-tempting set built + run (5 tasks incl. a collision
    canary; asis/pattern/inspired, n=1).** Metrics: reach, graph-FIRST,
    grep-free. Result across ALL three variants: reach **4/4**, graph-first
    **4/4** even in-flow — the model reaches for node_query FIRST, unprompted,
    on grep-tempting tasks, regardless of doc wording. grep-free was the only
    mover (asis/pattern 3/4, **inspired 2/4** — its longer prose induced MORE
    grep scaffolding; the verbose "inspirations" strategy underperforms tight
    recipes and even the plain spec).
  - ❗ **The bench structurally CANNOT answer the absolute adoption question.**
    In-harness, grep is just another advertised tool with a one-line desc — it
    has NO home-field advantage. The icebox's "0 calls" was grep as the model's
    NATIVE, reflexively-trained tool inside a real agent. This bench is NEUTRAL
    ground, where node_query competes as a peer and wins on description alone.
    So: node_query is *competitive as a peer* (real result), but "does it beat
    the model's native grep/read in a real agent" needs the REAL agent.
  - ◐ **True adoption test = the real agent (autowork3) already exists.**
    `services/autowork3` is a full agentkit worker that spawns poly-lsp-mcp as a
    sidecar (`cmd/worker/mcp_child.go`) AND merges it with native grep/read/ls in
    ONE session (`cmd/worker/main.go:174`) — the home-turf contest llm-bench
    can't stage. Tool calls log to the `spans` table (`attributes.tool_name` +
    `.role`). Query WIRED: `llm-bench/adoption_autowork3.sql` (+ `./pg.sh prod`)
    runs graph-vs-native against the live DB (`postgres://aw3@127.0.0.1:1212`).
    **Result today: EMPTY.** Prod has 3 sessions, all `product_owner`
    (orchestration only — submit_chat/thread_set_title); ZERO `developer`/
    `reviewer` sessions, so poly-lsp-mcp AND grep have never fired. The real
    test still hasn't RUN — worse than the icebox's "0 calls / 8 runs" (this DB
    lacks even those). llm-bench's remaining value: A/B descriptions as peers,
    correctness, cost — not the native-vs-newcomer asymmetry.
  - ❗ **Populating real dev spans = operating the whole autowork3 platform;
    stopped at that boundary.** Attempted (user opted in): booted a HEALTHY
    daemon (`aw daemon serve`, API :8401), confirmed `autowork3-worker-dev`
    images exist. But staging one developer session needs, in sequence: a
    context→repo→WORKTREE binding (the `projects` table that held `repo_path`
    is GONE; `aw context create` has no repo flag; repo lands via a
    `worktree.Manager` installed only in the PROD daemon config, `api.go:236`),
    per-thread worker CONTAINERS, and the product_owner→tech_lead→developer
    summon chain against a cold/flaky model (`Qwen3-6-27B-MPT` state=absent,
    warms on demand). That's autowork3-internal operational setup, unsafe to
    reverse-engineer blind against the prod DB. **Stopped clean: no thread/
    session rows written (prod still 3/3), daemon killed.** `bonsai` is NOT a
    registered autowork3 model nor corrallm-served — unresolved.
  - ◐ **Live populate attempt (bonsai) — chain runs, breaks at tech_lead.**
    `bonsai` = `ternary-bonsai-27b` (corrallm, state=ready; the ONE ready chat
    model — Qwen/gemma absent). Registered it in autowork3 (`aw model add`),
    created a repo-bound project (poly-lsp-mcp, `aw project create --repo-path`),
    booted `aw daemon serve --no-vite` (worktrees auto-install), started a
    thread on bonsai with a code-nav task. **product_owner RAN on bonsai +
    called submit_plan** (pipeline is live). But **tech_lead FAILED**: bonsai's
    GGUF chat template raises `No user query found in messages` → 400 (its
    template asserts a user turn; autowork3 builds tech_lead from the PO plan =
    system+assistant/tool only). So the summon chain dies BEFORE a developer is
    spawned → still no poly-lsp-mcp/grep spans. **Blocker is now a
    bonsai-template ↔ autowork3-message-shape incompatibility** (corrallm or
    autowork3 side), not missing infra. Prod DB now holds: model `bonsai`,
    project `13749f79`, thread `969c7fab` (PO closed, TL failed); daemon left up
    on :8401 for retry.
  - ✅ **tech_lead message-shape fix — DONE, verified.** Root cause: a freshly-
    summoned transient role (tech_lead/dev/reviewer) had its task in the SYSTEM
    prompt and an EMPTY conversation; agentkit's shaper never guarantees a user
    turn, so bonsai's template raised. Fix (`internal/server/scheduler.go`,
    runSession): for non-PO roles with zero prior events, seed one
    `EventUserMessage` ("Proceed with the task…") before the Turn. Retest on a
    fresh thread: **tech_lead failed→CLOSED** — seeded user turn present, it
    decomposed into leaves. Chain advanced PO→TL→developer. (This is an
    autowork3 code change — flag for their review/commit.)
  - ◻ **Next blocker (operational, not code): developer needs a node-manager.**
    developer session failed: `no live node-manager for thread … assign via aw
    node-manager assign`. A node-manager is the long-running worker process that
    orchestrates aw3-worker CONTAINERS (image `autowork3-worker-dev:latest`
    exists). To finish: `aw node-manager register` + `serve` + assign to the
    project → developer runs in a container → poly-lsp-mcp + grep fire →
    `./pg.sh prod` prints the home-turf ratio. Separate operational layer.
  - ◻ **Collision canary** (`collision*`): grep AND lexical node_query both
    return the merged set (verified) — flips to a graph win only when the LSP
    precision pass resolves the site. Re-run with precision ON to show the
    graph's real differentiator.
**Do NOT rewrite `modern.go`'s description** — across quick + in-flow, two
sets × three variants, the shipped spec-first desc already saturates reach and
graph-first. The wording is not the bottleneck on neutral ground; if `inspired`
taught anything it's that MORE prose is worse, not better.
**This gates further engine investment**: keep building the engine once
adoption says agents reach for the graph at all — pause if it says they don't.
**blocking decision (USER owns)**: if both variants stay low, the tool loses on
merit at point-of-use and the roadmap reorders around discovery/triggering, not
features.

◻ **Cost visibility + planning share an estimator.**
  - ◻ **Cardinality-order a descendant chain.** `A B` evaluates left-to-right;
    if B is far rarer than A, start from B and check ancestors. Needs the same
    per-compound estimate `:explain` renders (below). The ref pushdown was the
    measured 700× case; this is the general form.
  - ✅ **The ~76k inversion floor is gone from query budget.** `sitesByFile`
    is now `symbols.Index.SitesByFile` — index-owned derived state, memoized on
    `gen` (invalidates on Refresh), abs-keyed, liveness-evicting at build. An
    edge query no longer charges the inversion to its budget: `func::in.call`
    dropped 89,894 → 13,379 ops (−85%), `struct::in.type` 89,451 → 12,940.
    **Measured caveat (measure-first paid off): the win is OPS, not wall** —
    the inversion was 50ms of a 1.9s query; wall is unchanged because the real
    bottleneck is the per-target far-end build (the O(sites) item below), which
    this does not touch. Also note the 200k→10000ms default already relieved
    the ops PRESSURE this item was written for; the value now is a lower ops
    floor for `Nops` budgets + large workspaces, not a broad speedup. Tests:
    `TestSitesByFile{EquivalenceAndMemo,EvictsVanishedFiles}`; determinism
    budgets in `TestTrippedBudgetIsReproducible` retuned (trip moved into the
    walk).
  - ◻ **Nothing short-circuits.** `evaluate()` computes the FULL set, then the
    caller slices, so `--limit 5` / `:first` pay for everything. Traversal is
    document-ordered so a top-level early exit at offset+limit is sound.
    **blocking decision**: costs `totalMatches` (can't report "of 24,590"
    without finishing) — node_query's result shape changes.

✅ **`:explain` — cost-visible queries, all 3 commits shipped → done.md.**
`:explain <selector>` returns a cost tree: a-priori `est` (free, from the
commit-2 tallies) beside `measured` work, with `>x` lower bounds on the element
the budget tripped in and `—` for unreached elements. The always-on trace also
upgrades every plain budget-blow to point at the culprit. node_query returns
`{"explain": rows, "truncated"}` — a trace, not matches (the result-shape fork
the plan flagged; resolved by making it a MODE, not a change to plain queries).
**Open remainder**: the est column shows `?` for edges/`*` — the index has no
fan-out. A fan-out estimate (from `::out` avg degree, or the pushdown's
opposite-edge count) would fill those in; deferred until the descendant-chain
planner (below) needs it, since they share the estimator.

✅ **Cardinality-order a descendant chain → done.md.** A plain pure-descendant
chain whose tip is an exact NAME far rarer than the broad leading element is
now seeded from the INDEX (`declsNamed` loads only the files containing the
name) and filtered by an ancestor SUBSEQUENCE — `struct #Name` dropped ~6.9k
work → 0, equivalence + "actually cheaper" gated by test. Lesson recorded: the
first cut (seed via collectMatches) was a correct NO-OP — the tree walk negated
the win; the fix was index-seeding + an O(1) decision (`estCardCheap`, NOT
classCounts). **Remaining planner ideas** (opt-in): a bare-class or edge tip
can't be index-seeded (no class/fan-out in the index) — those still forward.
The fan-out estimate `:explain` shows as `?` for edges is the same gap; fill it
only when a query needs an edge-tip reorder.

◐ **Edges: from coincidence toward reference.** Two of three steps done
(→ done.md): lexical scope killed 99% of far ends (a local is not visible
outside its function), and the child-LSP pass now settles what remains,
per edge, with `conf: lsp|lexical` on every row. **next**:
  - ✅ **The CLI is lexical-only, and now SAYS so.** `bin/dev query` has no
    manager (a one-shot gopls spawn is seconds); its tree renders far ends with
    no conf column. Fixed by a footer caveat that fires whenever a selector uses
    `::in`/`::out` (`usesEdge`), on every path — match, traversal-to-symbols,
    empty, or budget-blow: "edges are name-keyed (lexical) here … the MCP server
    resolves via child LSPs (conf: lsp); `query` does not." Pinned by
    `TestQueryTextLexicalEdgeCaveat`. A per-row conf column is the fuller fix
    but needs the manager the CLI deliberately skips; the footer is the honest
    minimum. (Live proof it's needed: `func#New::out.call` shows `engine.s,
    modSelParser.s` — lexical collisions on the field name `s`.)
  - ◻ **Transitive queries still compound.** `::in.call{1,}` crosses many
    edges; each hop past the 200-cap is lexical again. The cap is per QUERY,
    so a deep walk spends it on hop 1. **blocking decision**: per-hop caps? a
    "precise-only" mode that refuses to cross a lexical edge? Doing nothing
    means the marquee feature stays the least trustworthy.
  - ◻ **`::in` on a common name is O(sites) round-trips** (#New = 93). Under
    the cap, but a warm-gopls session pays it per query — the LSP answer is
    not cached across queries.
  - ❓ **A resolved far end OUTSIDE the root has no node today** (North Star
    Stage 0). `fileByRel` only holds workspace files, so when definition lands
    in the stdlib/`node_modules` the edge drops it or falls back to a false
    local match. **verify FIRST**: what does `refineFar` (precision.go) do with
    an out-of-root definition location right now? Then mint an EXTERNAL STUB
    (`module@version#sym`, `domain: external`, ro) instead — the honest
    boundary marker. Owed by this slice, not deferrable: precision CREATES
    these the moment it resolves a library call.
**Assumption made**: `textDocument/definition`'s first location is the
declaration. True for gopls; unverified for tsserver/pylsp.

◻ **Node model — loose ends found this session.**
  - ✅ **TS `::in.type` double-count — NOT reproducible; the ❓ was stale.**
    Verified across interface / class / generic / export / .tsx / union /
    cross-file: `Widget::in.type` counts each occurrence ONCE, split cleanly by
    the position axis (param/return/field), with value refs (`new Widget()`) out
    of `.type`. The index emits 4 DISTINCT positions for 1 decl + 3 uses (no
    site dup); the old "4 on 2 uses" was fixed en route (likely the span-
    containment attribution that stopped name-only double-attribution). Pinned
    by `TestTSInTypeNoDoubleCount` so a real dup can't creep back.
  - ◻ **`return`/`var` slot NODES** (icebox): the position AXIS ships, but a
    `return`-type usage lands on the func (its far end), and `> return` as a
    node does not exist. Adding `return`/`var` slot nodes needs COLUMN precision
    in `treeNode.at` (param vs return share a signature line) — the infra step
    the axis deliberately dodged. `::in.return.type` is the cheaper win that
    covered the headline query.

Next candidates are opt-in, in icebox.md — most valuable: adoption
measurement (does bonsai USE ::in/::out unprompted?), then the child-LSP
edge-precision pass (refConf lexical → lsp).

Known caveats (documented in code): edges are name-keyed via the lexical
index, so same-named symbols share edges (the LSP pass is the fix);
unbounded `{m,}` collects nodes at their shortest hop; `:where(sel)` ≡
`:any(sel)` at tip granularity (pseudoHolds).

## Non-goals (for now)

- Indexing the entire host filesystem; we only index inside the git root.
- Replacing any single child LSP. We multiplex, we don't reimplement.
- Sandboxing child LSPs. They run as the user.
- Windows support until someone asks.
