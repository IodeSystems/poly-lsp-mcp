# poly-lsp-mcp

A polyglot LSP + MCP server in Go. One binary, two surfaces:

- **LSP server** (`poly-lsp-mcp`) — editor integration. Multiplexes child language servers (gopls / tsserver / pylsp / …) and falls back to tree-sitter where no child exists.
- **MCP server** (`poly-lsp-mcp mcp`) — LLM agent integration. Same workspace machinery exposed as a six-tool surface.

The unique value-add over single-language LSPs is **cross-language linkage**: rename `UserID` in Go and the rename propagates through TypeScript, Python, YAML config values, proto messages, OpenAPI schemas, and prose — declared bindings, schema-anchored sites, and `@ref` comment markers all stitch the languages together.

Supports go / ts / tsx / js / py / sql / proto / graphql / yaml / json / markdown today.

## Install

```sh
git clone https://github.com/iodesystems/poly-lsp-mcp.git
cd poly-lsp-mcp
go install .
```

Binary lands at `$GOPATH/bin/poly-lsp-mcp`.

## LSP mode

Default invocation. Speaks LSP over stdio. Point your editor's language client at the binary; configure per-workspace via `poly-lsp-mcp.yaml`.

Example (Neovim, nvim-lspconfig):

```lua
require('lspconfig.configs').poly = {
  default_config = {
    cmd = { 'poly-lsp-mcp' },
    filetypes = { 'go', 'typescript', 'python', 'proto' },
    root_dir = require('lspconfig.util').root_pattern('poly-lsp-mcp.yaml', '.git'),
  },
}
require('lspconfig').poly.setup({})
```

What it owns over the child LSPs:

- `workspace/symbol`, `textDocument/references` — answered from the cross-language symbol index (lexical + declared + schema-anchored sites).
- `textDocument/rename` — synthesizes a `WorkspaceEdit` that touches every site for the name including string-literal YAML/JSON values and prose `@ref` markers.
- `textDocument/documentSymbol` — forward to child LSP first, fallback to the index.

Child LSP requests (`hover`, `definition`, completion, signature help, code actions, formatting, …) are routed through to the appropriate child. If the child crashes, multiplex restarts it with exponential backoff (default 1s → 30s, 5 attempts).

## MCP mode

```sh
poly-lsp-mcp mcp --root /path/to/workspace
```

Speaks MCP (newline-delimited JSON-RPC) over stdio. Six tools:

| Tool | Purpose |
|------|---------|
| `structure` | Directory walk OR tree-sitter named children of a file with decl + name ranges. Optional `grep` regex prunes to matching subtrees. |
| `node_references` | Workspace-wide references to the identifier at a range (lexical / declared / comment confidence). |
| `node_read` | Read whole file, line preview, or byte-precise range. |
| `node_edit` | Atomic write: whole-file create-or-overwrite, range replace, or unified-diff patch. |
| `node_delete` | Delete a range OR delete the whole file. |
| `node_refactor` | Composable cross-language refactor: `refactor:{rename?, params?, return?}`. Supports go / typescript / python. |

### Tool capability matrix

Most tools are polymorphic — pick the input shape that fits the task. The "node" prefix in the names is historical from the early AST-only days; today the tools work at three levels: raw file, line preview, and AST/range.

| Tool | `{file}` | `{file, line, offset?, limit?}` | `{file, range}` | `{file, diff}` | Other |
|---|---|---|---|---|---|
| `structure` | ✓ (listing, optional `grep`, `depth`) | — | — | — | — |
| `node_read` | ✓ whole file | ✓ line preview (default limit 50) | ✓ byte-precise text | — | — |
| `node_edit` | ✓ (with `newText`: create-or-overwrite, auto-mkdir parent) | — | ✓ (with `newText`: range replace) | ✓ unified-diff patch (strict context) | — |
| `node_delete` | ✓ delete file from disk | — | ✓ delete range | — | — |
| `node_references` | — | — | ✓ identifier range required | — | — |
| `node_refactor` | — | — | ✓ identifier range required | — | `refactor:{rename, params, return}` |

`node_read` / `node_edit` / `node_delete` work on **any file** regardless of language or whether a tree-sitter grammar exists — markdown, JSON, plain text, config files all fine. AST features only activate when you ask for them via `structure` or pass identifier ranges to the semantic tools.

### Resources

