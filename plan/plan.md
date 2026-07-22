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
Bridge is staged — **Stage 0 SHIPPED**: a resolved far end outside the root is
now an honest EXTERNAL STUB (`module@version#sym`, `domain: external`,
read-only, `[not indexed]`) — nameable, never a false local (see the Edges
slice below). Stages 1–2 (content-addressed lib partitions, on-demand,
evictable) are deferred design in icebox ("Ownership domains"). **Rule until
then: nothing new may hard-code the single-root assumption deeper** — a node's
`domain` (owned rw / vendored ro / external ro) is the axis that will gate
mutation and budget; the field now EXISTS on treeNode (`"" `=owned,
`"external"`), populated by Stage 0.
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
toolsets.

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
    the model's native grep/read in a real agent" needs a REAL agent — and the
    one we had wired (autowork3) is a DEAD PROJECT (dropped 2026-07-21, user
    call). The native-vs-newcomer asymmetry is now UNMEASURED with no vehicle;
    llm-bench's standing value is A/B descriptions as peers, correctness, cost.
  - ◻ **Collision canary** (`collision*`): grep AND lexical node_query both
    return the merged set (verified) — flips to a graph win only when the LSP
    precision pass resolves the site. Re-run with precision ON to show the
    graph's real differentiator.
**Do NOT rewrite `modern.go`'s description** — across quick + in-flow, two
sets × three variants, the shipped spec-first desc already saturates reach and
graph-first. The wording is not the bottleneck on neutral ground; if `inspired`
taught anything it's that MORE prose is worse, not better.
**blocking decision (USER owns)**: adoption can no longer be measured on
home turf (autowork3 dead). Either find/build another real-agent vehicle, or
accept the llm-bench peer result and let the roadmap ride on it.

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
per edge, with tri-state `conf: lsp|lexical|unsettled` on every row. **next**:
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
  - ✅ **Transitive queries still compound — now they SAY where trust runs
    out.** `::in.call{1,}` spends the per-query LSP cap shallowest-first, so
    deep hops fall back to name-keying. **Decided (USER): not a refusal — a
    WARNING. Say what IS precise and what wasn't.** Shipped: `conf` is now
    TRI-STATE — the old `lexical` conflated two opposites, split into `lexical`
    (name UNIQUE in workspace → certain without an LSP) and `unsettled` (≥2
    same-named decls, no LSP settled it → a GUESS listing candidates).
    `refineFar`/`refineIn` return `unsettled` on every ambiguous-unresolved
    path (incl. the out-of-root `picked==nil` case — Stage 0 no longer reads as
    a false-local `lexical`). `precisionNote` is hop-aware: `evalRepeat`→
    `noteHop` tallies per-hop, and a transitive walk reports "crossed up to N
    hops; M unsettled edges begin at hop K — distant nodes least certain" (or
    "all LSP-resolved or name-unique" when clean). Tests (no-gopls, fast):
    `TestTriStateConfSplitsCertainFromGuess`, `TestTransitiveNoteReportsUnsettledHop`,
    `TestTransitiveNoteCleanWalkSaysSo`; `TestWithoutLSPEdges…` renamed to assert
    `unsettled`. Docs (grammar CONF line, modern.go conf comment, query_text
    caveat `lsp|lexical|unsettled`) updated. **Remaining**: per-hop LSP CAPS
    (spend budget across depth, not all on hop 1) were NOT done — the warning
    makes the current spend honest; a fairer spend is a separate opt-in.
  - ✅ **`::in`/precision round-trips are now cached across queries.** A warm
    session re-asked `textDocument/definition` for the same site every query
    (#New = 93 callers; every edge-precision pass, `:recursive`, external-stub
    resolution). `resolveDefinition` now memoizes on a Server-side `defCache`
    keyed on the site position, valid for ONE index `Generation()` — any
    mutation drops the whole cache, so a stale definition can't outlive an
    edit. The LSP round-trip runs OUTSIDE the lock (never serializes concurrent
    queries); negatives are cached too (an unresolvable site isn't re-asked).
    `defMisses` counts real round-trips. Tests: `TestDefCacheGenInvalidation`,
    `TestDefCacheCachesNegatives` (mechanics), `TestResolveDefinitionCachedAcrossQueries`
    (gopls e2e: identical 2nd query = 0 new round-trips). Pure perf, no
    behavior change.
  - ✅ **The LSP cap is TUNED from the workspace collision rate (Timsort-style).**
    The flat `defaultLSPResolveCap = 200` is gone. Only AMBIGUOUS edges cost a
    round-trip, and names are Zipfian (most unique → free), so the cap only has
    to cover the collision-prone tail: it now scales with the count of declared
    names that have ≥2 EDGE-TARGETABLE declarations (params/return/annotation
    excluded) — `tunedLSPCap` = floor 64 + 4/name, ceilinged at 1500 (bounded
    cost = the explainable-cost moat). Set LAZILY (`ensureLSPCap`) on the first
    round-trip off the already-built `declsByName` (free — the edge builds it
    first), so a non-edge query never pays. Legible: the cap + collision counts
    surface in `precisionNote` when it's hit; `SetLSPResolveCap` overrides.
    **Measured on THIS repo: 216 collision-prone of 1964 declared → cap 928**
    (4.6× the old flat 200 — a collision-heavy codebase gets budget where it
    needs it; a clean one sits at the floor and never hits it). Tests:
    `TestTunedLSPCapFormula`, `TestLSPCapTunedFromCollisions`,
    `TestLSPCapExplicitOverride`. **Next (the broader "tune for code" arc):**
    same treatment for the other magic constants (generated-file line
    threshold as a percentile, etc.), and adaptive re-plan when realized
    cardinality diverges from `:explain`'s estimate.
  - ✅ **A resolved far end OUTSIDE the root is now an EXTERNAL STUB** (North
    Star Stage 0 — SHIPPED). `refineFar`'s `picked==nil` path splits on
    `filepath.IsLocal(defRel)`: outside the root → mint an `external` node
    (`module@version#sym`, `domain:"external"`, ro, `[not indexed]`, conf `lsp`)
    as the far end; inside-but-unmatched stays `unsettled`. `externalIdentity`
    derives the identity best-effort per ecosystem (Go mod cache `@version`,
    stdlib/`node_modules`/`site-packages` package path, dir-base fallback) — always
    nameable, never a false local. `addr()`/`nodeIDs()` handle the class;
    node_query flags the row `domain:"external"`. Tests: `TestExternalIdentity`,
    `TestExternalStubShape` (fast), `TestPrecisionResolvesToExternalStub`
    (gopls e2e: two local `Split` + a `strings.Split` call → `strings#Split`, no
    false local). **Scope note**: only fires on the ≥2-candidate ambiguous path
    (`refineFar` skips len<2); a single local candidate that's actually external
    still fast-paths as `lexical` (asking the LSP per unambiguous edge is the
    cost the skip buys) — documented limitation, not this slice.
  - ✅ **`:recursive` — the first edge-SEMANTIC predicate, LSP-confirmed.** A
    callable with a self-call the child LSP resolves back into its OWN span.
    Unblocked by the precision pass (the icebox parked it as lexically unsound:
    `func Write` calling `w.Write` is io.Writer's, not itself). `isRecursive`
    walks `::out.call` for a self far end; `confirmSelfEdge` trusts an edge the
    precision pass already resolved to it (conf lsp), else re-resolves the site
    (name-unique self-edges are never LSP-checked at build) via the stored
    `refCol`. No LSP ⇒ confirms nothing and SAYS so (`recursive` note / CLI
    caveat), never a silent false negative. Bare only — mutual/cyclic rejects
    an arg and points at `::out.call{1,}`. Tests: `TestRecursivePredicateLSPConfirmed`
    (gopls: fib + method self-call yes; Write/Plain/mutual no),
    `TestRecursiveWithoutLSPIsUnderResolved`.
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
  - ✅ **`return` as a NODE — shipped.** The return TYPE is now a `return`
    CHILD of every callable (`func:any(return#error)` = funcs returning error),
    across Go/TS/Python (`appendReturnSymbols` + `returnTypeNodes`: Go `result`,
    TS/Python `return_type`). Go's `(T, error)` tuple SPLITS into one child per
    type so `return#error` matches it; a qualified type answers to its leaf
    (`return#Writer`) and its full alias (`return#'io.Writer'`). Three
    integration snags, all fixed + pinned: a `return` node's span is the
    signature line, so `enclosingSymPath` must skip it (else it steals call/type
    sites from the func — like `argument`); its name span sits ON the type
    usage, so `isDeclSite` must skip it (else the ref is deleted); and it answers
    to `#Type`, so it's excluded from `refNodes` edge-building (else
    `#Type::in.type` doubles). Tests: `symbols/return_node_test.go` (Go/TS/Py),
    `TestModernQueryReturnNode`. **Remaining** (icebox): `var` slot nodes, and
    the return-VALUE slot (needs column precision — param vs return share a line).
  - ✅ **`:arity(m,n)` — signature-size filter, shipped.** Sound/structural
    (counts `argument` children, no edge guessing): `:arity(2)` exact,
    `:arity(2,)` 2+, `:arity(0,0)` no-arg. `parseParenRange` mirrors the `{m,n}`
    shape; `TestModernQueryArity`.
  - ✅ **`search`/`::grep` long-line cap + generated-file skip, shipped** (icebox
    field-report BUG). `symbols.CapHitLine` (rune-safe, match-centred, 500B)
    caps every matched line on BOTH surfaces; `symbols.Search` skips files with
    a >5000B line (reported as `skippedGeneratedFiles`, `IncludeGenerated` opts
    back in). Tests: `symbols/cap_test.go`, `TestModernQueryGrepCapsLongLine`.
  - ✅ **`::signature` / `::body` pseudo-elements — shipped.** A callable split
    into its decl HEAD (doc- and body-excluded) and body block, GENERATED nodes
    (invisible to `*`) carrying source INLINE so `func::signature` is a one-query
    overview. `Symbol.BodyStartLine` (tree-sitter `body` field) is the split;
    the doc is skipped via the stored `commentAt`. `genPartOf`/`genPartMatches`
    mirror `::comment`; `isGenerated()` folds them into the planner guards.
    Known imprecision (line-granular): the `{` line shows in both halves on a
    single-line signature — column precision is v2. Tests: `mcp/genpart_test.go`
    (Go/TS/Python, invisibility to `*`, non-callables excluded).

Next candidates are opt-in, in icebox.md — most valuable: a real-agent
adoption vehicle (autowork3 is dead; needs a replacement), then the
external-stub Stage 0 node (conf is now honest, the node is not yet).

Known caveats (documented in code): edges are name-keyed via the lexical
index, so same-named symbols share edges (the LSP pass is the fix);
unbounded `{m,}` collects nodes at their shortest hop; `:where(sel)` ≡
`:any(sel)` at tip granularity (pseudoHolds).

## Non-goals (for now)

- Indexing the entire host filesystem; we only index inside the git root.
- Replacing any single child LSP. We multiplex, we don't reimplement.
- Sandboxing child LSPs. They run as the user.
- Windows support until someone asks.
