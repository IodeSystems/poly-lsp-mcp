#!/usr/bin/env python3
"""End-to-end LLM smoke against the poly-lsp-mcp MCP server.

Spawns poly-lsp-mcp as an MCP subprocess pointed at a TEMP COPY of the
polyglot fixture, exposes the MCP tools to an OpenAI-compatible
endpoint (no auth, configured for llm.iodesystems.com / Qwen), and
asks the model to perform a cross-language rename. Reports the
conversation and verifies the workspace was actually rewritten.

The temp copy is critical: the LLM is given write tools, and we don't
want it mutating the repo's checked-in fixture.

Usage:
    python3 scripts/smoke/llm_e2e.py
        runs against llm.iodesystems.com with model Qwen3-6-27B-MPT.

    python3 scripts/smoke/llm_e2e.py --model Qwen3-6-27B-MPT-MAIN
    python3 scripts/smoke/llm_e2e.py --endpoint https://other/v1/chat/completions
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import subprocess
import sys
import tempfile
import urllib.request

DEFAULT_ENDPOINT = "https://llm.iodesystems.com/v1/chat/completions"
DEFAULT_MODEL = "Qwen3-6-27B-MPT"
POLYGLOT_REL = os.path.join("testdata", "fixtures", "polyglot")
MAX_ITERATIONS = 20
HTTP_TIMEOUT_S = 120


# ----- MCP client -----------------------------------------------------------


class MCP:
    """Tiny newline-delimited JSON-RPC client wrapping a subprocess."""

    def __init__(self, proc: subprocess.Popen) -> None:
        self.proc = proc
        self._id = 0

    def send(self, obj: dict) -> None:
        assert self.proc.stdin is not None
        self.proc.stdin.write(json.dumps(obj) + "\n")
        self.proc.stdin.flush()

    def recv(self) -> dict:
        assert self.proc.stdout is not None
        line = self.proc.stdout.readline()
        if not line:
            raise RuntimeError("EOF on MCP server stdout")
        return json.loads(line)

    def req(self, method: str, params: dict | None = None) -> dict:
        self._id += 1
        self.send(
            {
                "jsonrpc": "2.0",
                "id": self._id,
                "method": method,
                "params": params or {},
            }
        )
        return self.recv()

    def notify(self, method: str, params: dict | None = None) -> None:
        self.send({"jsonrpc": "2.0", "method": method, "params": params or {}})

    def init(self) -> None:
        self.req("initialize", {"protocolVersion": "2024-11-05", "capabilities": {}})
        self.notify("notifications/initialized", {})

    def tools(self) -> list[dict]:
        return self.req("tools/list")["result"]["tools"]

    def call(self, name: str, args: dict) -> tuple[str, bool]:
        """Returns (text, is_error)."""
        resp = self.req("tools/call", {"name": name, "arguments": args})
        if "error" in resp:
            return f"JSON-RPC error: {resp['error'].get('message', resp['error'])}", True
        result = resp.get("result") or {}
        contents = result.get("content") or []
        is_error = bool(result.get("isError"))
        if contents and isinstance(contents[0], dict) and "text" in contents[0]:
            return contents[0]["text"], is_error
        return json.dumps(result), is_error

    def shutdown(self) -> None:
        try:
            self.req("shutdown")
        except Exception:
            pass
        try:
            assert self.proc.stdin is not None
            self.proc.stdin.close()
        except Exception:
            pass


# ----- OpenAI-compatible chat ----------------------------------------------


def llm_chat(endpoint: str, model: str, messages: list[dict], tools: list[dict]) -> dict:
    body = json.dumps(
        {
            "model": model,
            "messages": messages,
            "tools": tools,
            "max_tokens": 1500,
        }
    ).encode()
    req = urllib.request.Request(
        endpoint,
        data=body,
        headers={"Content-Type": "application/json"},
    )
    with urllib.request.urlopen(req, timeout=HTTP_TIMEOUT_S) as r:
        return json.loads(r.read())


def to_openai_tools(mcp_tools: list[dict]) -> list[dict]:
    """Translate MCP tool definitions into OpenAI function-tool schema."""
    out = []
    for t in mcp_tools:
        schema = t.get("inputSchema") or {}
        if isinstance(schema, str):
            schema = json.loads(schema)
        out.append(
            {
                "type": "function",
                "function": {
                    "name": t["name"],
                    "description": t.get("description", ""),
                    "parameters": schema,
                },
            }
        )
    return out


# ----- main ----------------------------------------------------------------


SYSTEM_PROMPT = """You are an autonomous code-refactoring agent. You have access to MCP tools that operate on the workspace. All file paths are workspace-relative.

