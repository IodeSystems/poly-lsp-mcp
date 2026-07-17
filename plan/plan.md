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

❓ **The edges are name coincidences, not references** (pre-existing, known,
and now the biggest correctness gap — see "Known caveats" below). Worth
raising to a scheduled slice: the cost work removed the excuse for not
fixing it, and every transitive query (`::in.call{1,}`, the headline feature)
compounds the noise per hop.

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
