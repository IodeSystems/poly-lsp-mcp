#!/usr/bin/env python3
"""Editor-side LSP conformance smoke.

Drives the poly-lsp-mcp binary as a real subprocess with the request shapes
a typical LSP client (nvim-lspconfig, vs-code, helix) sends, then
asserts every response against the LSP spec's required-field
contracts. Catches integration gaps the in-process conformance pack
can miss:

  - Subprocess + stdio framing (Content-Length headers, real EOFs).
  - Client capability negotiation (the spec leaves many fields
    optional; clients send what they support and we must not choke).
  - Response-shape contracts (SymbolKind valid range, Location.range
    well-formed, WorkspaceEdit.changes object-keyed by URI, etc.).

Exit code: 0 if every check passes, 1 otherwise. The pass/fail
output is one line per check so it's easy to scan and easy to wire
into CI.

Usage:
    python3 scripts/smoke/editor_smoke.py
        builds the binary at /tmp/poly_lsp_mcp_smoke if needed and runs
        the smoke against testdata/fixtures/polyglot.

    python3 scripts/smoke/editor_smoke.py --binary /path/to/poly-lsp-mcp
        skips the build, uses an existing binary.

    python3 scripts/smoke/editor_smoke.py --workspace /path/to/repo
        runs against a custom workspace root.
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
from dataclasses import dataclass
from typing import Any, Callable


# ----- spec contracts -------------------------------------------------------

# SymbolKind enum from LSP 3.17, section 5.4. Valid values are 1..26.
VALID_SYMBOL_KIND = set(range(1, 27))


def assert_position(p: Any, where: str) -> list[str]:
    """A Position must have integer `line` and `character`, both >= 0."""
    errs: list[str] = []
    if not isinstance(p, dict):
        return [f"{where}: not a dict, got {type(p).__name__}"]
    for field in ("line", "character"):
        v = p.get(field)
        if not isinstance(v, int) or v < 0:
            errs.append(f"{where}.{field}: expected non-negative int, got {v!r}")
    return errs


def assert_range(r: Any, where: str) -> list[str]:
    """A Range has Position start, Position end, end >= start (line, col)."""
    errs: list[str] = []
    if not isinstance(r, dict):
        return [f"{where}: not a dict, got {type(r).__name__}"]
    errs += assert_position(r.get("start"), f"{where}.start")
    errs += assert_position(r.get("end"), f"{where}.end")
    if errs:
        return errs
    s = r["start"]
    e = r["end"]
    if (e["line"], e["character"]) < (s["line"], s["character"]):
        errs.append(f"{where}: end before start ({e} < {s})")
    return errs


def assert_location(loc: Any, where: str) -> list[str]:
    """A Location has a `uri` string and a `range` Range."""
    errs: list[str] = []
    if not isinstance(loc, dict):
        return [f"{where}: not a dict"]
    uri = loc.get("uri")
    if not isinstance(uri, str) or not uri:
        errs.append(f"{where}.uri: empty or non-string")
    errs += assert_range(loc.get("range"), f"{where}.range")
    return errs


def assert_symbol_information(s: Any, where: str) -> list[str]:
    """SymbolInformation: name (str), kind (1..26), location (Location)."""
    errs: list[str] = []
    if not isinstance(s, dict):
        return [f"{where}: not a dict"]
    name = s.get("name")
    if not isinstance(name, str) or not name:
        errs.append(f"{where}.name: empty or non-string")
    kind = s.get("kind")
    if kind not in VALID_SYMBOL_KIND:
        errs.append(f"{where}.kind: {kind!r} not in valid SymbolKind set (1..26)")
    errs += assert_location(s.get("location"), f"{where}.location")
    return errs


# ----- jsonrpc framing ------------------------------------------------------


class LSPClient:
    """Tiny Content-Length-framed JSON-RPC client around a subprocess."""

    def __init__(self, proc: subprocess.Popen) -> None:
        self.proc = proc
        self._next_id = 0

    def _send(self, msg: dict[str, Any]) -> None:
        body = json.dumps(msg).encode()
        header = f"Content-Length: {len(body)}\r\n\r\n".encode()
        assert self.proc.stdin is not None
        self.proc.stdin.write(header + body)
        self.proc.stdin.flush()

    def _recv(self) -> dict[str, Any]:
        assert self.proc.stdout is not None
        headers = b""
        while b"\r\n\r\n" not in headers:
            ch = self.proc.stdout.read(1)
            if not ch:
                raise RuntimeError("EOF on server stdout")
            headers += ch
        n = int(headers.decode().split("Content-Length:")[1].split("\r\n")[0].strip())
        body = self.proc.stdout.read(n)
        return json.loads(body)

    def request(self, method: str, params: Any) -> dict[str, Any]:
        self._next_id += 1
        self._send({"jsonrpc": "2.0", "id": self._next_id, "method": method, "params": params})
        # Loop in case the server sends server-initiated requests we ignore.
        while True:
            resp = self._recv()
            if resp.get("id") == self._next_id:
                return resp

    def notify(self, method: str, params: Any) -> None:
        self._send({"jsonrpc": "2.0", "method": method, "params": params})


# ----- check harness --------------------------------------------------------


@dataclass
class Check:
    name: str
    run: Callable[[LSPClient, str], list[str]]


def realistic_initialize_params(workspace_root: str) -> dict[str, Any]:
    """ClientCapabilities mimicking what nvim 0.10 / vs-code 1.x send.

    The point is to be more permissive than the spec — clients always
    send extra fields, and a server that chokes on unknown fields fails
    a real-editor smoke.
    """
    uri = "file://" + workspace_root
    return {
        "processId": os.getpid(),
        "clientInfo": {"name": "poly-lsp-mcp-smoke", "version": "0.1"},
        "locale": "en-US",
        "rootUri": uri,
        "rootPath": workspace_root,  # legacy field — must coexist with rootUri
        "workspaceFolders": [{"uri": uri, "name": "smoke"}],
        "capabilities": {
            "workspace": {
                "applyEdit": True,
                "workspaceEdit": {"documentChanges": True, "resourceOperations": ["create", "rename", "delete"]},
                "didChangeConfiguration": {"dynamicRegistration": True},
                "didChangeWatchedFiles": {"dynamicRegistration": True},
                "symbol": {"dynamicRegistration": True, "symbolKind": {"valueSet": list(VALID_SYMBOL_KIND)}},
                "executeCommand": {"dynamicRegistration": True},
                "workspaceFolders": True,
                "configuration": True,
            },
            "textDocument": {
                "synchronization": {
                    "dynamicRegistration": True,
                    "willSave": True,
                    "willSaveWaitUntil": True,
                    "didSave": True,
                },
                "completion": {
                    "dynamicRegistration": True,
                    "contextSupport": True,
                    "completionItem": {"snippetSupport": True, "commitCharactersSupport": True},
                },
                "hover": {"dynamicRegistration": True, "contentFormat": ["markdown", "plaintext"]},
                "references": {"dynamicRegistration": True},
                "documentSymbol": {
                    "dynamicRegistration": True,
                    "symbolKind": {"valueSet": list(VALID_SYMBOL_KIND)},
                    "hierarchicalDocumentSymbolSupport": True,
                },
                "rename": {"dynamicRegistration": True, "prepareSupport": True},
                "publishDiagnostics": {"relatedInformation": True, "tagSupport": {"valueSet": [1, 2]}},
            },
            "general": {
                "regularExpressions": {"engine": "ECMAScript", "version": "ES2020"},
                "markdown": {"parser": "marked", "version": "1.1.0"},
            },
            "experimental": {"poly-lsp-mcp": True},
        },
        "initializationOptions": {},
        "trace": "off",
    }


def check_initialize_response(client: LSPClient, root: str) -> list[str]:
    resp = client.request("initialize", realistic_initialize_params(root))
    if "error" in resp:
        return [f"initialize error: {resp['error']}"]
    result = resp.get("result") or {}
    errs: list[str] = []
    caps = result.get("capabilities")
    if not isinstance(caps, dict):
        errs.append("missing capabilities object")
        return errs
    # Server may advertise any subset; we want the ones poly-lsp-mcp owns.
    for must_have in (
        "workspaceSymbolProvider",
        "referencesProvider",
        "documentSymbolProvider",
        "renameProvider",
    ):
        if must_have not in caps:
            errs.append(f"capabilities.{must_have} not advertised")
    info = result.get("serverInfo") or {}
    if info.get("name") != "poly-lsp-mcp":
        errs.append(f"serverInfo.name = {info.get('name')!r}, want 'poly-lsp-mcp'")
    client.notify("initialized", {})
    return errs


def check_workspace_symbol(client: LSPClient, root: str) -> list[str]:
    resp = client.request("workspace/symbol", {"query": "UserID"})
    if "error" in resp:
        return [f"workspace/symbol error: {resp['error']}"]
    result = resp.get("result")
    if not isinstance(result, list):
        return [f"workspace/symbol result must be array or null, got {type(result).__name__}"]
    if not result:
        return ["workspace/symbol returned empty for UserID (polyglot fixture should have hits)"]
    errs: list[str] = []
    for i, sym in enumerate(result[:5]):  # cap to keep output readable
        errs += assert_symbol_information(sym, f"result[{i}]")
    return errs


def check_document_symbol(client: LSPClient, root: str) -> list[str]:
    uri = f"file://{root}/main.go"
    resp = client.request("textDocument/documentSymbol", {"textDocument": {"uri": uri}})
    if "error" in resp:
        return [f"textDocument/documentSymbol error: {resp['error']}"]
    result = resp.get("result")
    if result is None:
        return []  # null is spec-legal
    if not isinstance(result, list):
        return [f"documentSymbol result must be array or null, got {type(result).__name__}"]
    errs: list[str] = []
    for i, sym in enumerate(result[:5]):
        errs += assert_symbol_information(sym, f"result[{i}]")
    return errs


def check_references(client: LSPClient, root: str) -> list[str]:
    uri = f"file://{root}/main.go"
    # Polyglot's main.go line 6 (0-based 5), char 6 is on "UserID".
    resp = client.request(
        "textDocument/references",
        {
            "textDocument": {"uri": uri},
            "position": {"line": 5, "character": 6},
            "context": {"includeDeclaration": True},
        },
    )
    if "error" in resp:
        return [f"textDocument/references error: {resp['error']}"]
    result = resp.get("result")
    if not isinstance(result, list):
        return [f"references result must be array, got {type(result).__name__}"]
    if not result:
        return ["references returned empty at UserID cursor"]
    errs: list[str] = []
    for i, loc in enumerate(result[:5]):
        errs += assert_location(loc, f"result[{i}]")
    return errs


def check_rename(client: LSPClient, root: str) -> list[str]:
    uri = f"file://{root}/main.go"
    resp = client.request(
        "textDocument/rename",
        {
            "textDocument": {"uri": uri},
            "position": {"line": 5, "character": 6},
            "newName": "PersonID",
        },
    )
    if "error" in resp:
        return [f"textDocument/rename error: {resp['error']}"]
    result = resp.get("result")
    if result is None:
        return ["rename result is null (expected WorkspaceEdit at the UserID cursor)"]
    if not isinstance(result, dict):
        return [f"rename result must be object, got {type(result).__name__}"]
    changes = result.get("changes")
    if not isinstance(changes, dict) or not changes:
        return [f"WorkspaceEdit.changes must be a non-empty object, got {type(changes).__name__}"]
    errs: list[str] = []
    for file_uri, edits in changes.items():
        if not isinstance(file_uri, str) or not file_uri.startswith("file://"):
            errs.append(f"changes key {file_uri!r}: expected file:// URI")
        if not isinstance(edits, list) or not edits:
            errs.append(f"changes[{file_uri}]: must be non-empty list")
            continue
        for i, e in enumerate(edits[:3]):
            errs += assert_range(e.get("range"), f"changes[{file_uri}][{i}].range")
            if not isinstance(e.get("newText"), str):
                errs.append(f"changes[{file_uri}][{i}].newText: expected string")
    return errs


def check_unknown_textdocument_method_returns_response(client: LSPClient, root: str) -> list[str]:
    # textDocument/hover isn't owned by us; with no child LSP it should
    # return null result (NOT a JSON-RPC error code). A real client
    # treats null as "no hover available" and moves on; a method-not-
    # found error code would break hover popups everywhere.
    uri = f"file://{root}/main.go"
    resp = client.request(
        "textDocument/hover",
        {"textDocument": {"uri": uri}, "position": {"line": 5, "character": 6}},
    )
    if "error" in resp:
        return [
            f"textDocument/hover returned error response: {resp['error']} "
            "(real editors expect null when the server can't provide hover)"
        ]
    if "result" not in resp:
        return ["textDocument/hover missing both result and error"]
    return []


def check_workspace_notifications_dropped_silently(client: LSPClient, root: str) -> list[str]:
    # didChangeConfiguration is a notification — no response expected.
    # Send it and follow up with a request; if the dispatch path is
    # broken, the request hangs or errors.
    client.notify("workspace/didChangeConfiguration", {"settings": {"trace": "verbose"}})
    resp = client.request("workspace/symbol", {"query": ""})
    if "error" in resp:
        return [f"server stopped responding after didChangeConfiguration: {resp['error']}"]
    return []


# ----- main ----------------------------------------------------------------


def build_binary(out: str) -> str:
    """Build poly-lsp-mcp into `out` from the repo root."""
    repo_root = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
    print(f"[build] {out} from {repo_root}", file=sys.stderr)
    res = subprocess.run(
        ["go", "build", "-o", out, "."],
        cwd=repo_root,
        capture_output=True,
        text=True,
    )
    if res.returncode != 0:
        print(res.stderr, file=sys.stderr)
        raise SystemExit(f"build failed: exit {res.returncode}")
    return out


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", help="poly-lsp-mcp binary path (default: build)")
    parser.add_argument(
        "--workspace",
        help="workspace root to test against (default: testdata/fixtures/polyglot)",
    )
    args = parser.parse_args()

    repo_root = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
    binary = args.binary or build_binary("/tmp/poly_lsp_mcp_smoke")
    workspace = args.workspace or os.path.join(repo_root, "testdata", "fixtures", "polyglot")

    print(f"[smoke] binary={binary}")
    print(f"[smoke] workspace={workspace}")

    proc = subprocess.Popen(
        [binary],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL,
    )
    client = LSPClient(proc)

    checks: list[Check] = [
        Check("initialize advertises required capabilities", check_initialize_response),
        Check("workspace/symbol returns valid SymbolInformation", check_workspace_symbol),
        Check("textDocument/documentSymbol returns valid SymbolInformation", check_document_symbol),
        Check("textDocument/references returns valid Locations", check_references),
        Check("textDocument/rename returns valid WorkspaceEdit", check_rename),
        Check("textDocument/hover returns null (no error)", check_unknown_textdocument_method_returns_response),
        Check("workspace notifications don't break dispatch", check_workspace_notifications_dropped_silently),
    ]

    failures: list[tuple[str, list[str]]] = []
    for c in checks:
        try:
            errs = c.run(client, workspace)
        except Exception as e:  # pylint: disable=broad-except
            errs = [f"exception: {e!r}"]
        status = "PASS" if not errs else "FAIL"
        print(f"  {status}  {c.name}")
        if errs:
            for e in errs:
                print(f"        - {e}")
            failures.append((c.name, errs))

    # Clean shutdown so the server's persistence path runs.
    try:
        client.request("shutdown", None)
        client.notify("exit", None)
    except Exception:  # pylint: disable=broad-except
        pass
    proc.wait(timeout=5)

    print()
    if failures:
        print(f"FAILED: {len(failures)} / {len(checks)} checks", file=sys.stderr)
        return 1
    print(f"PASSED: {len(checks)} / {len(checks)} checks")
    return 0


if __name__ == "__main__":
    sys.exit(main())
