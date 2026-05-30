# scripts/smoke

Manual smoke harnesses that complement the in-process Go tests by
driving the real binary as a subprocess.

## editor_smoke.py

Drives `tslsmcp` over stdio with the request shapes a typical LSP
client (nvim-lspconfig, vs-code, helix) sends, and asserts every
response against the LSP spec's required-field contracts.

```bash
python3 scripts/smoke/editor_smoke.py
```

This builds the binary at `/tmp/tslsmcp_smoke` if needed and runs the
smoke against `testdata/fixtures/polyglot`. Override with `--binary`
or `--workspace`. Exit 0 = every check passed.

What it catches that the in-process Go tests don't:

- Subprocess + stdio framing with real `Content-Length` headers and
  real EOFs.
- Tolerance for the kitchen-sink `ClientCapabilities` payload real
  editors send (many optional fields).
- Per-method response-shape contracts: `SymbolKind` in the valid
  range, `Range.end >= start`, `Location.uri` is a `file://` URI,
  `WorkspaceEdit.changes` keyed by URI with non-empty edits each.
- `textDocument/hover` returns `null` (not an error) when no child
  LSP is configured — a `-32601 method not found` here would break
  hover popups in every editor.

Worth running once before any release tagged with editor-facing
behavior changes.
