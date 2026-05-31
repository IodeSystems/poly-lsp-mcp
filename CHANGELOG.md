# Changelog

All notable user-facing changes. Format loosely follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

The project ships from `main` without semver tags today. This section captures the cumulative state of the LSP + MCP surface — what an editor / agent gets from a fresh build.

### Build

- Go 1.26 module at `github.com/iodesystems/poly-lsp-mcp`.
- Single binary; default invocation is the LSP server, `poly-lsp-mcp mcp` is the MCP server.

### Breaking changes since the early scaffold

- **Library mode shipped**: `config`, `mcp`, `server`, `multiplex`, `symbols` promoted out of `internal/` to top-level packages. External code can now import them as `github.com/iodesystems/poly-lsp-mcp/{config,mcp,server,multiplex,symbols}`. `bindings`, `git`, and `jsonrpc` stay internal (implementation detail). See `examples/embed/main.go` for the embedding pattern.
- Module + binary renamed `tslsmcp` → `poly-lsp-mcp` (commit `84c2cb5`). Adopters update:
  - Go import path: `github.com/iodesystems/tslsmcp` → `github.com/iodesystems/poly-lsp-mcp`.
  - Binary name.
  - Config file name: `tslsmcp.yaml` → `poly-lsp-mcp.yaml`.
  - Cache directory: `<root>/.tslsmcp/cache.gob` → `<root>/.poly-lsp-mcp/cache.gob`.
  - Resource URIs: `tslsmcp://workspace`, `tslsmcp://bindings`, `tslsmcp://diagnostics` → `poly-lsp-mcp://…`.
  - serverInfo.name on `initialize`: `tslsmcp` → `poly-lsp-mcp`.
  - OpenAPI extension key accepted by the `@ref` scanner: `x-tslsmcp-source` → `x-poly-lsp-mcp-source` (`x-ref` and `x-source` unchanged).
- MCP tool surface trimmed from 10 → 6 (commit `386d9df`). Removed: `find_symbol`, `find_references` (name-based), `document_symbols` (use `structure` on a file), `list_bindings` (use the bindings resource), `rename` (preview), `apply_rename` (use `node_refactor`), `refresh`, `read_range` (use `node_read`), `replace_range` (use `node_edit`). Kept: `structure`, `node_references`, `node_read`, `node_edit`, `node_delete`, `node_refactor`.
- `node_refactor` accepts a nested `refactor:{rename?, params?, return?}` shape (commit `aa6032f`). The legacy `kind="rename", newName=X` shape continues to work for backward compat.

### LSP surface

- Multiplex of child language servers — eager spawn per observed language; `RouteByURI` for `textDocument/*` requests; restart-on-crash with exponential backoff.
- Cross-language `workspace/symbol`, `textDocument/references` answered from the symbol index (lexical / declared / schema-anchored / comment confidence).
- `textDocument/rename` synthesizes a `WorkspaceEdit` that touches every declared site in every language including YAML/JSON string values and `@ref`-marked prose.
- `textDocument/documentSymbol` — forward-first with index fallback.
- Workspace-folder + `workspace/didChangeConfiguration` fanout to every child.
- 18-test LSP conformance pack; real-binary editor-shaped smoke (`scripts/smoke/editor_smoke.py`).

### MCP surface

- Six tools: `structure`, `node_references`, `node_read`, `node_edit`, `node_delete`, `node_refactor`.
- **Phase 6 tool ergonomics** — input shapes expanded so the agent can ship with just MCP + `shell` (no `read_file`/`write_file`/`grep`/`ls` shims):
  - `structure` accepts `grep` (regex) — matches file basenames, directory names, code identifiers; subtrees without a match get pruned. Auto-bumps depth to 32 when `grep` is set.
  - `node_read` accepts `{file}` (whole file), `{file, line, offset?, limit?}` (line preview, defaults `offset=0` / `limit=50`), or the existing `{file, startLine/Col, endLine/Col}` byte-precise range.
  - `node_edit` accepts `{file, newText}` (create-or-overwrite, auto-mkdir parent), `{file, diff}` (unified-diff patch with strict context matching), or the existing `{file, range, newText}` form.
  - `node_delete` accepts `{file}` (whole-file delete — operator-grade destructive, errors clearly on missing/directory paths) or the existing range form.
