#!/usr/bin/env python3
"""Vanilla-tools baseline for the poly-lsp LLM e2e bench.

The A/B partner to llm_e2e.py. SAME model, SAME fixture, SAME task
(cross-language UserID -> PersonID rename), but the model is given only
the plain, structure-BLIND tools a generic coding agent has — grep,
read_file, str_replace — implemented locally over a temp copy of the
polyglot fixture. No MCP server, no tree-sitter, no atomic rename.

The point is the DELTA: llm_e2e.py measures how many tool calls the
poly-lsp structural surface needs; this measures how many the vanilla
surface needs for the identical result. Run both, compare the counts.

Usage:
    python3 scripts/smoke/vanilla_e2e.py
    python3 scripts/smoke/vanilla_e2e.py --model Qwen3-6-27B-MPT-MAIN
"""

from __future__ import annotations

import argparse
import json
import os
import re
import shutil
import sys
import tempfile
import urllib.request

DEFAULT_ENDPOINT = "https://llm.iodesystems.com/v1/chat/completions"
DEFAULT_MODEL = "Qwen3-6-27B-MPT"
POLYGLOT_REL = os.path.join("testdata", "fixtures", "polyglot")
MAX_ITERATIONS = 40  # vanilla needs more turns; give it room, then report.
HTTP_TIMEOUT_S = 120
MAX_GREP_HITS = 200
SKIP_DIRS = {".git", ".poly-lsp-mcp", "node_modules"}


# ----- vanilla toolbox (local, structure-blind) ----------------------------


class Workspace:
    """grep / read_file / str_replace over a directory tree. The whole
    vanilla surface — no symbol index, no cross-file awareness."""

    def __init__(self, root: str) -> None:
        self.root = root

    def _abs(self, rel: str) -> str:
        # Keep every op inside the workspace (a rename agent has no
        # business escaping it); reject traversal loudly.
        abs_path = os.path.normpath(os.path.join(self.root, rel))
        if abs_path != self.root and not abs_path.startswith(self.root + os.sep):
            raise ValueError(f"path escapes workspace: {rel}")
        return abs_path

    def _walk(self) -> list[str]:
        out = []
        for d, dirs, files in os.walk(self.root):
            dirs[:] = [x for x in dirs if x not in SKIP_DIRS]
            for f in files:
                out.append(os.path.relpath(os.path.join(d, f), self.root))
        return sorted(out)

    def grep(self, pattern: str) -> str:
        try:
            rx = re.compile(pattern)
        except re.error as e:
            return f"ERROR: bad regex: {e}"
        hits = []
        for rel in self._walk():
            try:
                with open(self._abs(rel), encoding="utf-8") as fh:
                    for i, line in enumerate(fh, 1):
                        if rx.search(line):
                            hits.append(f"{rel}:{i}:{line.rstrip()[:200]}")
                            if len(hits) >= MAX_GREP_HITS:
                                hits.append(f"... (capped at {MAX_GREP_HITS} hits)")
                                return "\n".join(hits)
            except (UnicodeDecodeError, IsADirectoryError, FileNotFoundError):
                continue
        return "\n".join(hits) if hits else "(no matches)"

    def read_file(self, path: str) -> str:
        try:
            with open(self._abs(path), encoding="utf-8") as fh:
                return fh.read()
        except Exception as e:
            return f"ERROR: {e}"

    def str_replace(self, path: str, old_str: str, new_str: str) -> str:
        try:
            abs_path = self._abs(path)
            with open(abs_path, encoding="utf-8") as fh:
                content = fh.read()
        except Exception as e:
            return f"ERROR: {e}"
        count = content.count(old_str)
        if count == 0:
            return f"ERROR: old_str not found in {path}"
        if count > 1:
            return f"ERROR: old_str is not unique in {path} ({count} occurrences); add surrounding context to disambiguate"
        with open(abs_path, "w", encoding="utf-8") as fh:
            fh.write(content.replace(old_str, new_str, 1))
        return f"ok: replaced 1 occurrence in {path}"


