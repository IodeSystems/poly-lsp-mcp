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

## North Star — what "world-class" means for us

The owned quadrant, empty of incumbents: a **live, no-build, multi-language,
reference-aware, edit-capable code querier with a predictable, explainable cost
model** — driven by an LLM agent, live, mid-task. Every priority is judged
against holding THIS position, not against beating CodeQL at its own game.

- **The consumer is a MODEL, not a security engineer** — that inverts the
  weights. A grammar the model already knows (CSS). Loud, honest partiality
  (`conf` labels, budget-blow-that-says-so, `:explain`, `>x` floors) as
  HALLUCINATION-PREVENTION, not polish — a human supplies the skepticism a
  model doesn't. Token-lean, composable output over rich reports.
- **The unoccupied intersection:** {live + no-build} × {cross-file reference
  edges with a precision LADDER} × {mutation} × {predictable cost}.
  CodeQL/Glean/Kythe/Sourcegraph buy the reference graph with a
  build/batch/per-language extractor and cannot rewrite; ast-grep/Comby/Semgrep
  rewrite live but have NO cross-file graph. Nobody sits in the middle.
- **The moat is the precision LADDER:** lexical → tree-sitter-scoped →
  LSP-resolved, each LABELED. "Resolved or lexical, and it says which" is a
  category property no interactive querier ships.

