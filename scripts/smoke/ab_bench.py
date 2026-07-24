#!/usr/bin/env python3
"""A/B task bench: poly-lsp structural tools vs vanilla grep/read/edit.

Runs a NAMED task (from the TASKS registry) through both tool surfaces on
a fresh temp copy of a real repo, verifies the result with a deterministic
STATIC predicate (go vet / grep counts — no DB, no running app), and prints
the tool-call delta.

Reuses the MCP client + LLM plumbing from llm_e2e.py and the vanilla toolbox
from vanilla_e2e.py by import (those modules guard main() under __main__, so
importing has no side effects). This file adds only the task registry, the
real-repo fixture handling, and the shared agent loop.

Usage:
    python3 scripts/smoke/ab_bench.py                       # islive-rename, both arms
    python3 scripts/smoke/ab_bench.py --arm poly            # one arm only
    python3 scripts/smoke/ab_bench.py --task islive-rename --keep-tmp
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import subprocess
import sys
import tempfile

_HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, _HERE)

from llm_e2e import MCP, build_binary, llm_chat, to_openai_tools  # noqa: E402
from vanilla_e2e import VANILLA_TOOLS, Workspace, dispatch  # noqa: E402

REPO_ROOT = os.path.abspath(os.path.join(_HERE, "..", ".."))  # poly-lsp-mcp
SIBLINGS = os.path.abspath(os.path.join(REPO_ROOT, ".."))  # .../iodesystems
MAX_ITERATIONS = 30
# Heavy/irrelevant trees we never copy into the temp fixture.
COPY_IGNORE = shutil.ignore_patterns(
    ".git", "node_modules", "dist", "build", ".next", ".turbo", "__pycache__"
)


# ----- task registry -------------------------------------------------------
#
# Each task: fixture (sibling repo dir name), instruction (the NL prompt —
# self-contained, names real code), optional setup (shell run in the temp
# copy BEFORE the agent, to construct a starting state), and verify (shell
# run in the temp copy AFTER; exit 0 == PASS). Verify must be static: no DB,
# no app boot.

TASKS: dict[str, dict] = {
    "islive-rename": {
        "fixture": "redline",
        "instruction": (
            "In internal/integrations/payments/payments.go, the payments `Gateway` "
            "interface declares a method `IsLive() bool`. Rename that method to "
            "`MovesRealMoney` across the payments integration: the interface itself, "
            "every implementation (LogGateway and Router in payments.go, plus the NMI "
            "and EPN gateways in nmi.go and epn.go), and every call site (including "
            "internal/api/admin_settings.go and any tests in the payments package).\n\n"
            "IMPORTANT: internal/integrations/llm has UNRELATED interfaces (Rewriter, "
            "Translator) that ALSO declare a method named `IsLive` — that is a different "
            "feature that merely shares the name. Do NOT rename those, their "
            "implementations, or their call sites (note internal/api/admin_settings.go "
            "has BOTH: one payments IsLive call and one llm-translator IsLive call — "
            "only the payments one changes).\n\n"
            "When finished, make sure the build still vets clean."
        ),
        "setup": None,
        "verify": r"""
set -uo pipefail
fail() { echo "VERIFY-FAIL: $1" >&2; exit 1; }

# 1. Build/interface satisfaction end-to-end (also proves llm still compiles).
#    pipefail is required: without it the pipeline's exit is tail's (0), so a
#    go vet failure would slip through unnoticed.
go vet ./... 2>&1 | tail -5 || fail "go vet did not pass"

# 2. The payments interface method was actually renamed.
grep -q "MovesRealMoney" internal/integrations/payments/payments.go \
  || fail "MovesRealMoney not present in payments.go"

# 3. Anti-collateral: the pure-llm files' IsLive count must be UNCHANGED (13).
n=$(grep -rho "IsLive" \
      internal/integrations/llm/openrouter.go \
      internal/integrations/llm/openrouter_test.go \
      internal/integrations/llm/translate.go \
      internal/integrations/llm/rewrite.go \
      internal/api/translate_test.go | wc -l)
test "$n" -eq 13 || fail "llm IsLive count changed: $n != 13 (unrelated feature was touched)"