VANILLA_TOOLS = [
    {
        "type": "function",
        "function": {
            "name": "grep",
            "description": "Search every file in the workspace for a Python-regex pattern. Returns matching lines as `path:line:text`.",
            "parameters": {
                "type": "object",
                "properties": {"pattern": {"type": "string"}},
                "required": ["pattern"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "read_file",
            "description": "Return the full text of a workspace-relative file.",
            "parameters": {
                "type": "object",
                "properties": {"path": {"type": "string"}},
                "required": ["path"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "str_replace",
            "description": "Replace a UNIQUE occurrence of old_str with new_str in one file. Errors if old_str is missing or occurs more than once (add context to disambiguate).",
            "parameters": {
                "type": "object",
                "properties": {
                    "path": {"type": "string"},
                    "old_str": {"type": "string"},
                    "new_str": {"type": "string"},
                },
                "required": ["path", "old_str", "new_str"],
            },
        },
    },
]


def dispatch(ws: Workspace, name: str, args: dict) -> str:
    if name == "grep":
        return ws.grep(args.get("pattern", ""))
    if name == "read_file":
        return ws.read_file(args.get("path", ""))
    if name == "str_replace":
        return ws.str_replace(args.get("path", ""), args.get("old_str", ""), args.get("new_str", ""))
    return f"ERROR: unknown tool {name}"


# ----- OpenAI-compatible chat (same retrying client as llm_e2e.py) ---------


MAX_LLM_RETRIES = 5
RETRY_BACKOFF_FALLBACK_S = 15


def llm_chat(endpoint: str, model: str, messages: list[dict], tools: list[dict]) -> dict:
    body = json.dumps(
        {"model": model, "messages": messages, "tools": tools, "max_tokens": 1500}
    ).encode()
    headers = {"Content-Type": "application/json"}
    token = os.environ.get("POLY_LSP_MCP_LLM_TOKEN") or os.environ.get("OPENAI_API_KEY")
    if token:
        headers["Authorization"] = f"Bearer {token}"

    for attempt in range(1, MAX_LLM_RETRIES + 1):
        req = urllib.request.Request(endpoint, data=body, headers=headers)
        try:
            with urllib.request.urlopen(req, timeout=HTTP_TIMEOUT_S) as r:
                return json.loads(r.read())
        except urllib.error.HTTPError as e:
            if e.code != 429 or attempt == MAX_LLM_RETRIES:
                try:
                    body_text = e.read().decode("utf-8", errors="replace")
                except Exception:
                    body_text = ""
                raise urllib.error.HTTPError(
                    e.url, e.code, f"{e.reason}: {body_text[:300]}", e.headers, None
                )
            import time as _time

            wait_s = RETRY_BACKOFF_FALLBACK_S * attempt
            print(f"[llm] 429, retrying in {wait_s}s ({attempt}/{MAX_LLM_RETRIES})", file=sys.stderr)
            _time.sleep(wait_s)
    raise RuntimeError("llm_chat: exhausted retries")


# ----- task (identical target to llm_e2e.py) -------------------------------


SYSTEM_PROMPT = """You are an autonomous code-refactoring agent working in a workspace of many files in different languages. All file paths are workspace-relative.

Your tools:
- grep(pattern) — regex-search every file; returns `path:line:text`.
- read_file(path) — read a whole file.
- str_replace(path, old_str, new_str) — replace ONE unique occurrence in a file. If old_str occurs more than once, include surrounding text to make it unique.

There is no rename or refactor tool. To rename an identifier everywhere you must find every occurrence and replace each one yourself, in every file and language. When you believe you are done, grep for the old name to confirm nothing is left, then briefly summarize.

Do not ask for permission. Use the tools."""

USER_TASK = "Rename the UserID identifier to PersonID across this entire workspace."


def walk_files(root: str) -> list[str]:
    out = []
    for d, _, files in os.walk(root):
        if any(s in d for s in SKIP_DIRS):
            continue
        for f in files:
            out.append(os.path.relpath(os.path.join(d, f), root))
    return sorted(out)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--endpoint", default=DEFAULT_ENDPOINT)
    parser.add_argument("--model", default=DEFAULT_MODEL)
    parser.add_argument("--keep-tmp", action="store_true")
    args = parser.parse_args()

    repo_root = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
    src = os.path.join(repo_root, POLYGLOT_REL)
    if not os.path.isdir(src):
        print(f"[err] fixture missing: {src}", file=sys.stderr)
        return 1

    tmp = tempfile.mkdtemp(prefix="poly_lsp_mcp_vanilla_")
    ws_dir = os.path.join(tmp, "polyglot")
    shutil.copytree(src, ws_dir)
    ws = Workspace(ws_dir)

    print(f"[vanilla] workspace {ws_dir}", file=sys.stderr)
    print(f"[vanilla] tools: {[t['function']['name'] for t in VANILLA_TOOLS]}", file=sys.stderr)
    print(f"[llm] endpoint={args.endpoint} model={args.model}", file=sys.stderr)

    messages: list[dict] = [
        {"role": "system", "content": SYSTEM_PROMPT},
        {"role": "user", "content": USER_TASK},
    ]

    print("\n=== conversation ===", file=sys.stderr)
    tool_calls = 0
    final_text = ""
    for it in range(MAX_ITERATIONS):
        try:
            resp = llm_chat(args.endpoint, args.model, messages, VANILLA_TOOLS)
        except Exception as e:
            print(f"[err] LLM call failed: {e}", file=sys.stderr)
            break

        choice = resp.get("choices", [{}])[0]
        msg = choice.get("message", {})
        finish = choice.get("finish_reason", "")

        entry = {"role": "assistant", "content": msg.get("content") or ""}
        if msg.get("tool_calls"):
            entry["tool_calls"] = msg["tool_calls"]
        messages.append(entry)

        if msg.get("tool_calls"):
            for tc in msg["tool_calls"]:
                tool_calls += 1
                name = tc["function"]["name"]
                raw = tc["function"].get("arguments") or "{}"
                try:
                    parsed = json.loads(raw) if isinstance(raw, str) else raw
                except json.JSONDecodeError:
                    parsed = {}
                shown = json.dumps(parsed)
                if len(shown) > 120:
                    shown = shown[:117] + "..."
                print(f"  [{it:02d}] {name}({shown})", file=sys.stderr)
                out = dispatch(ws, name, parsed)
                preview = out.replace("\n", " ")
                if len(preview) > 200:
                    preview = preview[:197] + "..."
                marker = "ERR" if out.startswith("ERROR") else "ok "
                print(f"        {marker} → {preview}", file=sys.stderr)
                messages.append({"role": "tool", "tool_call_id": tc["id"], "content": out})
            continue

        if msg.get("content"):
            final_text = msg["content"]
            print(f"\n[agent] {final_text}\n", file=sys.stderr)
        if finish in ("stop", "length", ""):
            break
    else:
        print(f"[warn] hit MAX_ITERATIONS={MAX_ITERATIONS}", file=sys.stderr)

    # ----- verification (identical criteria to llm_e2e.py) -----------------

    print("\n=== verification ===", file=sys.stderr)
    files = walk_files(ws_dir)
    changed: list[str] = []
    userid_before = userid_after = personid_after = 0
    for rel in files:
        op = os.path.join(src, rel)
        np = os.path.join(ws_dir, rel)
        if not os.path.exists(op):
            continue
        with open(op) as f:
            orig = f.read()
        with open(np) as f:
            new = f.read()
        userid_before += orig.count("UserID")
        userid_after += new.count("UserID")
        personid_after += new.count("PersonID")
        if orig != new:
            changed.append(rel)

    print(f"tool calls:      {tool_calls}", file=sys.stderr)
    print(f"files changed:   {len(changed)} / {len(files)}", file=sys.stderr)
    for f in changed:
        print(f"  {f}", file=sys.stderr)
    print(f"UserID   before: {userid_before}", file=sys.stderr)
    print(f"UserID   after:  {userid_after}", file=sys.stderr)
    print(f"PersonID after:  {personid_after}", file=sys.stderr)

    success = len(changed) >= 8 and personid_after >= 15
    print("\n" + ("PASS" if success else "FAIL"), file=sys.stderr)

    if not args.keep_tmp:
        shutil.rmtree(tmp, ignore_errors=True)
    else:
        print(f"\n[kept] workspace at {ws_dir}", file=sys.stderr)

    return 0 if success else 1


if __name__ == "__main__":
    sys.exit(main())
