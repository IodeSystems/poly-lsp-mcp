# icebox — deferred, opt-in next-steps

Nothing here is scheduled. See plan.md for active work; done.md for the
graph-selector design record this used to hold.

---

## Wall-clock budget — SHIPPED as ms/ops-suffixed, → done.md

`budget` now carries a unit: `Nops` deterministic work units, `Nms` (and a bare
number) wall clock. Ms is the intuitive default; ops is the reproducible
opt-in. The determinism hazard is HANDLED, not ignored — a time-tripped result
labels itself nondeterministic ("vary run to run; use Nops for a reproducible
cut"), distinct from the deterministic work-budget note. Both capped (5M ops /
30s) so a runaway can't stall the server.

Open decision (USER owns): the OMITTED default is still 200000 **ops**
(deterministic) — a bare *provided* value is ms, but no-budget-given keeps the
deterministic default. If the omitted default should also become a wall-clock
time, that is a one-line change; left deterministic-by-default deliberately.

## `return` as a node — the remaining trivia sibling

Shipped `annotation` and `comment` as first-class child nodes (decorators,
struct tags, joined doc blocks). `return` is the natural third: the return
TYPE as a `.return` child, so `func:any(return#error)` = funcs returning
error, `method:any(return#'*Server')`, etc. Needs per-language extraction of
the return type node (Go `result` field, TS `type_annotation` after params,
Python `->` annotation) — a `returnType(lang, node)` mirroring the decorator
helpers. Not built yet; `argument` already covers the params side.

Also noted: `argument` is a plain tag node, NOT `::arg` — `func > argument`
and `func:any(argument#ctx)` already work.

## `:recursive` and other EDGE-semantic predicates — need call-target precision

Prototyped `:recursive` (a callable that directly calls itself: a self-edge in
its own ::out.call). Removed unshipped: it is lexically UNSOUND for the common
case. `func Write(w io.Writer)` calling `w.Write(body)` name-matches the local
`func Write`, so a lexical self-edge appears and the func reads as recursive —
and Write/Read/Close/String/Error (every io/fmt interface method) hit this. A
boolean predicate cannot carry the lexical-vs-lsp caveat the edge rows do, so
it would just be wrong.

Ship only once the child-LSP precision pass resolves the self-call's target
(then a self-edge is real). The same bar applies to any predicate built on
edge SEMANTICS — "calls X", "reachable from X", cyclic/mutual recursion. Safe
to build now (text/structure, no edge guessing): the shipped `:annotated`,
`~=` regex, `:contains`, containment queries. Idea worth a slice: `:arity(m,n)`
/ signature-size filters (count of `argument` children — structural, sound).

## Ownership domains — crossing into libraries (the North Star chasm)

Precision resolves into dependencies by definition (gopls→stdlib,
tsserver→`node_modules`, pylsp→`site-packages`), so the single-`.project`
assumption is already leaking. Stage 0 (the honest external stub) is ACTIVE on
the Edges slice in plan.md. Stages 1–2 are deferred design:

- **Domain as a node property.** One root becomes a FOREST of domain roots:
  `owned` (rw, file-watched, small, hot) + N × `lib:<module>@<version>` (ro,
  immutable, large, cold). The tree SHAPE stays uniform — the `.project`-level
  foresight in query.go pays off — nodes just gain `domain` + `mutable`.
  `node_edit` refuses non-`owned` domains structurally; `[domain=…]` becomes a
  selectable axis.
- **Lazy, evictable partitions ("spin up/down").** A lib subtree materializes
  only when a query OPTS IN to crossing (a `:with(libs)` scope, or drilling
  into a stub), and is evictable under memory pressure. Content-addressed by
  `module@version` → indexed once per MACHINE, reused across projects, never
  re-indexed (released libs are immutable). This is what makes millions of LOC
  tractable: the cost is paid once, globally, not per-project-per-query.
- **Partition-aware budget.** The 200k budget assumes a bounded root; one
  `::in.call` on `Write` across the stdlib enumerates every implementer in
  every dep. So the DEFAULT stays "don't cross" (today's honest bounded
  behavior); crossing is opt-in and separately budgeted per partition.
- **Per-domain freshness.** `owned` invalidates per keystroke; `lib@version`
  indexes are immutable and cache FOREVER. The invalidation surface the
  inversion-floor / `:explain`-tally work builds must be domain-aware from the
  start, or it re-scans immutable libs for nothing.

Do NOT build until a query actually needs to cross (adoption-gated, like the
rest). But the domain axis must be NAMED now so nothing hard-codes single-root
deeper — see the North Star rule in plan.md.

## Graph selectors — deferred remainder

Slice 1 (the `:parents` move, hop ranges, `:where/:any/:all/:empty`) shipped
2026-07-17; group ranges and :depth retirement are ACTIVE in plan.md. Still
parked here:

- **Path-level `:where`.** Today `:where` ≡ `:any` because tips are tested
  one by one; true path filtering (prune arriving paths so a later ∀ sees the
  subset) needs the traversal to retain predecessor edges. The evaluator
  keeps reachability SETS — cheap, cycle-safe — and edges were deliberately
  not recorded until a consumer exists. If this ships, the reachability-
  subgraph representation (nodes + crossed edges, O(V+E), never enumerate
  paths) is the design; see done.md.
- **Same-path ∀.** Known trade, written down in done.md: the subgraph unions
  paths, so `:all` says "every node between a and b matches" and cannot say
  "there EXISTS a path whose every hop matches". Only revisit with a real
  query that needs it.
- **Adoption measurement.** Capability ≠ adoption: given bash, nothing used
  poly-lsp (0 calls / 8 runs); only recipes keyed to the caller's actual
  state moved usage, and only removing the alternative (cedeFileTools) made
  it the default. Before investing in more language, measure whether
  :parents gets used unprompted — and if not, whether the description's
  recipes ("you have store.go#Save — now what") are the fix, per the
  2026-07-16 evidence in done.md.

## Selector language — opt-in next steps (post slice 3)

- **Child-LSP edge precision.** Edges are name-keyed (lexical); resolve a
  site to its TRUE target via textDocument/definition when a child LSP
  (gopls/tsserver) is running, stamping refConf "lsp" per edge. The refConf
  field already exists on every edge node; no reshaping needed.
- **More edge kinds.** .ptr / .read / .write / (ts) .implements — per-language
  tree-sitter context. The class vocabulary is closed and validated, so new
  kinds are additive.
- **`:with(project.go)` — scoped views.** A prelude that installs a global
  :root filtered to a language/subtree (user sketch, 2026-07-17; prior:
  CSS @scope, SQL WITH). Language classes (file.go) cover the per-compound
  case today; :with is query-wide state and needs its own design pass.
- **More pseudo-elements, same contract.** ::doc / ::signature / ::body as
  generated readable/editable sub-parts of a symbol — invisible to `*`,
  addressable like edge sites.