# 4. The same-file collision was resolved correctly: admin_settings.go keeps
#    exactly one IsLive (the llm translator call) and gains one MovesRealMoney.
il=$(grep -c "IsLive" internal/api/admin_settings.go)
mr=$(grep -c "MovesRealMoney" internal/api/admin_settings.go)
test "$il" -eq 1 || fail "admin_settings.go IsLive lines = $il != 1"
test "$mr" -eq 1 || fail "admin_settings.go MovesRealMoney lines = $mr != 1"

echo "VERIFY-OK"
""",
    },
    "phone-region-param": {
        "fixture": "redline",
        "instruction": (
            "internal/api/validation.go's `normalizePhone(raw string) string` currently "
            "assumes US phone numbers. Add a `region string` parameter (the caller's "
            "country code, e.g. \"US\") and thread it through EVERY call site — pass \"US\" "
            "at each existing call so behavior is unchanged. The call sites are in "
            "internal/api (validation.go, user.go, donate.go, handlers.go, member_profile.go, "
            "user_admin_contacts.go) plus the test in validation_test.go.\n\n"
            "Do NOT modify the unrelated `normalizePhoneSearch` function in "
            "internal/api/user.go — it is a different function that merely shares the prefix.\n\n"
            "When finished, make sure the build vets clean."
        ),
        "setup": None,
        "verify": r"""
set -uo pipefail
fail() { echo "VERIFY-FAIL: $1" >&2; exit 1; }

# 1. The signature actually gained a region parameter.
grep -qE "func normalizePhone\(.*\bregion\b" internal/api/validation.go \
  || fail "normalizePhone signature has no region parameter"

# 2. The prefix-sharing decoy is untouched.
grep -q "func normalizePhoneSearch(s string) string" internal/api/user.go \
  || fail "normalizePhoneSearch signature was modified (the decoy)"

# 3. Build vets clean — proves EVERY call site was updated to the new arity;
#    a missed one is an arg-count mismatch and go vet fails.
go vet ./... 2>&1 | tail -5 || fail "go vet failed — a call site was likely not updated"

echo "VERIFY-OK"
""",
    },
}


# ----- generic agent loop (arm-agnostic) -----------------------------------


POLY_SYSTEM = """You are an autonomous code-refactoring agent operating on a real multi-file codebase. All paths are workspace-relative.

Your tools are node_query (find nodes and references — a selector language plus a call/type graph and ::grep), node_read (read a file or an addressed symbol whole), and node_edit (the one write tool: rename / oldText+newText / newText / delete / params / return). Their schemas describe exactly how to drive them.

Do the task the user gives you, then confirm it as the task asks. Do not ask for permission; use the tools."""

VANILLA_SYSTEM = """You are an autonomous code-refactoring agent operating on a real multi-file codebase. All paths are workspace-relative.

Your tools: grep(pattern) regex-searches every file (`path:line:text`); read_file(path) reads a whole file; str_replace(path, old_str, new_str) replaces ONE unique occurrence (include surrounding text to disambiguate). There is no rename/refactor tool — find and replace each occurrence yourself.

