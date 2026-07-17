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
- Selector language: CSS containment + the graph half — `:parents(sel){m,n}`
  move (refs-only re-root, legal anywhere in the chain) and
  `:where/:any/:all/:empty` set operators. Shipped 2026-07-17 → done.md
  ("Graph selector language — slice 1") for the full design record.

## Active work — graph selector language, remainder

Slice 1 ✅ (move + hop ranges + quantifiers, old pseudos removed with guided
errors, docs/budget updated, live-smoked on this repo). Remaining, in build
order:

- ◻ Group ranges on CONTAINMENT: `a *{m,n} b`, `a func{1,3} b` (b within m..n
  hops of a, through nodes matching the group). Unifies `>` = `*{0}` and
  space = `*{0,}` as non-primitives.
  - **next**: extend the combinator parse to accept a compound + `{m,n}` as a
    range-carrying group; route through the same walk collectMatches does.
- ◻ Retire `:depth(m,n)` once group ranges land (it dissolves into
  `*{m-1,n-1}`); guided error naming the spelling, same pattern as :has.
- **risks**: `:where` ≡ `:any` at tip granularity (documented at pseudoHolds)
  — only diverges if path-level filtering ships; unbounded `:parents{m,}`
  with m>1 collects nodes at their shortest hop only (documented at
  moveParents); referrers are name-keyed (lexical index), so same-named
  symbols share referrers — unchanged from :references.
- **blocking decisions**: none open. Edge model (refs-only re-root), operator
  name (`parents`, counter-documented), and quantifier scoping (relative +
  scope-binding) were resolved with the user 2026-07-17.
- **optional extensions** (icebox): path-level :where over retained edges;
  :parents adoption measurement.

## Non-goals (for now)

- Indexing the entire host filesystem; we only index inside the git root.
- Replacing any single child LSP. We multiplex, we don't reimplement.
- Sandboxing child LSPs. They run as the user.
- Windows support until someone asks.