Non-goals — forfeiting these is what KEEPS the quadrant:
- **Dataflow / taint / points-to** (CodeQL's turf) needs the build+batch we
  refuse. Explicit non-goal, not a gap.
- **We do not out-scale Sourcegraph/Glean on multi-repo.** A single owned root
  is home.

**The known chasm — ownership domains.** The precision pass does not ASK to
leave the workspace; it crosses by definition — gopls resolves into the stdlib,
tsserver into `node_modules`, pylsp into `site-packages`. The moment it works,
a `refFar` can land OUTSIDE the git root, where `fileByRel` has no node and the
edge silently falls back to a false local match (the `Write`/`Read` collision
the icebox flags). So the single-`.project` assumption is already leaking, and
"lib linking" is not a feature to add but a boundary already being crossed.
Bridge is staged — **Stage 0 SHIPPED**: a resolved far end outside the root is
now an honest EXTERNAL STUB (`module@version#sym`, `domain: external`,
read-only, `[not indexed]`) — nameable, never a false local (see the Edges
slice below). Stages 1–2 (content-addressed lib partitions, on-demand,
evictable) are deferred design in icebox ("Ownership domains"). **Rule until
then: nothing new may hard-code the single-root assumption deeper** — a node's
`domain` (owned rw / vendored ro / external ro) is the axis that will gate
mutation and budget; the field now EXISTS on treeNode (`"" `=owned,
`"external"`), populated by Stage 0.
**Decided: crossing into libs is opt-IN** (`:with(libs)` / drill into a stub),
the default stays workspace-bounded — revisit only if adoption data shows
agents actually want the cross-lib answer unprompted.

## Current state

- Phases 0–6.1 (scaffold, multiplex, cross-language index + rename,
  stacked-branch parse cache, MCP server, diagnostics-in-edit-responses,
  tool ergonomics) — ✅ all in `done.md`.
- MCP default surface: **3 tools** — node_query / node_read / node_edit —
  over one unified node tree (project > dir > file > symbols > argument),
  addressed as `<file>#<sym>`, queried by CSS selector. Legacy 9-tool surface
  behind `--legacy-tools`. Sandbox jail + read-only mode: commit `0fbeb02`.
- Selector language: CSS containment + the graph as NODES. References are
  reified edge pseudo-elements — `::in`/`::out` on TWO orthogonal class axes,
  KIND (.call/.type/.import) × POSITION (.return/.param/.field/.var), composed
  CSS-style (`::in.return.type`); far end (via `>`) is the SOURCE symbol, the
  ref IS the occurrence (address = site file@line), invisible to `*`. `{m,n}` =
  regex repetition; edge hops on an edge element. `:parents(sel)` = the one
  inverse. Bare `:any/:all/:empty` = position claims. Language classes
  (file.go), `:first`/`:last` per anchor. Attribute axes: `[name]` (called) vs
  `[path]` (lives), ops `= ^= $= *=` literal, `~=` regex (bracket-aware).
  Shipped 2026-07-17 → done.md ("Graph selector language" + the per-feature
  entries) for the full record.
- Trivia/metadata as NODES (→ done.md): `annotation` (decorators py/ts/kotlin/
  groovy + struct-tag keys go, a CHILD of its symbol, leaf + virtual-FQN
  alias — NOT java, see Active work), `::comment` (joined doc block, a
  GENERATED pseudo-element invisible to `*`), `argument` (params).
  `:annotated('pat')`/`:contains('pat')` are the text fallbacks (Go comment
  directives have no AST node).
- **Twelve languages index and model nodes** (→ done.md for the per-language
  record): go, typescript/tsx, python, java, kotlin, groovy, c, c++, sql, xml,
  markdown, plus proto/graphql/yaml/json on the lexical tier. Markdown is
  structural — nested `section` nodes owning their body, and only headings,
  fenced code and inline code spans enter the index. Child LSPs default for
  go/ts/py (gopls/tsserver/pylsp) and c/c++ (clangd); the JVM trio is
  tree-sitter only because their servers want a build.
- **Daemon mode is COMPLETE** (→ done.md): `mcp --daemon` proxies to one
  shared per-user daemon over a unix socket — one warm index, one child-LSP
  fleet, one parse cache for every client.

## Active work

Conventions for this file are at the top: current state + active work ONLY.
Completed trees live in `plan/done.md`; deferred opt-ins in `plan/icebox.md`.

Open frontier:

✅ **A trailing comment now belongs to the line it sits on — 2026-08-01.** A
dogfood `node_edit` wrote a struct field the way it appears on screen, comment
included, and got "oldText not found". Behind it: both span rules asked only
whether a comment ENDED on the line above a declaration, which a comment
trailing the PREVIOUS declaration does. In go/java that mis-aimed `::comment`;
in typescript/kotlin/c it pulled the neighbour's comment into the next
declaration's span, so deleting a field DESTROYED the comment above it and
stranded its own. A declaration now owns the comment trailing it (USER's call
— the existing "owns its doc comment" rule, pointed the other way), and owns
no other. Full forensics in `plan/bugs.md`.
- **risks**: field node text is now wider wherever a trailing comment exists,
  so a caller that matched on the bare declaration still works (substring) but
  a caller comparing whole node text sees more.
- Java's field span was widened in the same pass (USER, 2026-08-01): a java
  field's range was the bare declarator (`name`), now the whole
  `field_declaration` — `@Inject String name; // why`, modifiers and
  annotations included. Same single-declarator rule as C: `int a, b;` keeps
  per-declarator ranges so siblings cannot overlap.

◻ **Bug CLASSES are asserted across the whole language registry —
2026-08-01.** `TestLanguageBugMatrix` (+ `langmatrix_classes_test.go`) states a
property ONCE and runs it against `config.Default().Languages`, not a
hand-written list. Every registered language must land in exactly one cell —
`ok` / `n/a`+reason / `KNOWN`+reason / **unmeasured, which FAILS** — so adding
a grammar breaks the test until someone decides, per class, where it belongs.
A `KNOWN` entry that starts passing ALSO fails, so a fix cannot leave a stale
"unsupported" note behind. Motivation: the hand-written table shipped with the
trailing-comment fix listed five languages and silently omitted java, which was
the one still broken. Two classes seeded ("owns the comment trailing it",
"owns the doc block above it"); the harness's three failure modes are each
verified by negative control.
- **next**: decide the two violations the matrix surfaced on its first run
  (below), then add a class per bug that turns out to be language-shaped.
- **risks**: fixtures are per-language source, so a class costs real authoring
  per grammar — the pressure will be to write `n/a` instead of a fixture.
  `n/a` reasons are prose and nothing checks they are true.
- The matrix surfaced two real defects on its first run. **typescript is
  FIXED** (USER, 2026-08-01): an exported declaration lost its doc comment and
  its `export` keyword. The wrappers nest — `export const x = 1` is a
  declarator inside a lexical_declaration inside an export_statement — so
  rising one level would have fixed `export function`, left `export const`
  broken, and shown the class GREEN. `docCommentAnchor` loops instead, and the
  fixture now covers all three shapes plus a bare `const`, which had no doc
  comment either. 0 overlaps and identical TS symbol counts on a real
  frontend, so the wider spans lose nothing.
- **sql remains KNOWN**: comments are not attached to statements at all, so
  `-- doc` above a `CREATE TABLE` is dropped. (Column-level trailing comments
  DO work, so it is specifically the block-above rule.) Not yet a decision —
  raise it if SQL docs start mattering.


✅ **The child LSP now hears about OUT-OF-BAND writes — 2026-07-31.** A
`git rebase` run through a dun session's exec tool rewrote a file on disk; the
fsnotify watcher re-indexed it and told the child LSP nothing, so gopls kept
type-checking against a proactively-`didOpen`'d overlay that had been wrong
for twenty minutes — confidently, with `diagnosticsTimedOut:false`, while
`go build`, `node_read` and `node_query` all agreed on the truth. The watcher
now pushes what it indexes (`notifyChildOfExternalChange`, `didClose` on
delete), hash-guarded so the tool's own writes aren't echoed back. Full
forensics in `plan/bugs.md`; pinned by a test with a demonstrated negative
control. Two ergonomics fixes rode along, both from the same session corpus:
an empty `oldText` now CREATES a file instead of answering "no such file",
and a shell-double-quoted `::grep('-E "a|b"')` is answered with the search the
caller meant plus a note saying what was stripped, where it used to be an
error (and before that, a silent zero-match).
- **risks**: an out-of-band change now costs a `didChange` round-trip per
  watched file, so a branch switch across hundreds of files is a burst the
  child must absorb; the debounce coalesces per path, not across the batch.
  Untested at that scale.
- **optional extensions**: register `workspace/didChangeWatchedFiles`
  properly, which would also cover files that were never opened — it needs
  client capabilities beyond `{}` and a `client/registerCapability` handler,
  neither of which the current fix requires.

✅ **Edit parity across every callable language — 2026-07-28.** The gap that
headed this list is closed. `node_refactor` signature rewriting now covers
java, kotlin, groovy, c and c++ alongside go/typescript/python
(`symbols/refactor_langs.go`), and Java gained the `.annotation` children five
other languages already had. XML followed: an element's ATTRIBUTES are
its parameter list, through its own non-grammar path. Only SQL and Markdown
remain out — a stored function's callers are strings inside other statements,
and a document section has no call site. (Rename always worked everywhere,
including XML; it runs over the index, not the grammar.) Found and fixed while exercising it:
a C++ out-of-line rename spanned the whole `Widget::area`, deleting the class
scope and silently detaching the definition — now pinned by
`TestCppQualifiedRenameKeepsScope`.

✅ **Verification debts closed — 2026-07-28** → done.md. Idiomatic Java is
now measured on JDK 21's `java.base` (3,498 hand-written files, 246k symbols,
0 parse errors; fixed `module-info.java`, the one file that indexed empty).
yaml/json measured on a real config tree: the "keep every token" rule works as
designed on hand-written config (62 files, 343 names, 3% junk), and the noise
is entirely generated data.

