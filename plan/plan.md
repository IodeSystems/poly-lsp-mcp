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

◻ **Cost visibility + planning share an estimator.**
  - ◻ **Cardinality-order a descendant chain.** `A B` evaluates left-to-right;
    if B is far rarer than A, start from B and check ancestors. Needs the same
    per-compound estimate `:explain` renders (below). The ref pushdown was the
    measured 700× case; this is the general form.
  - ◻ **The ~76k inversion floor is per-query.** Every edge query rebuilds
    `sitesByFile`; on a 3× workspace even anchored edge queries approach the
    budget. Cache the inversion in `symbols.Index` (invalidate on Refresh) —
    same derived-state hazard as the `:explain` tallies below.
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

◻ **Cardinality-order a descendant chain** (moved up from the shared-estimator
note): `A B` evaluates left-to-right; if B is far rarer than A, start from B.
The per-element `est` `:explain` now renders is the number to reorder on —
build the reorder on top of it. This is where the deferred fan-out estimate
earns its consumer.

◐ **Edges: from coincidence toward reference.** Two of three steps done
(→ done.md): lexical scope killed 99% of far ends (a local is not visible
outside its function), and the child-LSP pass now settles what remains,
per edge, with `conf: lsp|lexical` on every row. **next**:
  - ◻ **The CLI is lexical-only, by choice.** `bin/dev query` has no manager
    (a one-shot gopls spawn is seconds); its tree renderer does NOT show conf,
    so CLI edge output looks identical to a resolved one. Either render conf
    or say "lexical" in the footer — today it is the one place an ambiguous
    edge can still read as a fact.
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
  - ❓ **TS edges double-count.** `Widget::in.type` = 4 on a fixture with 2
    real uses; the doubling is per-USE and TS-specific (Go is clean), so it is
    an index/site-dup in the TS path, NOT the position axis (which split the 4
    into 2 param + 2 return cleanly). Independent bug; find where a TS type site
    enters the index twice.
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
