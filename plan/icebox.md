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

Resolved: the OMITTED default is now **10000ms** (10s wall clock) — the user's
call. buildTree applies it when no explicit server ops budget is set; a query
that finishes under 10s stays deterministic, a 10s+ one trips with the
nondeterministic label. `SetQueryWorkBudget` and an `Nops` arg both force the
deterministic path. Trade: the old restrictive 200k-op default (truncated many
broad edge queries) is gone; broad queries now complete.

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

## Field reports — redline2 dogfooding (2026-07-20)

Three items from an agent driving poly-lsp-mcp across a full redline2 session
(many `/go` slices). Reading/nav (`structure grep=`, `node_read` + its "call
again with startLine=" hints) were the wins and got used constantly; editing
drifted to the host's built-in Edit + Bash. Why, filed as work:

- **BUG — `search` blows the token budget instead of capping.** A broad regex
  that hit generated files returned a **422 (~422k chars)** and force-dumped to
  a scratch file with "read it in chunks" instructions — a hard stop mid-task.
  Repro: `search "View as coach|View user record"` over redline2 matched
  `ui/src/gql/*` (generated, ~40k-char lines). `grep --exclude` never does this.
  Fix: a byte cap + truncation summary (`N more matches — narrow with
  glob=/path=`), the way node_read / node_query already degrade; and/or
  default-skip generated / huge-line files (the noise-dir set already skips
  .git/node_modules/vendor — generated source with 40k-char lines is the same
  hazard). Single sharpest edge — it cost a real detour.

- **FEATURE (reinforces the existing "Child-LSP edge precision" item) — Go
  refs/rename read as lexical, so I never trusted them.** The host doc flags
  node_references / node_refactor as tree-sitter-lexical (not gopls) for Go;
  that caveat alone suppressed 100% of my rename use — I fell back to Edit +
  grep every time. The existing icebox fix (resolve a site via
  textDocument/definition, stamp refConf `lsp`) is the answer; this is field
  evidence it gates ADOPTION, not just precision. For a tool named *LSP*,
  lexical rename is the expectation-gap that most undersells it.

- **NOTE (mostly host-side) — node_edit doesn't fire the project's custom
  post-edit checks, so built-in Edit won.** node_edit already returns LSP
  diagnostics (phase 6.1), but redline2's value came from PROJECT-custom
  PostToolUse hooks (a reflection-based nullability test, graphql-eslint) that
  fire only on the host's Edit/Write tool, not on an MCP edit — and they caught
  real errors the instant I saved. Net: gopls diagnostics ⊄ project validation,
  and the hook wiring keys on the built-in tool, so it's likely a Claude Code
  harness concern, not poly-lsp's to fix — but it's the #1 reason node_edit went
  unused here. Recorded as the honest adoption blocker; the safe-edit thesis
  competes directly against it.