✅ **The index honours `.gitignore` — 2026-07-28** → done.md. Far bigger than
the config finding that prompted it: on `zdx-go`, **2,128,213 → 471,029 sites
(-78%)**, with go itself down 75%. Filters IGNORED, never UNTRACKED, so a file
an agent just created still indexes. The bindings walker honours it too, since
a binding is a stronger claim than an indexed name. Shared as
`internal/git.IgnoreSet`.

⏸ **Groovy is speculative** — zero corpus on this box (→ done.md). A
Jenkinsfile cannot even be routed: it is extensionless and the registry keys
on extension alone.

◻ **Known limits, recorded so they are not re-investigated.**
  - SQL: `CREATE PROCEDURE` is unparseable by tree-sitter-sql (3 migration
    files stay empty); quoted identifiers keep their quotes
    (`"USER"."USER_pkey"`); index/view/type/trigger/sequence/constraint all
    collapse to class `type`, so a selector cannot ask for views specifically.
  - Go keeps pointer/slice decoration on return segments (`*Config`,
    `[]Schema`) while java/kotlin/typescript/c strip it — USER's call
    2026-07-27, documented on `goTypeSegment`.
  - Markdown keeps inline backtick spans in the index. Assistant's judgement,
    not an instruction: dropping them breaks the prose-rename claim and two
    polyglot tests. Reversible in one line.
  - TypeScript arrow components (`const Foo = () => …`) get no
    `.argument`/`.return` children. Measured at 9 of 331 declarators (3%) on a
    real React frontend, so left alone; revisit against a codebase that
    actually writes them.

