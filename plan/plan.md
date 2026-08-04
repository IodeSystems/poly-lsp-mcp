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
- **A space is always a node boundary** (2026-08-03, BREAKING, `656114e`). The
  bare-attribute-attaches rule is gone: `func[path=a.go]` filters,
  `func path=a.go` descends, a bare attr is its own `*[…]`. The old exception
  lived only in the `?` help, which 2 of 426 measured calls ever asked for,
  while the always-present description said "space=descendant" and nothing
  else. Attribute booleans (`|`/`&`/grouping) shipped with it and are under
  review — see Active work.
- **Merge-conflict awareness** (2026-08-03/04): conflicts are NODES
  (`::conflict`/`::mine`/`::theirs`/`::base`, sides carrying git's ref),
  resolvable (`node_edit accept:`), unwriteable while straddled, withheld from
  the flat index when a row exists on neither side, and ANNOUNCED unprompted —
  poly-lsp declares `logging` and pushes on the transition, agentkit's mcpmgr
  delivers it, dun lifts it into the conversation.

## Active work

Conventions for this file are at the top: current state + active work ONLY.
Completed trees live in `plan/done.md`; deferred opt-ins in `plan/icebox.md`.

Open frontier:

✅ **An argument the tool does not have is REFUSED — 2026-08-04.** Swept the
legacy 9-tool surface; the defect it turned up was never legacy-only.
`encoding/json` drops unknown fields, so a misspelled or wrong-surface argument
was discarded and the tool answered a different question — and the answer
looked right. `structure(file:…)` ignored `file` (the arg is `path`) and listed
the whole workspace; `search(pattern:…, file:"util.ts")` returned hits from
another file; `node_read(node:…, lineLimt:2)` ignored the cap. The modern tools
were protected only by their `required` fields, which is luck, not design.
Checked at both dispatch paths against each tool's own declared schema, so no
handler changed. Arguments that are off-schema on PURPOSE are now named in
`Tool.Undeclared` (hidden `commit`/`rollback`, retired `grep`, node_read's v0.2
aliases) with a test keeping that list from rotting into a copy of the schema.
Also fixed two internal leaks found on the pass: `newErrors` serialized the
error-fingerprint dedup keys verbatim (NUL bytes, `file://` URIs), and a
missing range was reported by dumping the Go struct — the zero VALUES, never
the missing ARGUMENTS.
  - ◻ Known wart, left alone: legacy `node_read` with the v0.2 alias `limit`
    on a `node` address errors naming `lineLimit / lineLength`, params the
    caller never typed. It errors rather than silently ignoring, and the modern
    surface already says this well.

✅ **A declared binding that did not resolve now TELLS the client — 2026-08-04.**
A binding site is hand-written config, so the expected failure is a typo: a
symbol not in that file, a jsonpath matching nothing, a regex that found only
aliases. Every one produced a good stderr diagnostic and nothing else — and
stderr does not reach the model. From the query side, a binding that failed to
resolve is indistinguishable from one nobody declared: `::in`/`::out` simply
does not cross, silently, exactly as if the config were absent. Two routes now
carry it: a health block on the `initialize` response, and a warning pushed as
`notifications/message`. A clean binding set stays quiet — a warning every
startup would teach the model to ignore the channel. The report keeps
bindings/sites/links apart; collapsing them printed "5 of 1 applied".

✅ **Three usage misses found by scanning LIVE sessions — 2026-08-04.** The
sweep loop, run against `~/.dun/sessions` filtered to the last two days. The
date filter is the method, not a detail: the top error in the full corpus (50
hits, a bare path read as a tag) was fixed on 07-30 and every hit predated it.
Scanning without dating the corpus re-finds solved bugs.
  - **`node_query` now accepts the address it prints.** `matches[].node` is
    `"<file>#<sym>"`, `node_read` and `node_edit` both take it, and
    `node_query` rejected it — four times in one day, always an agent
    composing on an address it had just been handed. The refusal also gave
    advice that cannot work: `path=<file>#<sym>` matches nothing, because no
    path equals that string.
  - **An `oldText` bigger than the node is a wrong ADDRESS.** `::grep` hands
    back a one-line site address; it reads as a starting point, so the next
    call carries the whole enclosing block as `oldText`. Five hits, the top
    `node_edit` failure. The old error invited trimming the text when the fix
    is to widen the address, so it now names the enclosing declaration.
  - **"did you mean" is ranked by similarity, not file order.** It listed the
    package name and every import; a list that cannot contain the answer reads
    as "the symbol is not here". `A.B` where `A` exists shows A's members.

✅ **`|` is an operator only before another test — 2026-08-04 (USER).** The
boolean attribute grammar shipped on 08-03 made every `|` an operator, with a
quoted value as the escape. The next session's transcripts said that was the
wrong trade: **0** true boolean ORs in 97 real selectors against **19** regex
alternations inside one value (`func[name~=pending|queued|lifted|midTurn]`),
which the change turned into an error — and one recovery silently narrowed to
`[name~=pending]`, dropping three alternatives without a word.
USER's call, and the framing that settled it: *"those are not attribute
phrases … it's cheaper to parse more than it is to llm turn."* `|`/`&` now end
a value only when what FOLLOWS is another attribute test or a group, so
`[name=a|name=b]` is boolean and `[name~=a|b]` is one regex. Both readings
survive; the tax is gone.
- **and the reading RENDERS the decision** (USER's other half: "we render an
  explain for a query right? … That should clear it up"). It had to stop
  reading `c.attrs`, which holds only top-level conjuncts and therefore
  rendered NOTHING for an OR — the one shape where seeing the parse matters.
- **measured cost of the round trip**: ~6 errors and one wrong answer over two
  days, against a feature with no users. The lesson is in the adoption slice:
  ship, scan, keep or revert — the scan is what caught it.

✅ **Budget semantics: charge the real work, scale with the corpus, stop on
breadth — 2026-08-04 (USER).** Asking what the budget's semantics WERE turned
up three defects at once.
- **It did not bind.** `spend(1)` was charged per matched SITE while the
  expensive work (`declsOf`/`LookupExisting`) is per NODE and was free, so a
  wide query burned seconds while barely spending and the clock — sampled
  every 256th spend — was never consulted. Measured against a 2.3s tree build:
  a 1ms budget took 5.06s, a 1000ms budget 23.33s. Charging before the lookup:
  1.91s and 3.43s.
- **A fixed default measures the wrong thing.** Work scales with the corpus
  AND the tree build comes out of the same budget and also grows, so a flat
  10s shrinks in the only term that matters as a workspace grows. Now 10s
  floor + 1s per 1,000 indexed names, clamped to the SAME 30s ceiling an
  explicit budget already had.
- **Nothing curtailed BREADTH, and the exploding shape is the one the planner
  skips**: `planReorder` is sound only for pure-descendant chains and excludes
  edges/`{m,n}`/`>`/`*`; `estCard` has no figure for an edge. The guard
  therefore MEASURES rather than estimates — expand 64 tips, project the rate,
  stop when it cannot fit — and names the tip count, since "the clock ran out"
  is true of every blown query. 23.0s → 6.0s on `::in.call > *`.
- **risks**: the guard needs 64 tips to sample, so it never applies to a small
  workspace (its test builds a 400-symbol one rather than skipping). The
  scaled default means a big corpus can now legitimately spend 30s.

✅ **Merge conflicts are first-class — 2026-08-03/04.** A conflicted file was
the one input where the index was CONFIDENTLY wrong: tree-sitter recovers past
the markers, so both sides landed in the symbol table as peers and a conflict
crossing a function boundary produced `func B[1]` — ours' header, a `=======`,
theirs' tail. A declaration from no commit and no branch, read back without a
warning and editable. Full forensics in `bugs.md`. The tree, in the order it
was built: `accept:"mine"|"theirs"` (whole file or one block by its span);
`::conflict` / `::mine` / `::theirs` / `::base` as nodes, sides carrying the
REF git wrote; a refusal on any write STRADDLING a marker; whole-file
reconstruction of each side with `symbols.ParsesCleanly` as the verdict and a
diffed-TEXT fallback when neither parses; and finally withholding the phantoms
from query results, which also repairs the ordinals of the real symbols around
them.
- **decided**: provenance is PRIMARY, not decoration. Under a rebase "ours" is
  the upstream being replayed onto and "theirs" is your own commit — the
  conflict that prompted this was exactly that shape — so sides report the ref
  and the grammar says trust the ref, not the position.
- **decided**: side declarations are names + classes, NEVER addresses. A symbol
  found in a reconstruction has reconstructed-coordinate spans while the file
  on disk still holds markers, so an address minted there would resolve against
  different bytes than it was computed from.
- **risks**: UNMEASURED in the wild. No conflict occurred in the three sessions
  scanned on 08-04, so every part of this has passed tests and one scripted
  live run and nothing else.
- **next**: ◻ `::mine func` still does not QUERY the reconstructed side — the
  trees are built and parsed, only their verdict and declaration list surface.
  ◻ No workspace-scoped resolve: a 30-file merge is 30 calls, when `accept` is
  by definition uniform.

✅ **A conflict announces itself, out of band — 2026-08-04.** Three repos, and
the keystone was missing rather than hard: mcp-go's client has always supported
`OnNotification`; agentkit's `mcpmgr` never wired it, and dun built AROUND the
gap (sub-agents are in-process purely so a child could notify a parent).
poly-lsp declares `logging` and pushes on the conflict TRANSITION; `mcpmgr`
grew `SetNotificationHandler`; dun lifts `notifications/message` into the
conversation via `Notify`, so it lands inside the next tool result. Verified
live end to end: the model read the notification, ran the selector it
suggested, and offered both resolutions by branch name.
- **note**: this makes dun's in-process sub-agent transport optional rather
  than necessary. Not changed — it works, and the reason to revisit it is now a
  different one.
- **risks**: `mcpmgr` is shared infrastructure, so every MCP server behind dun
  can now push. Nothing rate-limits or de-duplicates a chatty server.

✅ **A zero-result query says which clause emptied it — 2026-08-02** →
done.md. `mcp/query_hint.go`: `returned == 0` attaches a one-line `hint`
naming ONE alternative, from six probes tried in order of certainty — a dead
`[path]` filter, a dead chain PREFIX (`#Nope::in` no longer reads as "unused"),
drop-the-tag (the `method name~=newInputStream` → `func` case that motivated
it), the edge form of the same guess (wrong KIND class), a near-miss name
verified against the tree, and drop-the-last-attribute as the general form.
Edge selectors included: the limit was lifted the same day (USER). Measured at
0.03%–1.9% of the query it explains, `lspAsked == 0`.
- **risks**: probes reuse the LIVE engine, so a future field the evaluator
  writes and the payload reads must be added to `probeState` or a probe's
  budget blow will report the caller's own complete query as truncated.
  `TestProbeLeavesEngineUntouched` compares the whole struct, which catches
  the omission only once the new field is IN the struct. Likewise a future
  child-LSP path reached during a probe must call `probeBlocked()`; `lspLeft
  = 0` stops the round-trip either way, but without the mark the probe would
  report a degraded answer as a confident one.
- **assumption, recorded**: a hint's cost ceiling is derived from
  `time.Since(e.startedAt)`, i.e. tree build + evaluation — what the caller
  actually waited for. Overshoot is bounded by one tree-sitter parse, which is
  not interruptible.

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
- **next**: nothing pending — both first-run violations are fixed (below) and
  the matrix carries no KNOWN entries. Standing rule, not a task: add a class
  whenever a bug turns out to be language-shaped.
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
- **sql is FIXED too** (USER, 2026-08-01): two separate causes, not one.
  tree-sitter-sql calls a `--` line a `comment` but a `/* … */` block
  `marginalia`, which `isCommentNode` did not know — so a sql block comment
  was not a comment anywhere in the codebase. And every CREATE is wrapped in a
  `statement` node with the doc comment as the WRAPPER's sibling, two levels
  up from the symbol. Both classes are now `ok` in every language that has a
  symbol grammar; the matrix carries no KNOWN entries.


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
    (The `;` sitting OUTSIDE the statement node is handled — `terminatorEnd`
    absorbs it, so deleting a table no longer leaves a bare `;`.)
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
**We own what we measure — 2026-08-02 (USER).** The earlier "the bench NO
LONGER LIVES HERE" was wrong in one direction: the HARNESS is corrallm's, but
the probes were not. A probe asking "did the model reach for `node_query`" is a
statement about poly-lsp's behaviour, and it was living in corrallm — reviewed
by people with no reason to care, with `find-render-entrypoints` carrying a
snapshot of our own `mcp` package as its fixture. USER's framing: llm-bench was
made external precisely because what we want to test has no right to belong to
corrallm.
- **`bench/` is now ours**: `bench/llm-bench.yaml` (the seven `polylsp*`
  toolsets + `full`) and `bench/probes/` (the four net-benefit probes). corrallm
  keeps the harness — runner, scoring, judge, journal, report, queue-wait
  subtraction — plus `baseline`/`mcpshell` and the probes that measure MODELS.
  Run: `llm-bench run --config bench/llm-bench.yaml`.
- **Directory REFERENCES, not copies** (corrallm `9f559cb`): `probeDirs` in a
  bench config, and `--tasks-dir` / `--bench-probes`, take a LIST. A box names
  `~/…/poly-lsp-mcp/bench/probes` once in `<corrallm-home>/llm-bench.yaml`;
  edits here change what it runs, no restart, nothing copied. Relative entries
  anchor to the config file, not the process cwd. Replace-not-merge is
  unchanged and deliberate — naming dirs means those probes and no others.
- **Two defects fell out of the move.** corrallm's `MaterializeBuiltins` never
  pruned its long-lived temp extraction, so a probe deleted from the library
  kept running on every box that had ever benched (library said 16, runner ran
  20) — fixed in `bf4fea7`. And our own index swallowed the new fixture: a
  pinned snapshot of `mcp/*.go` made `func name=handleModernNodeQuery` return
  two matches, the `.dun`-worktree failure from the other direction.
  `skipScanDir` now skips `_fixture` — narrow on purpose, since Jekyll's
  `_posts` is real content.
- **risks**: the `find-render-entrypoints` fixture is a pinned snapshot and
  will drift from the real `mcp` package. Refreshing it moves the numbers, so
  it has to be its own change, not a drive-by.
**next**:
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
  - ✅ **Transcript scan replaces the bench for surface changes — 2026-08-04.**
    Reading real dun sessions answers what the bench structurally cannot,
    because it measures the model that actually ran, on real work, against the
    surface as shipped. Three sessions after the 08-03 changes, 151 `node_*`
    calls:
    - **The read `outline` works.** Paging hints fell 36% → **16%** of reads,
      and the "read it whole in one call" escape (`lineLimit`) was used **5
      times after never being used once**. Not controlled — different work —
      but the lineLimit shift has no other explanation.
    - **The space change broke nothing.** ZERO selectors used the old attach
      form; 21 used the bracketed filter. A breaking grammar change with no
      observable breakage.
    - **The booleans are unused and costly** — see the ⏸ slice at the top.
    - Untested in the wild: every conflict feature (no conflict occurred), and
      `hop`/multi-hop walks (0 uses; `via` twice).
    **Method**: pair `tool_call`/`tool_result` out of `~/.dun/sessions/*.jsonl`
    and count both the ERRORS and the corrective NOTES, which is where a
    surface tells on itself. This is now the loop for any description or
    grammar change: ship, scan the next sessions, keep or revert. It has paid
    for itself once already — the `|` revert above.
  - ✅ **Recipe sweep — run what the description advertises (2026-08-04).**
    A second instrument, aimed at the surface rather than at sessions: run
    every recipe and read the OUTPUT, not the exit code. Four defects have now
    come from reading real output that tests passed over, because a test
    asserts what the code DOES and the output is where you see what it SAYS —
    the `file@line` page-1 read, the `|` semantics, `{m,n}` reading as
    containment on an edge walk, and `totalMatches: 0` for a walk that never
    finished. Two suspicions were also WRONG and worth recording as method:
    `func:recursive` returning 0 from the CLI is correct and says why (it
    needs an LSP to tell a self-call from a name collision), and a doubled
    space in a `::grep` repair was an artifact of my own display filter. Twice
    the "bug" was a grep of my own output.
  - ❗ **The bench structurally CANNOT answer the absolute adoption question.**
    In-harness, grep is just another advertised tool with a one-line desc — it
    has NO home-field advantage. The icebox's "0 calls" was grep as the model's
    NATIVE, reflexively-trained tool inside a real agent. This bench is NEUTRAL
    ground, where node_query competes as a peer and wins on description alone.
    So: node_query is *competitive as a peer* (real result), but "does it beat
    the model's native grep/read in a real agent" needs a REAL agent — and the
    one we had wired (autowork3) is a DEAD PROJECT (dropped 2026-07-21, user
    call). llm-bench's standing value is A/B descriptions as peers,
    correctness, cost.
  - ✅ **The vehicle question is RESOLVED — 2026-08-01 (USER): `dun` +
    `agentkit`.** dun is a real agent with its OWN native tools, so grep/read
    carry the reflexive home-field advantage llm-bench-mcp's advertised
    `read_file` never had. corrallm's llm-bench stays the harness and
    measurement lib; dun is the SUBJECT. Nothing in either repo mentions the
    other yet (`grep -ri dun` over corrallm's bench + probes: zero hits), so
    the wiring is unbuilt — that is the work, not the decision.
  - ◻ **Wire dun as an llm-bench subject.** The axis that matters is
    dun-with-poly-lsp vs dun-without, on the same probes, scoring whether
    node_query is reached AT ALL while the model's native grep/read stay
    present and untouched. corrallm's `cedeFileTools` switch is the existing
    near-miss: it REMOVES the alternative, which is the thing already known to
    force adoption. The finding lives in not removing it.
    - **Where it lives — decided 2026-08-02 (USER): a GENERIC external-agent
      subject in corrallm**, `subject: {cmd, args, protocol: jsonl}`. Driving an
      external agent is a benching capability in a benching tool; the dun-ness
      lives entirely in `bench/llm-bench.yaml` here. The test that keeps it
      honest: no string in corrallm may say `dun` or `poly-lsp`. (Rationale
      comments recording why a capability exists are exempt — that is history,
      and `llm-bench-mcp`'s `--file-tools` note is worse without its motivating
      case.)
    - **Prerequisite, unverified**: whether dun's `-p` line-delimited JSON
      events expose enough (tool name, args, result) to compute reach /
      graph-first / grep-free. Everything downstream is blocked on that answer;
      if they don't, the work is a dun change first.
    - **The hooks that already exist** (checked 2026-08-02): `dun "task"` runs
      headless and exits; `-p` emits/reads JSON events; `--url/--model/--key`
      aim it at corrallm, so queue-wait subtraction and per-model comparison
      keep working; poly-lsp is already a dun BUILT-IN (`id: code`, opt-in
      `autostart`) and dun's config already uses `{{workspace}}`, "the same
      token llm-bench toolsets use". So the with/without arm is a dun config
      line, not new plumbing.
    - **What has to be built**: `runOne` is ~380 lines with the mcpmgr Manager,
      tool surface, `agent.Session`, dispatcher wrapper and stage loop all
      inline — there is no subject seam to hang this on. And two controlled
      variables are lost: `MaxToolCallsPerStage`/`MaxTurnsPerStage` and the
      agentkit Shaper context budget are Session-level, but dun owns its own
      compaction, so "the same context budget for every model" stops holding
      unless dun exposes a knob.
    - **risks**: both arms must run the same dun revision, or the diff measures
      dun's churn instead of poly-lsp. `check.EvaluateAll` runs against the
      scratch workspace, so pass/fail ports across unchanged.
  - ✅ **The exclusive lease is GONE from corrallm — 2026-08-01 (USER's call:
    "no longer needed and is an anti-pattern").** A bench run is now an
    ordinary caller: no lease, no eviction, nobody turned away. Rationale, in
    the USER's framing: exclusivity existed to separate model time from queue
    time, and the bench already measures queue wait directly (`OnRetry` →
    `stageQueueWait`) and subtracts it — so the outage bought nothing.
    Removed: `internal/proxy/calibration.go` + its two proxy hooks and
    `X-Corrallm-Calibrating`; the three `/api/v1/calibrate/*` routes;
    `--exclusive` through CLI → API → runner → UI; and the bench's
    `Unload`/`UnloadAll` calls. Kept: `/api/v1/models/{load,unload,unload-all}`
    for operators (USER: "we still want to be able to programatically unload
    models"), and `ModeWarm`, since a `Load` takes nothing from anyone.
    `RunMode` and its store column survive, so no schema migration.
    Verified: `go build ./...`, `go test ./...` (exit 0), `llm-bench validate`
    (20 probes, 0 invalid), `make gen` regenerated the SDL, `pnpm build`
    typechecks. Pre-existing lint/gofmt debt confirmed pre-existing by
    stash-diff and left alone.
    - **the cost, recorded so it is not re-derived**: `run: cold|both` is gone,
      so `capability-vision` is warm-only and the cold-path bug class it was
      written for (2026-07-18: `ternary-bonsai-27b` dropped an attached image
      on the first request after a cold load while `/props` said
      `vision: true`, warm was fine) is **undetectable by the bench**. Flagged
      before cutting; USER chose to drop cold anyway. Re-testing that path
      needs a mechanism that does not evict on a shared box — observe the
      first request after a load someone ELSE caused, rather than arranging
      one. Written up in `probes/README.md` § "What losing cold mode costs".
  - ◻ **Run policy on a contested box (USER, 2026-08-01): low priority,
    generous 429 retry.** Retry is ALREADY right — `internal/bench/run/
    client.go` sets `RetryBudget = -1` (unbounded, Retry-After-honouring, 429
    only; 5xx stays bounded) and `OnRetry` accrues the wait so it is
    subtracted back out of probe timings. Priority is NOT: corrallm.yaml's
    `keys:` maps only `aw3→interactive` and `ragtag→batch`, so a bench run
    falls through to the `default` group. Fix is in corrallm, not here: map a
    bench token to `batch` and point the bench config's `apiKeyEnv` at it.
    **`reject` is NOT a drop** (assistant got this wrong first pass, USER
    corrected 2026-08-01): it returns a `BackpressureError` → 429 +
    Retry-After + capacity/inflight/waiting headers, `429 not 503`
    deliberately, and `Retry-After = ceil((waiting+1)/capacity) × dwellEWMA`
    (floor 1s) is a real queue-position estimate. A default-group run gets
    served; it retries into it. The two properties that DO argue for `batch`:
    - `interruptible: true` — interactive can PREEMPT a running batch request
      mid-flight (`pickVictim` → `victim.cancel(ErrPreempted)`). `default`
      omits the flag, so a default-group bench run holds its slot AGAINST
      interactive traffic. **But interruptible is WRONG for the bench as
      wired** (USER asked, verified 2026-08-01): preemption cancels reqCtx
      MID-GENERATION, and agentkit's retry covers only "up to and including
      response headers — a stream that dies mid-generation is not resumable".
      `llm.TransientUpstream` would classify it as infrastructure, but it has
      NO CALLERS in agentkit or the bench, so `run.go` turns it into
      `failedRows(Pass:false)` — an interactive user scores as the model
      failing the task. That is verbatim the incident `transient.go`'s doc
      comment was written about. So prefer a `bench` group: weight 1,
      `local: {queue: true}`, `default: reject`, **`interruptible: false`** —
      yields by queueing without destroying in-flight work. Cost, stated
      plainly: with no interruptible victim `pickVictim` returns nil and
      interactive takes `then: fallThrough`, spilling to claude at `$20/hr`.
      Money instead of destroyed bench work — **USER's call.** Reusing the
      existing `batch` group is not an option: `ragtag` shares it.
    - `local: {queue: true}` — a `reject` caller is never appended to
      `bs.waiters`, so it holds no position and its retries RACE for the freed
      slot; a queued caller blocks server-side and `promote` picks it by
      preempt-priority then weighted fairshare. Plus `dwell: 600s/min` caps
      consumption.
    Note the backoff hint is NOT lowered by being next in line —
    `backpressure()` reads the backend's waiter count only, so weight-1 batch
    and weight-10 interactive get the SAME Retry-After for the same state.
    Priority decides promotion off the queue, not the hint. And for a reject
    caller `waiting` counts only server-side waiters, so a saturated box with
    nobody queued hands back ~one service time — an under-estimate under a
    retry storm.
    **Weight is proportional, not a reservation** (USER asked, verified
    2026-08-01): `pickGrantableWaiter` picks `min(numerator/weight)`, so
    repeated picks settle interactive at ~10× batch's CONCURRENT slots — but
    only under sustained contention with both queued, it is work-conserving
    (idle interactive ⇒ batch takes every slot), and it needs enough physical
    slots to express (capacity 1 degrades it to a temporal share). Weight is
    consulted ONLY among waiters, so a `default`-group caller's weight is
    inert — it never enters `bs.waiters` at all. The real guarantee primitive
    is `reservation.go`, keyed by LANE not weight (`effCapLocked = capacity −
    reservedByOthers`, 5m TTL + heartbeat); nothing in corrallm.yaml reserves
    anything today. And `numerator` is REQUEST COUNT (no group sets
    `shareCurrency`), so the 10:1 is over concurrent requests, not GPU-seconds
    — relevant when reading bench wall-clock. `dwell: 600s/min` is a separate
    consumption limit, not the share currency.
  - ◻ **Collision canary** (`collision*`): grep AND lexical node_query both
    return the merged set (verified) — flips to a graph win only when the LSP
    precision pass resolves the site. Re-run with precision ON to show the
    graph's real differentiator.
**Do NOT rewrite `modern.go`'s description** — across quick + in-flow, two
sets × three variants, the shipped spec-first desc already saturates reach and
graph-first. The wording is not the bottleneck on neutral ground; if `inspired`
taught anything it's that MORE prose is worse, not better.
**blocking decision — CLOSED 2026-08-01.** The vehicle is dun/agentkit driven
by corrallm's llm-bench; the roadmap does not have to ride on the peer result.
- **risks**: dun's tool surface and prompt are moving targets, so a
  with/without-poly-lsp diff is only honest if both arms run the same dun
  revision. And on a contested box an un-queued (`default`-group) run retries
  into a race rather than holding a fairshare position, so its wall clock is a
  measurement of luck — the bench subtracts 429 waits, but not the extra
  round-trips a lost race costs.
- **blocking decision (USER owns)**: low-priority for the bench means a NEW
  `bench` group (queue + weight 1 + `interruptible: false`), not `batch` —
  preemption destroys in-flight generation that nothing retries or labels. The
  price is interactive spilling to claude (`$20/hr`) instead. Pick one.
- **latent bug found on the way, corrallm-side, worth filing there**:
  `llm.TransientUpstream` / `TransientUpstreamReason` are fully written and
  entirely UNCALLED, so every mid-stream upstream fault (deploy, restart,
  preempt) still scores as a model failure — the exact regression the function
  was written to prevent.

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