- `poly-lsp-mcp://workspace` — `{root, languages, names, declared}` summary.
- `poly-lsp-mcp://bindings` — every declared cross-language binding (Tier 2 + Tier 3).
- `poly-lsp-mcp://diagnostics` — workspace-wide diagnostic snapshot enriched the same way edit responses are.

Edit / refactor responses carry enriched diagnostics: `{text, context, enclosingNode, references}` per diagnostic. Sibling-file diagnostics roll up by default so compile cascades are visible in one response. Configurable caps per call (`diagnosticLimit`, `referenceLimit`, `contextLines`, `siblingDiagnostics`).

## Configuration

`poly-lsp-mcp.yaml` at the workspace root. All sections optional; defaults work for the supported languages.

```yaml
# Per-language config. Override the defaults if you need custom LSP
# args or extensions.
languages:
  - name: go
    extensions: [go]
    lsp: {cmd: gopls}
    treesitter: go
  - name: typescript
    extensions: [ts, tsx, js, jsx, mjs, cjs]
    lsp: {cmd: typescript-language-server, args: ["--stdio"]}
    treesitter: typescript

# Tier 2: hand-declared cross-language bindings. Three site forms:
# symbol (identifier match), jsonpath (YAML/JSON values), regex.
bindings:
  - name: UserType
    sites:
      - {file: main.go, symbol: UserID}
      - {file: client.ts, symbol: UserID}
      - {file: config.yaml, jsonpath: "$.users[*].id"}
      - {file: schema.sql, regex: ["\\buser_id\\b"]}

# Tier 3: schema-anchored. One entry per schema file auto-binds every
# named entity (proto messages / openapi components / jsonschema $defs).
schemas:
  - {file: api.proto, dialect: proto}
  - {file: openapi.yaml, dialect: openapi}

# Auto-detect schemas in the workspace at startup. Opt-in because the
# scan touches every YAML/JSON file looking for distinctive top-level
# keys.
auto_schemas: true
```

## Cross-language linkage — three tiers

| Tier | Setup | What it catches |
|------|-------|-----------------|
| 1. Lexical / tree-sitter | none | Identifier tokens. High recall, low precision. Drives `workspace/symbol` and references-as-preview. |
| 2. Declared bindings | `bindings:` section | Hand-declared cross-language identity including string-literal config values and aliases across naming conventions. |
| 3. Schema-anchored | `schemas:` section | Auto-derived bindings: proto messages/enums/services/rpcs, openapi components + operationIds, jsonschema $defs + title. One config line ≈ dozens of bindings. |

Plus a **universal comment scanner** that runs on every walked file:

- `@see X`, `{@link X}` → soft (comment-confidence) reference.
- `@ref X`, `x-ref: X` → hard (declared-confidence) reference.

Generators that emit cross-language artifacts (e.g., gat in gwag emits `@ref` back-references in proto / GraphQL SDL / OpenAPI x-ref) get cross-language linkage for free with no per-framework parsing dialect on our side.

## Diagnostics in edit responses (MCP)

After every `node_edit` / `node_delete` / `node_refactor`:

1. The edited file is sent through `didOpen`/`didChange`/`didSave` to the matching child LSP.
2. Per-URI `WaitAfter` blocks for up to 1500ms (default) for `publishDiagnostics`.
3. Sibling files in the same package that gain new diagnostics are rolled in by default.
4. Each diagnostic carries `text` (range source), `context` (configurable lines around it), `enclosingNode` (containing tree-sitter declaration with name + decl ranges), and `references` (node_references-shape hits when the range is an identifier).

The diagnostic store also feeds `poly-lsp-mcp://diagnostics` for workspace-wide health checks without an edit. A proactive workspace open at MCP initialize seeds the store so the resource is useful before the agent makes its first change.

## `node_refactor` — composable signature ops

Supports Go, TypeScript (.ts/.tsx/.js), and Python.

```jsonc
{
  "file": "lib.ts",
  "startLine": 1, "startCol": 17, "endLine": 1, "endCol": 22,
  "refactor": {
    "rename": "hello",                                       // workspace-wide
    "params": [                                              // rebuild signature
      {"name": "name", "type": "string"},
      {"name": "age",  "type": "number"}
    ],
    "return": "string"                                       // replace or insert
  }
}
```

- **rename**: workspace-wide, with declared-binding + aliasing safety. Touches comments and prose only when `includeComments: true`.
- **params**: rebuilds the function declaration. When arity changes, callers across the workspace are rewritten best-effort — args truncated on shrink, padded with language-appropriate zero values on growth (`""`, `0`, `false`, `nil` / `null` / `None`, `[]` / `{}`, …). Spread / splat callers are reported as `skipped` so you decide.
- **return**: replaces the existing return type or inserts one into a previously-void declaration.

