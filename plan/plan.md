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

## Current state

- Phases 0–6.1 (scaffold, multiplex, cross-language index + rename,
  stacked-branch parse cache, MCP server, diagnostics-in-edit-responses,
  tool ergonomics) — ✅ all in `done.md`.
- MCP default surface: **3 tools** — node_query / node_read / node_edit —
  over one unified node tree (project > dir > file > symbols > argument),
  addressed as `<file>#<sym>`, queried by CSS selector. Legacy 9-tool surface
  behind `--legacy-tools`. Sandbox jail + read-only mode: commit `0fbeb02`.
- Selector language: CSS containment + the graph as NODES. References are
  reified edge pseudo-elements — `::in`/`::out`, kind as class
  (.call/.type/.import), far end as child, site as address (file@line),
  invisible to `*` (gates are opaque). `{m,n}` = regex repetition on
  elements/(groups); edge hops on an edge element. `:parents(sel)` = the one
  inverse (upstream roots). Bare `:any/:all/:empty` = position claims.
  Language classes (file.go/func.ts), `:first`/`:last` per anchor. Shipped
  2026-07-17 in three slices → done.md ("Graph selector language") for the
  full design record; slice 3 ("references are NODES") is the shipped
  language.

## Active work

✅ **Edge cost — fixed, → done.md.** The direction split + the once-per-query
index inversion together took `func::out` from budget-dead to its full 24,590
matches inside the 200k default, and `func#main::out.call > func` back to life.
The budget is no longer the binding constraint on any query tried. Remaining,
opt-in:
  - ◻ **Nothing short-circuits.** `evaluate()` computes the FULL set and the
    caller slices after, so `--limit 5` and `:first` pay for everything. Much
    less urgent now that the sweep is gone, but it is why `:first` costs what
    no-`:first` costs. Traversal is document-ordered, so a top-level early exit
    at offset+limit is sound. **blocking decision**: it costs `totalMatches`
    (you cannot report "of 24,590" without finishing) — node_query's result
    shape and truncation contract would change.
  - ◻ **Move the inversion into `symbols.Index`** (build it once per index, not
    per query). Would drop the last fixed ~66k prefix off every edge query.
    **risk**: it is derived state, so it needs invalidating on Refresh /
    RemoveFiles / file-watch — the per-query version has no staleness surface,
    which is why it was chosen first.
**Assumption made**: `:parents` wants incoming edges only — that is what the
old code filtered for, and the split now hard-codes it.

◐ **Query planning — leading-ref pushdown done, more possible.** A global
leading ref filtered to an exact far name (`::in.call#'Save'`) used to expand
the implied universal host to every symbol and build every edge before
discarding all but the few whose far end is Save. Now the candidate hosts are
derived from the index — the far ends of Save's opposite-direction edges — so
the sweep never happens (measured 6 hosts vs 4,206 for #Save; equivalence and
"never costs more" gated by test). Fixing this also surfaced and fixed a real
double-count: edges were attributed by sym-path name alone, so a `module main`
and a `func main` both claimed the same call site — now attribution requires
span containment (→ done.md). **next**:
  - ◻ **Cardinality-order a descendant chain.** `A B` still evaluates
    left-to-right; if B is far rarer than A, starting from B (then checking
    ancestors) is cheaper. Needs per-compound cardinality estimates from the
    index. Bigger change; the ref pushdown was the measured 700× case.
  - ◻ **The ~76k inversion floor is per-query.** Every edge query rebuilds
    sitesByFile; on a 3× workspace even anchored edge queries approach the
    budget. Cache the inversion in symbols.Index (invalidate on Refresh).
**Common dev queries are NOT pathological**: at the default 200k budget, every
query in the common set (callers/callees of X, dead funcs, non-test funcs,
type members, transitive 2-hop) completes — the broad `::in.call` over the
whole workspace is the ceiling at 159k.

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
**Assumption made**: `textDocument/definition`'s first location is the
declaration. True for gopls; unverified for tsserver/pylsp.

◻ **Query CLI + determinism** — shipped this pass, see done.md:
`poly-lsp-mcp query [flags] <selector>` + `bin/dev` launcher; deterministic
truncation; `[path]` axis + de-leaked `[name]`. **next**: the budget call
above. **risks**: `[name]` de-leak is BREAKING (`func[name*=test]` 508 → 1);
`bin/dev` is untested on darwin/arm64.

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
