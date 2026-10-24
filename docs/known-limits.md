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
- **Pure-Go runtime** (`CGO_ENABLED=0`): same grammars, about 10x slower to
  parse; on valid kotlin / tsx / c, some GLR ambiguity picks differ from C.
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