Do the task the user gives you, then confirm it as the task asks. Do not ask for permission; use the tools."""


def run_agent(endpoint, model, system, instruction, oai_tools, call_tool, label):
    """Drive the model to completion. call_tool(name, args) -> (text, is_error).
    Returns the number of tool calls made."""
    messages = [
        {"role": "system", "content": system},
        {"role": "user", "content": instruction},
    ]
    print(f"\n=== {label} conversation ===", file=sys.stderr)
    tool_calls = 0
    for it in range(MAX_ITERATIONS):
        try:
            resp = llm_chat(endpoint, model, messages, oai_tools)
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
                text, is_error = call_tool(name, parsed)
                preview = (text or "").replace("\n", " ")
                if len(preview) > 180:
                    preview = preview[:177] + "..."
                print(f"        {'ERR' if is_error else 'ok '} → {preview}", file=sys.stderr)
                messages.append({"role": "tool", "tool_call_id": tc["id"], "content": text})
            continue

        if msg.get("content"):
            print(f"\n[agent:{label}] {msg['content']}\n", file=sys.stderr)
        if finish in ("stop", "length", ""):
            break
    else:
        print(f"[warn] {label} hit MAX_ITERATIONS={MAX_ITERATIONS}", file=sys.stderr)
    return tool_calls


# ----- fixture + verification ----------------------------------------------


def make_fixture(task: dict) -> str:
    src = os.path.join(SIBLINGS, task["fixture"])
    if not os.path.isdir(src):
        raise SystemExit(f"fixture repo not found: {src}")
    tmp = tempfile.mkdtemp(prefix=f"ab_{task['fixture']}_")
    dst = os.path.join(tmp, task["fixture"])
    print(f"[copy] {src} -> {dst} (excluding .git/node_modules/dist/build)", file=sys.stderr)
    shutil.copytree(src, dst, ignore=COPY_IGNORE, symlinks=True)
    if task.get("setup"):
        r = subprocess.run(["bash", "-c", task["setup"]], cwd=dst, capture_output=True, text=True)
        if r.returncode != 0:
            raise SystemExit(f"setup failed: {r.stderr}")
    return tmp, dst


def verify(task: dict, ws_dir: str) -> tuple[bool, str]:
    r = subprocess.run(
        ["bash", "-c", task["verify"]], cwd=ws_dir, capture_output=True, text=True, timeout=600
    )
    out = (r.stdout + r.stderr).strip()
    return r.returncode == 0, out


# ----- arms ----------------------------------------------------------------


def run_poly(task, endpoint, model, keep_tmp):
    tmp, ws_dir = make_fixture(task)
    binary = os.path.join(tmp, "poly-lsp-mcp")
    build_binary(REPO_ROOT, binary)
    print(f"[poly] mcp --root {ws_dir}", file=sys.stderr)
    proc = subprocess.Popen(
        [binary, "mcp", "--root", ws_dir],
        stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True,
    )
    mcp = MCP(proc)
    mcp.init()
    oai_tools = to_openai_tools(mcp.tools())
    print(f"[poly] tools: {[t['function']['name'] for t in oai_tools]}", file=sys.stderr)
    calls = run_agent(endpoint, model, POLY_SYSTEM, task["instruction"], oai_tools, mcp.call, "poly")
    mcp.shutdown()
    try:
        proc.wait(timeout=5)
    except subprocess.TimeoutExpired:
        proc.kill()
    passed, out = verify(task, ws_dir)
    _cleanup(tmp, ws_dir, keep_tmp)
    return calls, passed, out


def run_vanilla(task, endpoint, model, keep_tmp):
    tmp, ws_dir = make_fixture(task)
    ws = Workspace(ws_dir)
    print(f"[vanilla] tools over {ws_dir}", file=sys.stderr)

    def call_tool(name, args):
        out = dispatch(ws, name, args)
        return out, out.startswith("ERROR")

    calls = run_agent(endpoint, model, VANILLA_SYSTEM, task["instruction"], VANILLA_TOOLS, call_tool, "vanilla")
    passed, out = verify(task, ws_dir)
    _cleanup(tmp, ws_dir, keep_tmp)
    return calls, passed, out


def _cleanup(tmp, ws_dir, keep_tmp):
    if keep_tmp:
        print(f"[kept] {ws_dir}", file=sys.stderr)
    else:
        shutil.rmtree(tmp, ignore_errors=True)


def main() -> int:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--task", default="islive-rename", choices=sorted(TASKS))
    p.add_argument("--arm", default="both", choices=["poly", "vanilla", "both"])
    p.add_argument("--endpoint", default="https://llm.iodesystems.com/v1/chat/completions")
    p.add_argument("--model", default="Qwen3-6-27B-MPT")
    p.add_argument("--keep-tmp", action="store_true")
    args = p.parse_args()
    task = TASKS[args.task]

    results = {}
    if args.arm in ("poly", "both"):
        results["poly-lsp"] = run_poly(task, args.endpoint, args.model, args.keep_tmp)
    if args.arm in ("vanilla", "both"):
        results["vanilla"] = run_vanilla(task, args.endpoint, args.model, args.keep_tmp)

    print(f"\n=== A/B: {args.task} (model {args.model}) ===", file=sys.stderr)
    print(f"{'arm':<10} {'calls':>6} {'result':>8}", file=sys.stderr)
    for arm, (calls, passed, _out) in results.items():
        print(f"{arm:<10} {calls:>6} {'PASS' if passed else 'FAIL':>8}", file=sys.stderr)
    for arm, (_c, passed, out) in results.items():
        if not passed:
            print(f"\n[{arm} verify output]\n{out}", file=sys.stderr)

    # Exit non-zero if any run FAILED verification (CI signal).
    return 0 if all(r[1] for r in results.values()) else 1


if __name__ == "__main__":
    sys.exit(main())
