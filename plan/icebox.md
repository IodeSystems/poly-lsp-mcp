# icebox — deferred, opt-in next-steps

Nothing here is scheduled. See plan.md for active work; done.md for the
graph-selector design record this used to hold.

---

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
