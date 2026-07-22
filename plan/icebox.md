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

## `:recursive` — SHIPPED (LSP-confirmed), → plan.md/done

Direct self-recursion, gated on child-LSP confirmation (the icebox's
soundness bar): a self-edge whose site the LSP resolves back INTO the func's
own span. `func Write` calling `w.Write` (io.Writer's) is correctly NOT
recursive; `fib` and method self-calls (`s.Loop()`) are. Without an LSP it
confirms nothing and says so (`recursive` note / CLI caveat) rather than
reading as "none found". See `isRecursive`/`confirmSelfEdge`.

Remaining EDGE-semantic predicates (still parked — same LSP bar):
- **Mutual / cyclic recursion** — a cycle over the call graph, not a self-
  edge. `:recursive` rejects an argument today and points at `::out.call{1,}`.
- **"calls X" / "reachable from X"** are already expressible as
  `#'X'::in.call` / `::in.call{1,}` — no new predicate needed.
Safe to build without the LSP (text/structure, no edge guessing): the shipped
`:annotated`, `~=` regex, `:contains`, `:arity(m,n)`, containment queries.

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
- ✅ **`.implements` — SHIPPED (LSP-native).** `interface#Foo::in.implements > *`
  = implementers, `type#Bar::out.implements > *` = interfaces Bar satisfies.
  Unlike site-based kinds it has NO lexical clause (Go structural typing), so
  it's resolved entirely by the child LSP (`textDocument/implementation`,
  cached like definition), built ONLY when explicitly named, conf `lsp`, far
  ends outside the root become external stubs, unavailable-without-LSP says so.
  See `implementsRefs`/`resolveImplementations`.
- **More edge kinds.** .ptr / .read / .write — per-language tree-sitter
  context (site-based, unlike .implements). The class vocabulary is closed and
  validated, so new kinds are additive.
- **`:with(project.go)` — scoped views.** A prelude that installs a global
  :root filtered to a language/subtree (user sketch, 2026-07-17; prior:
  CSS @scope, SQL WITH). Language classes (file.go) cover the per-compound
  case today; :with is query-wide state and needs its own design pass.
- ✅ **`::signature` / `::body` — SHIPPED.** A callable split into its decl
  HEAD (doc- and body-excluded) and its body block, generated nodes carrying
  their source INLINE so `func::signature` is a one-query overview. `::doc` is
  already `::comment`. Body-start comes from `Symbol.BodyStartLine`
  (tree-sitter `body` field); the doc is excluded via the stored `commentAt`.
  Known imprecision (line-granular): the `{` line appears in both halves when
  the signature is single-line — column precision is a v2 refinement.
- **More pseudo-elements, same contract.** Anything else generated/editable —
  invisible to `*`, addressable like edge sites — follows the ::comment/::grep/
  ::signature template.

## Field reports — redline2 dogfooding (2026-07-20)

Three items from an agent driving poly-lsp-mcp across a full redline2 session
(many `/go` slices). Reading/nav (`structure grep=`, `node_read` + its "call
again with startLine=" hints) were the wins and got used constantly; editing
drifted to the host's built-in Edit + Bash. Why, filed as work:

- ✅ **BUG (FIXED 2026-07-21) — `search` blew the token budget instead of
  capping.** A broad regex over generated files dumped ~422k chars and hard-
  stopped mid-task. Fixed BOTH surfaces via `symbols.CapHitLine` (rune-safe,
  match-centred per-line cap, 500B, `(+N chars)` marker): the legacy `search`
  tool AND the modern `::grep` path. Plus a default generated-file skip in
  `symbols.Search` (a line >5000B ⇒ minified/generated ⇒ skipped whole,
  COUNTED and reported as `skippedGeneratedFiles` + a note; `IncludeGenerated`
  opts back in). Tests: `symbols/cap_test.go`, `TestModernQueryGrepCapsLongLine`.

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