✅ **Language coverage hardening — 2026-07-26/28, twelve languages → done.md**
for the per-language record. Added c/cpp, kotlin, groovy; markdown became
section NODES with prose out of the index (34,093 → 4,979 sites); the Android
resource binding reads Kotlin literals; and every existing arm was verified
against a live corpus, which is where the defects came from — Kotlin classes
losing every member to one bad statement, `.d.ts` files indexing as empty,
96% of TS return segments unusable, Python indexing no bindings at all, Go
composites claiming a false leaf, SQL missing functions/triggers/sequences and
246 constraint names.

✅ **Daemon mode — COMPLETE → done.md.** One shared poly-lsp per user; steps
1-5 + per-connection read-only/validate policy shipped, step 6 (worktree COW)
measured and iceboxed. Verified live 2026-07-28: `mcp --daemon` auto-starts,
warms the root and proxies tool calls; `daemon --stop` shuts it down.

✅ **`--validate`, staged-edit transactions (`commit:false`) and editable
`::signature` / `::body` — all SHIPPED → done.md.** The safe-edit loop:
revert-on-new-diagnostics, benchmarked and measured with error bars.

✅ **Dogfooding wired → done.md.** `.mcp.json` registers poly-lsp as a native
tool for Claude Code in this repo, so the project develops itself with its own
`node_query`/`node_read`/`node_edit`. Two dogfood passes found cost cliffs
100% test coverage had missed. **Both residual items from that arc are now
SHIPPED → done.md ("Rename you can check, and the UTF-16 column fix").**

✅ **`:explain` cost-visible queries + cardinality-ordered descendant chains —
SHIPPED → done.md.**


◐ **Java + Android XML — the JVM/Android blind spot (started 2026-07-26).**
Motivating measurement, termux-app: 17 of 291 source files were visible
(yml/md/json only) — 197 `.java` and 67 `.xml` invisible, i.e. 6% of the repo
and none of the code. Slice 1 (Java via tree-sitter) and slice 2 (the Android
resource binding, `internal/bindings/android.go`) SHIPPED → done.md; the
binding now reads Kotlin literals as well as Java ones, and XML gained real
named symbols (USER, 2026-07-27).
- **next**: decide whether this tree is complete now that XML is real, or
  whether `R.id.x` still wants the generated-R side.
- **risks**: only direct literals are read (no constant folding, so
  `KEY = PREFIX + "x"` is invisible); the tier-2 distinctiveness heuristic is
  lexical, so a genuinely short lowercase resource name paired with a real
  literal is dropped as noise.
- **optional extensions**: jdtls as an opt-in child LSP for resolved edges and
  safe rename.


