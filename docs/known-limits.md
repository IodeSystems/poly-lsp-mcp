# Known limits

Limits that are accepted, recorded so they are not re-investigated. A limit
someone intends to fix is an aw4 issue in project `poly-lsp-mcp`, not an
entry here.

## Languages

- **SQL:** `CREATE PROCEDURE` is unparseable by tree-sitter-sql (such files
  index empty). Quoted identifiers keep their quotes
  (`"USER"."USER_pkey"`). index / view / type / trigger / sequence /
  constraint all have class `type`, so a selector cannot ask for views
  specifically. tree-sitter-sql calls `/* ... */` `marginalia`;
  `isCommentNode` knows it. The `;` outside the statement node is absorbed
  (`terminatorEnd`).
- **Go:** of the four grouped declaration forms, only `import` and `var`
  parse through a list node (`import_spec_list`, `var_spec_list`); `const`
  and `type` specs have no wrapper even when grouped. A one-spec group takes
  the whole declaration as its span.
- **Markdown** keeps inline backtick spans in the index (dropping them breaks
  prose rename and two polyglot tests). Reversible in one line.
- **TypeScript** arrow components (`const Foo = () => ...`) get no
  `.argument` / `.return` children: 9 of 331 declarators (3%) on a real
  React frontend.
- **JVM** languages (java, kotlin, groovy) are tree-sitter only: their
  servers want a build. Groovy has no corpus here; a Jenkinsfile cannot be
  routed (extensionless, the registry keys on extension).
- **Swift:** the grammar is smacker's pin of tree-sitter-swift, older than
  Swift 6 syntax: of 150 Swift files on this machine (vendored expo
  modules), 26 parse with an ERROR (raw identifiers like
  ``func `does a thing`()``, `#expect` macros). ERROR nodes are walked
  through, as in kotlin, so members survive and some locals surface as
  members. `let a = 1, b = 2` and `case a, b` give each name its own
  node spanned by the name alone. No signature refactor: call sites carry
  argument labels. sourcekit-lsp publishes a just-opened file about 1 s
  after the open with no semantic errors (build settings not loaded), then
  again 1-3 s later; an edit skips the first and takes the second, so the
  first edit of a cold package needs more than the 2 s `--validate-wait`
  default and is kept, reported `validationSkipped: lsp-timeout`. A clean
  result published while sourcekit-lsp reports progress (package loading,
  background indexing) is not taken either: a clean edit made while it
  indexes is reported `lsp-timeout` when indexing outlasts the wait (a
  large package's first index).
- **Edit validation, servers without document versions** (sourcekit-lsp,
  typescript-language-server 5.1.3): a clean result never validates an
  edit. It is kept and reported `validated:false`,
  `validationSkipped: lsp-unconfirmed`; a new error still reverts it. Such
  a server can publish a clean result for text it has not analysed in full
  and nothing says so (sourcekit-lsp before its build settings load).
  gopls, clangd and pylsp version their publishes and validate.
  `trust_unversioned_diagnostics: true` in a language's `lsp` block lets
  that server's clean result validate; it is off for every default server.
- **typescript-language-server 5.1.3 is not trusted** (measured
  2026-10-27, Linux, normal and with the child under a 5% CPU quota):
  - It publishes nothing for a file whose diagnostics stay empty (it keeps
    syntax, semantic and suggestion diagnostics per kind and skips an
    empty-to-empty update). A clean edit to a file with no diagnostics,
    and an edit that breaks only another file, wait out
    `--validate-wait` and report `lsp-timeout`.
  - The first publish of a just-opened file can hold syntax diagnostics
    only: under the quota it published [] and the semantic result 346 ms
    later. The skipped first publish after an open covers it; when the
    server is fast it sends one complete publish instead, so an edit made
    before the open's publish waits out the cap (`lsp-timeout`, kept).
  - It coalesces a revert's publish with the next change. The next
    edit's error baseline then still holds the reverted error, and the
    same broken edit sent again at once was reported `validated:true`
    with trust on. gopls does the same (next item), so trusting it would
    add a false `validated` path, not the only one.
- **Edit validation, an edit retried right after its revert:** the error
  baseline is read from the diagnostics store when the edit starts; the
  reverted text's publish may not have arrived yet, so the reverted
  error counts as pre-existing and the identical broken edit is kept and
  reported `validated:true`. gopls 0.24: 5 of 5 retries (2026-10-27).
- **Edit validation, servers without document versions, opens:** the first
  publish after a file's didOpen is never taken as an edit's result (see
  design.md "Editing"). A server that publishes once per open (pylsp,
  untested here) therefore validates no edit that itself opens a file
  (a new file, or one the proactive open skipped): it waits the full
  `--validate-wait` and reports `lsp-timeout`.
- **Pure-Go runtime** (`CGO_ENABLED=0`): same grammars, about 10x slower to
  parse; on valid kotlin / tsx / c / swift, some GLR ambiguity picks differ
  from C (swift: 125 of 126 cleanly-parsing files give the same symbols).
  `doctor` reports `runtime`.

## Android

- `R.<type>.<name>` resolves lexically. Distinctive ids answer cleanly; a
  common-word id (`messages`, `gguf` on llama.android) answers with every
  same-named identifier. Resource types collapse (`R.id.x` cannot choose
  between `@+id/x` and a same-named `<string>`).
- The resource binding reads direct string literals only (no constant
  folding).

## Query surface

- The query CLI is load-only on the parse cache (it never writes it) and,
  without `--resolve`, edges are lexical; it says so in a footer when a
  selector uses `::in` / `::out`.
- `::signature` / `::body` are line-granular: on a single-line signature the
  `{` line is in both halves.
- `:where(sel)` is the same as `:any(sel)` at tip granularity; `:all` over a
  path union cannot say "there exists a path whose every hop matches".
- Unbounded `{m,}` collects nodes at their shortest hop. Transitive walks
  spend the per-query LSP cap shallowest-first, so deep hops fall back to
  name-keying and are labelled.
- Legacy `node_read` with the v0.2 alias `limit` on a `node` address errors
  naming `lineLimit` / `lineLength`, parameters the caller never typed.
- `delete:true` on a single-line `file@N` empties the line and leaves a blank
  line (`TestRefSiteAddrDeleteEmptiesTheLineNotTheFile`).
- `lostWrites` is one file, syntactic assignments only: no aliasing, no
  pointer writes, no cross-file resolution. A declaration is not a read, so
  removing the write and every reader together is silent.

## Probes and hints

- Zero-result probes reuse the live engine. A new field the evaluator writes
  and the payload reads must be added to `probeState`, or a probe's budget
  blow reports the caller's complete query as truncated
  (`TestProbeLeavesEngineUntouched` catches it only once the field is in the
  struct). A child-LSP path reached during a probe must call
  `probeBlocked()`.
- A hint's cost ceiling derives from `time.Since(e.startedAt)` (tree build +
  evaluation). Overshoot is bounded by one tree-sitter parse, which is not
  interruptible.

## Indexing and daemon

- The home check uses `$HOME`, so a dotfiles repository at `~` is refused
  (`--index-anyway` overrides).
- Files a followed symlink reaches are indexed under the link's path (a link
  into `node_modules` indexes it). A gitignored link target is left out as
  `git-ignored`.
- Two clients on one root with different index options rebuild it on each
  other's opens (one index per root, last open wins).
- A root's grace stop can race a returning client; its child LSP restarts on
  demand.
- Each git worktree of one repository gets its own child LSP in the daemon
  (measured 0.8-2 GB RSS per gopls).