Tool playbook (use exactly this flow):
- structure(path='.', depth=2) — survey the workspace; returns directories and files.
- structure(path='<file>') — outline a file's named declarations. Each entry has BOTH a `range` (whole declaration: startLine/startCol/endLine/endCol) and a `nameRange` (just the identifier: nameStartLine/nameStartCol/nameEndLine/nameEndCol).
- To rename an identifier across the workspace: call node_refactor with file=the file where the identifier is declared, the FOUR nameRange fields as the range, kind='rename', and newName=<the new name>. ONE call does the whole cross-language rewrite.
- After the refactor, briefly summarize what changed.

Do not ask for permission. Use the tools."""

USER_TASK = "Rename the UserID identifier to PersonID across this entire workspace."


def build_binary(repo_root: str, out_path: str) -> None:
    print(f"[build] poly-lsp-mcp → {out_path}", file=sys.stderr)
    res = subprocess.run(
        ["go", "build", "-o", out_path, "."],
        cwd=repo_root,
        capture_output=True,
        text=True,
    )
    if res.returncode != 0:
        print(res.stderr, file=sys.stderr)
        raise SystemExit("build failed")


def walk_files(root: str) -> list[str]:
    out = []
    for d, _, files in os.walk(root):
        if ".poly-lsp-mcp" in d:
            continue
        for f in files:
            out.append(os.path.relpath(os.path.join(d, f), root))
    return sorted(out)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--endpoint", default=DEFAULT_ENDPOINT)
    parser.add_argument("--model", default=DEFAULT_MODEL)
    parser.add_argument("--keep-tmp", action="store_true", help="Don't rm the temp workspace afterwards.")
    args = parser.parse_args()

    repo_root = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
    src = os.path.join(repo_root, POLYGLOT_REL)
    if not os.path.isdir(src):
        print(f"[err] fixture missing: {src}", file=sys.stderr)
        return 1

    tmp = tempfile.mkdtemp(prefix="poly_lsp_mcp_llm_")
    ws = os.path.join(tmp, "polyglot")
    shutil.copytree(src, ws)

    # Write a poly-lsp-mcp.yaml declaring the polyglot fixture's schema
    # files so api.proto, openapi.yaml, and user.schema.json contribute
    # bindings. Without this the LLM couldn't reach UserID inside
    # api.proto (the lexical registry doesn't include `.proto`).
    cfg_path = os.path.join(ws, "poly-lsp-mcp.yaml")
    with open(cfg_path, "w") as cf:
        cf.write(
            "schemas:\n"
            "  - {file: api.proto, dialect: proto}\n"
            "  - {file: openapi.yaml, dialect: openapi}\n"
            "  - {file: user.schema.json, dialect: jsonschema}\n"
        )

    binary = os.path.join(tmp, "poly-lsp-mcp")
    build_binary(repo_root, binary)

    print(f"[mcp] starting against {ws}", file=sys.stderr)
    proc = subprocess.Popen(
        [binary, "mcp", "--root", ws, "--config", cfg_path],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL,
        text=True,
    )
    mcp = MCP(proc)
    mcp.init()

    mcp_tools = mcp.tools()
    oai_tools = to_openai_tools(mcp_tools)
    print(f"[mcp] tools: {[t['name'] for t in mcp_tools]}", file=sys.stderr)
    print(f"[llm] endpoint={args.endpoint} model={args.model}", file=sys.stderr)

    messages: list[dict] = [
        {"role": "system", "content": SYSTEM_PROMPT},
        {"role": "user", "content": USER_TASK},
    ]

    print("\n=== conversation ===", file=sys.stderr)
    final_text = ""
    last_assistant_was_tool = False
    for it in range(MAX_ITERATIONS):
        try:
            resp = llm_chat(args.endpoint, args.model, messages, oai_tools)
        except Exception as e:
            print(f"[err] LLM call failed: {e}", file=sys.stderr)
            break

        choice = resp.get("choices", [{}])[0]
        msg = choice.get("message", {})
        finish = choice.get("finish_reason", "")

        # Persist whatever the model returned in history.
        assistant_entry = {"role": "assistant", "content": msg.get("content") or ""}
        if msg.get("tool_calls"):
            assistant_entry["tool_calls"] = msg["tool_calls"]
        messages.append(assistant_entry)

        if msg.get("tool_calls"):
            last_assistant_was_tool = True
            for tc in msg["tool_calls"]:
                name = tc["function"]["name"]
                raw_args = tc["function"].get("arguments") or "{}"
                try:
                    parsed_args = json.loads(raw_args) if isinstance(raw_args, str) else raw_args
                except json.JSONDecodeError:
                    parsed_args = {}
                args_repr = json.dumps(parsed_args)
                if len(args_repr) > 120:
                    args_repr = args_repr[:117] + "..."
                print(f"  [{it:02d}] {name}({args_repr})", file=sys.stderr)
                text, is_error = mcp.call(name, parsed_args)
                marker = "ERR" if is_error else "ok "
                preview = text.replace("\n", " ")
                if len(preview) > 200:
                    preview = preview[:197] + "..."
                print(f"        {marker} → {preview}", file=sys.stderr)
                messages.append(
                    {
                        "role": "tool",
                        "tool_call_id": tc["id"],
                        "content": text,
                    }
                )
            continue

        last_assistant_was_tool = False
        if msg.get("content"):
            final_text = msg["content"]
            print(f"\n[agent] {final_text}\n", file=sys.stderr)
        if finish in ("stop", "length", ""):
            break
    else:
        print(f"[warn] hit MAX_ITERATIONS={MAX_ITERATIONS}", file=sys.stderr)

    if last_assistant_was_tool and not final_text:
        print("[note] conversation ended on a tool call without a final summary", file=sys.stderr)

    mcp.shutdown()
    try:
        proc.wait(timeout=5)
    except subprocess.TimeoutExpired:
        proc.kill()

    # ----- verification ----------------------------------------------------

    print("\n=== verification ===", file=sys.stderr)
    files = walk_files(ws)
    changed: list[str] = []
    total_userid_before = 0
    total_userid_after = 0
    total_personid_after = 0
    for rel in files:
        orig_path = os.path.join(src, rel)
        new_path = os.path.join(ws, rel)
        if not os.path.exists(orig_path):
            continue
        with open(orig_path) as f:
            orig = f.read()
        with open(new_path) as f:
            new = f.read()
        total_userid_before += orig.count("UserID")
        total_userid_after += new.count("UserID")
        total_personid_after += new.count("PersonID")
        if orig != new:
            changed.append(rel)

    print(f"files changed: {len(changed)} / {len(files)}", file=sys.stderr)
    for f in changed:
        print(f"  {f}", file=sys.stderr)
    print(file=sys.stderr)
    print(f"UserID   before: {total_userid_before}", file=sys.stderr)
    print(f"UserID   after:  {total_userid_after}  (remaining are expected in comments / proto field-type refs)", file=sys.stderr)
    print(f"PersonID after:  {total_personid_after}", file=sys.stderr)

    # Success: enough files changed to span every supported format
    # (go/ts/py + proto + openapi + jsonschema + sql + md + yaml = 9),
    # AND the new name is widely present. Remaining UserID in comments
    # is expected behavior, not a failure — comments aren't tree-sitter
    # identifier nodes so the rename intentionally doesn't touch them.
    success = len(changed) >= 8 and total_personid_after >= 15
    print(file=sys.stderr)
    print("PASS" if success else "FAIL", file=sys.stderr)

    if not args.keep_tmp:
        shutil.rmtree(tmp, ignore_errors=True)
    else:
        print(f"\n[kept] workspace at {ws}", file=sys.stderr)

    return 0 if success else 1


if __name__ == "__main__":
    sys.exit(main())