◐ **Adoption measurement — the existential question, now instrumented.**
`llm-bench/` (own nested module, uses `agentkit` — `mcpmgr` spawns the server,
`agent.Session` drives the model on llm.iodesystems.com) poses relationship-
shaped code-nav tasks with BOTH the graph tools AND a strong grep/read/list
baseline present, and tallies **reach-rate** — the fraction of tasks that call
`node_query` at all. Compiles + the server binary spawns (verified); NOT yet
run (needs `AGENTKIT_API_KEY` + network). **next**:
  - ◻ **Run the `asis` baseline.** Reach-rate high → adoption isn't the wall,
    engine work is justified. Low → the icebox's "0 calls / 8 runs" reproduces
    and everything below is premature.
  - ✅ **First A/B run (asis vs pattern vs inspired, 3 direct-question tasks,
    n=1).** Reach-rate SATURATED — 3/3 for all three variants; even spec-first
    `asis` gets node_query reached for (callers answered correctly). **Finding:
    on direct relational questions the docs are NOT the wall** — which means the
    icebox "0 calls / 8 runs" was the IN-FLOW case (model mid-task, grep
    momentum, unprompted), not the wording. The direct-question set can't
    measure the thing that matters; I measured the easy case. Only flicker:
    `pattern` reached graph grep-free most often (weak, n=1).
  - ✅ **In-flow grep-tempting set built + run (5 tasks incl. a collision
    canary; asis/pattern/inspired, n=1).** Metrics: reach, graph-FIRST,
    grep-free. Result across ALL three variants: reach **4/4**, graph-first
    **4/4** even in-flow — the model reaches for node_query FIRST, unprompted,
    on grep-tempting tasks, regardless of doc wording. grep-free was the only
    mover (asis/pattern 3/4, **inspired 2/4** — its longer prose induced MORE
    grep scaffolding; the verbose "inspirations" strategy underperforms tight
    recipes and even the plain spec).
  - ❗ **The bench structurally CANNOT answer the absolute adoption question.**
    In-harness, grep is just another advertised tool with a one-line desc — it
    has NO home-field advantage. The icebox's "0 calls" was grep as the model's
    NATIVE, reflexively-trained tool inside a real agent. This bench is NEUTRAL
    ground, where node_query competes as a peer and wins on description alone.
    So: node_query is *competitive as a peer* (real result), but "does it beat
    the model's native grep/read in a real agent" needs a REAL agent — and the
    one we had wired (autowork3) is a DEAD PROJECT (dropped 2026-07-21, user
    call). The native-vs-newcomer asymmetry is now UNMEASURED with no vehicle;
    llm-bench's standing value is A/B descriptions as peers, correctness, cost.
  - ◻ **Collision canary** (`collision*`): grep AND lexical node_query both
    return the merged set (verified) — flips to a graph win only when the LSP
    precision pass resolves the site. Re-run with precision ON to show the
    graph's real differentiator.
**Do NOT rewrite `modern.go`'s description** — across quick + in-flow, two
sets × three variants, the shipped spec-first desc already saturates reach and
graph-first. The wording is not the bottleneck on neutral ground; if `inspired`
taught anything it's that MORE prose is worse, not better.
**blocking decision (USER owns)**: adoption can no longer be measured on
home turf (autowork3 dead). Either find/build another real-agent vehicle, or
accept the llm-bench peer result and let the roadmap ride on it.

✅ **Cost visibility + estimator, edges, and the node model — all three trees
COMPLETE → done.md.** `:explain` cost trees, the descendant-chain planner,
short-circuiting, the ~76k inversion floor; edges settled per-edge with
tri-state `conf`; `return`/`argument`/`annotation` as nodes, `:arity()`,
`::grep` caps. The one deliberate non-ship is the general-form chain reorder
(built, measured at 0% saving on real code, reverted).

## Non-goals (for now)

- Indexing the entire host filesystem; we only index inside the git root. (The
  daemon serving MANY roots does not change this — each workspace stays
  single-rooted; the daemon is a process-sharing move, not a scope change.)
- Replacing any single child LSP. We multiplex, we don't reimplement.
- Sandboxing child LSPs. They run as the user.
- Windows support until someone asks.