All three combine in one call. Diagnostic round-trip on every touched file.

## Stacked-branch parse cache

Phase-3 win for stacked-branch workflows:

- Parse results are content-addressed by `(language, sha256(content))` — switching back to a branch you were just on hits the cache for free.
- On MCP initialize, the upstream chain (`feature/c` → `feature/b` → `feature/a` → `main`) is walked asynchronously and every ancestor's files are pre-parsed so a switch *forward* to those branches is also free.
- LRU-bounded (5000 entries by default), persisted to `<root>/.poly-lsp-mcp/cache.gob` across MCP sessions.

Disable per-session via `Server.SetGitPrewarm(false)` if the up-front cost outweighs the later switch-time saving (very large stacks, ephemeral CI workloads).

## Testing

Convenience targets in the `Makefile`:

```sh
make test          # short suite — fast, skips live-LSP / live-gat e2e
make test-all      # full suite (needs gopls + git on PATH)
make test-race     # short suite + race detector (per-PR gate)
make test-race-all # full suite + race detector (pre-release)
make check         # vet + test + test-race
make smoke-editor  # real-binary LSP conformance smoke
make smoke-llm     # live LLM end-to-end smoke
```

The race detector is the standing concurrency gate — exercises the
`DiagnosticStore` ↔ child-LSP-readloop interactions, parse cache
under concurrent reads/writes, and the manager spawn/restart
goroutines. Clean under three consecutive `make test-race-all`
runs as of the most recent commit on `main`.

## Library usage

The same packages the standalone binary uses are importable. See
`examples/embed/main.go` for a small program that builds an MCP
server in its own process.

```go
import (
    "github.com/iodesystems/poly-lsp-mcp/config"
    "github.com/iodesystems/poly-lsp-mcp/mcp"
    "github.com/iodesystems/poly-lsp-mcp/multiplex"
)

cfg, _, _ := config.LoadOrDefault("poly-lsp-mcp.yaml")
reg, _ := cfg.Build()
srv := mcp.New(reg, "/path/to/workspace", cfg.Bindings, cfg.Schemas)
srv.SetManager(multiplex.NewManager(reg))
srv.Serve(os.Stdin, os.Stdout)
```

Public packages (stable):

| Package | What's in it |
|---|---|
| `config` | Language registry, YAML loader, `auto_schemas` detect. |
| `mcp` | MCP server, tools, resources. |
| `server` | LSP server (multiplex + index fallback). |
| `multiplex` | Child LSP supervisor + `DiagnosticStore`. |
| `symbols` | Cross-language index, parse cache, tree-sitter extractors, refactor primitives (`FindFunctionSignature`, `RewriteSignature`, `FindCallSites`, `PrewarmFromBranch`). |

Internal-only (subject to change without notice):

- `internal/bindings` — declared-binding resolver + schema dialects (used by `server` and `mcp` directly).
- `internal/git` — `git` binary wrapper (used by `symbols.PrewarmFromBranch` and `mcp`'s prewarm).
- `internal/jsonrpc` — JSON-RPC 2.0 framing.

## Layout

```
main.go                      entry, subcommand dispatch
config/                      language registry, YAML loader, schema
                             auto-detect (PUBLIC)
mcp/                         MCP server + tools + resources (PUBLIC)
server/                      LSP server (PUBLIC)
multiplex/                   child LSP supervisor + diagnostic store
                             (PUBLIC)
symbols/                     index, tree-sitter extractors, lexical
                             fallback, parse cache, comment scanner,
                             refactor primitives, branch prewarm
                             (PUBLIC)
internal/jsonrpc/            JSON-RPC framing
internal/bindings/           declared bindings (Tier 2) + schema
                             dialects (Tier 3)
internal/git/                git binary wrapper
testdata/fixtures/polyglot/  multi-language fixture
testdata/fixtures/gat-greeter/
                             live gat → poly-lsp-mcp @ref fixture +
                             cross-language diagnostic Go server
examples/embed/              library-mode example
plan/plan.md                 phased roadmap (all phases shipped)
```

## Status

Phase 0 through Phase 5 plus the stacked-branch tail of Phase 3 are all shipped. The roadmap is effectively complete; further work is scope expansion (new refactor kinds, more language coverage) and ergonomics.

See `plan/plan.md` for the full feature history with rationale for each design decision.
