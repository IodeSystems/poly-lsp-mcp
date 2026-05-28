# tslsmcp — roadmap

Fused multi-language LSP server. Tree-sitter for cross-language edits and
references, child LSPs for in-language semantics, falls back to tree-sitter
edits where no LSP exists. MCP layer comes later via an external bridge or a
thin wrapper of our own.

## Architecture (target)

```
        ┌───────── editor / MCP bridge ─────────┐
        │                                       │
        ▼                                       │
  ┌─────────────┐                               │
  │ tslsmcp LSP │  ← single LSP frontend        │
  └──────┬──────┘                               │
         │ route by ext / treesitter language   │
   ┌─────┴──────┬──────────────┬────────────┐   │
   ▼            ▼              ▼            ▼   │
 gopls       tsserver        pylsp       tree-sitter
                                       (fallback edits,
                                        cross-lang refs)
```

State is keyed by **stacked branch**: child branch indexes inherit from parent
branch indexes by content hash, so switching `feature/c → feature/b` is cheap.

## Phase 0 — scaffold (DONE)

- [x] Go module, stdlib JSON-RPC framing, stdio loop
- [x] `initialize` / `initialized` / `shutdown` / `exit` handshake
- [x] Empty capabilities (placeholder)

## Phase 1 — multiplex + fallback edit (v0.1)

Goal: editor connects, opens any file, gets either real LSP semantics or
tree-sitter–derived hover/refs/edits. **No cross-language stitching yet.**

- [ ] `internal/config`: language registry. `ext → {lsp_cmd, treesitter_lang}`.
      Source from a single config file (default `tslsmcp.yaml` at repo root)
      and bake a sensible default for go/ts/py.
- [ ] `internal/multiplex`:
  - [ ] Child LSP process supervisor (spawn, restart-on-crash, drain, kill).
  - [ ] Per-child JSON-RPC client reusing `internal/server` framing.
  - [ ] Forward `textDocument/*` to the child owning that URI.
  - [ ] Capabilities merge: union child `ServerCapabilities`, then mask with
        anything we override (e.g. we own `textDocument/rename` once
        cross-language rename lands).
  - [ ] Workspace folder broadcast + `didChangeConfiguration` fanout.
- [ ] `internal/treesitter`:
  - [ ] Parser registry (smacker/go-tree-sitter or
        tree-sitter/go-tree-sitter — decide in the multiplex PR).
  - [ ] Per-document parse cache keyed by content hash.
  - [ ] Symbol/identifier queries per language (`.scm` files).
- [ ] Fallback path: when the child LSP returns no result for `definition`,
      `references`, `documentSymbol`, or returns an empty edit for `rename`,
      synthesize a result from the tree-sitter index.
- [ ] LSP conformance smoke test: open vs-code or `nvim-lspconfig`, see that
      definition/references work across go + ts in one session.

**Open decisions (resolve in Phase 1):**
- LSP framework: stdlib (current) vs `tliron/glsp` vs `go.lsp.dev/protocol`.
  Current stdlib path scales until we want strongly-typed params.
- Tree-sitter binding: `smacker/go-tree-sitter` (mature, CGO) vs
  `tree-sitter/go-tree-sitter` (official, newer). Both CGO.

## Phase 2 — stacked-branch index

Goal: switching branches in a stack doesn't re-parse the world.

- [ ] `internal/git`:
  - [ ] Detect stack via `git config branch.<name>.parent` or
        a `.tslsmcp/stack` file (TBD; survey what gt/branchless write).
  - [ ] Compute the parent → child diff once per branch switch.
- [ ] Index store keyed by `(file_content_hash, language)`.
- [ ] On branch switch: invalidate only files whose content hash differs
      from the parent branch's snapshot.
- [ ] On working-tree edits: overlay over the branch's frozen index.

## Phase 3 — cross-language references / safe rename

Goal: rename a TS export and have Python imports, Go cgo headers,
SQL templates, and `*.md` doc links updated coherently.

- [ ] Symbol normalization: `(language, kind, qualified_name)` → canonical id.
- [ ] String-literal cross-language scanning (the hard part: TS export name
      appears as a bare string in a Python import, in YAML, in templates).
- [ ] `workspace/applyEdit` synthesizing edits from both child LSPs *and*
      tree-sitter, atomically previewed.

## Phase 4 — MCP

Two options, decided once Phase 1 is real:

- **Option A:** ship a tiny `cmd/tslsmcp-mcp` that speaks MCP and adapts to
  our LSP — same binary, two subcommands.
- **Option B:** point users at `isaacphi/mcp-language-server` configured to
  spawn `tslsmcp` as the LSP. Zero work for us, but no
  tslsmcp-specific MCP tools (e.g. `tslsmcp.cross_lang_refs`).

Probable answer: B for v0.1 ship, A once we have tslsmcp-specific tools
worth exposing.

## Non-goals (for now)

- Indexing the entire host filesystem; we only index inside the git root.
- Replacing any single child LSP. We multiplex, we don't reimplement.
- Sandboxing child LSPs. They run as the user.
- Windows support until someone asks.
