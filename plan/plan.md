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
- Selector language: CSS containment + the graph half. Two moves —
  `:parents` (incoming) / `:references` (outgoing), bare or `(sel)`, `{m,n}`
  hops, `{1,}` transitive — legal anywhere in the chain. Bare
  `:any/:all/:empty` close a move postfix and collapse to the subject
  (`func:parents:empty` = dead code); parenthesized forms test relative
  selectors whose start is assumed `&` (CSS nesting rule; `:root`
  re-anchors). `{m,n}` on a compound is the canonical depth range
  (`:depth` = alias). Shipped 2026-07-17 in two slices → done.md ("Graph
  selector language") for the design record and decisions.

## Active work

None in flight. The graph selector language (both slices) ✅ → done.md.
Next candidates are opt-in, in icebox.md — the most valuable is adoption
measurement: does anything USE :parents/:references unprompted?

Known caveats (documented in code): `:where(sel)` ≡ `:any(sel)` at tip
granularity (pseudoHolds); unbounded `{m,}` with m>1 collects nodes at their
shortest hop (moveEdges); reference edges are name-keyed via the lexical
index, so same-named symbols share edges.

## Non-goals (for now)

- Indexing the entire host filesystem; we only index inside the git root.
- Replacing any single child LSP. We multiplex, we don't reimplement.
- Sandboxing child LSPs. They run as the user.
- Windows support until someone asks.
