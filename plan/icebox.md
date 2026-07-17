# icebox — deferred, opt-in next-steps

Nothing here is scheduled. See plan.md for active work; done.md for the
graph-selector design record this used to hold.

---

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