- Three resources: `poly-lsp-mcp://workspace`, `poly-lsp-mcp://bindings`, `poly-lsp-mcp://diagnostics`.
- `node_refactor` supports Go / TypeScript / Python with composable ops:
  - `rename`: workspace-wide identifier rewrite (declared-binding + aliasing safety).
  - `params`: rebuild function signature + best-effort call-site arg-list rewriting (truncate on shrink, pad with language-appropriate zero values on growth, skip spread/splat).
  - `return`: replace existing return type OR insert into a previously-void signature.
- Edit / refactor responses carry enriched diagnostics: `text` (range source, 256-char ellipsis), `context` (N lines around the diagnostic), `enclosingNode` (containing tree-sitter declaration), `references` (node_references-shape hits when the range is an identifier).
- Sibling-file diagnostic rollup: gopls publishes for whole packages; the response includes URIs whose generation advanced during the wait window. Opt-out with `siblingDiagnostics: false`.
- Configurable caps per call: `diagnosticLimit` (default 25), `referenceLimit` (default 15), `contextLines` (default 3).
- Proactive workspace open on `initialize` — sends `didOpen` to every indexed file's child LSP so the diagnostics resource is useful before any edit happens. `SetProactiveOpen(false)` opts out.

### Cross-language indexing

- **Tier 1 (lexical / tree-sitter)**: tree-sitter identifier extractors for Go / TypeScript / TSX / JSX / Python / SQL / Protobuf; lexical extractor for YAML / JSON / Markdown / GraphQL.
- **Tier 2 (declared bindings)**: `bindings:` section in `poly-lsp-mcp.yaml`. Three site forms — `symbol`, `jsonpath` (YAML/JSON value addressing), `regex` (multi-pattern with aliasing safety).
- **Tier 3 (schema-anchored)**: `schemas:` section. Auto-binds every named entity in proto / OpenAPI / JSONSchema specs. `auto_schemas: true` opt-in workspace detection.
- **Universal `@ref` scanner**: language-agnostic regex pass picks up `@see`, `{@link}`, `@ref`, and `x-ref:` markers wherever they appear. Generator-emitted artifacts (gat in gwag) get cross-language linkage for free.

### Parse cache + stacked branches (Phase 3)

- Content-addressed cache keyed on `(language, sha256(content))`. Files with identical content share one parse across branches, worktrees, and renames.
- LRU eviction (default 5000 entries). Disk persistence via `<root>/.poly-lsp-mcp/cache.gob` between MCP sessions.
- Ancestor-branch prewarm on MCP initialize: walks the upstream chain (capped at 16 ancestors), parses every file in each ancestor's tree, seeds the cache. Switching forward to those branches later is free. `SetGitPrewarm(false)` to disable.

### Configuration

- `poly-lsp-mcp.yaml` schema: `languages`, `bindings`, `schemas`, `auto_schemas`. All sections optional; defaults cover the supported languages.
- Defaults merge: partial configs (e.g., only `schemas:` declared) get the default `languages` registry folded in so the index doesn't silently break.

### Testing fixtures

- `testdata/fixtures/polyglot/` — multi-language fixture exercising the cross-language index (`UserID` lives in go / ts / py / yaml / json / md / sql / proto / openapi / jsonschema).
- `testdata/fixtures/gat-greeter/` — separate Go module pulling local gwag; `cmd/dump` emits gat-rendered GraphQL SDL and OpenAPI JSON. Used by `TestGatGreeterFixture` to prove the live gat → poly-lsp-mcp `@ref` linkage.
- `testdata/fixtures/gat-greeter/server/` — hand-written Go stubs shaped like `protoc-gen-go` output, `@ref`-linked to the proto. Used by `TestCrossLanguageDiagnosticOnGeneratedStub` and `TestSiblingDiagnosticsRollup` to exercise the diagnostic-enrichment path against real gopls output.

### Known follow-ups (not blocking 1.0)

- `workspace/applyEdit` — currently we return `WorkspaceEdit` from rename and let the editor apply. Server-driven apply is an LSP option we haven't needed.
- Legacy `kind="rename", newName=X` shape on `node_refactor` is deprecated in favor of `refactor:{rename: X}`; both still accepted.
- Refactor kinds beyond rename + signature (`extract_function`, `inline`, `change_visibility`).
- Keyed param reorder/rename ops (preserve call-site args when only shuffling).
- Signature ops for languages beyond go / ts / py.

See `plan/plan.md` for the full feature history with design rationale.
