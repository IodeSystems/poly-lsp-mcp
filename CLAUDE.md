# poly-lsp-mcp

## Work items

Work items are aw4 issues in project poly-lsp-mcp. This repo has no plan/
directory; this overrides the global Planning rule.

- File a finding, a next step or a decision only Carl can make with
  `aw4 issue ask "<one line>" --body "$(cat <file>)" --project poly-lsp-mcp --by <you>`,
  run from the aw4 checkout. Write the body to a file with a quoted heredoc
  (`<<'EOF'`); an inline body loses anything in backticks.
- Design reasoning (decisions, conventions, measured trade-offs) goes to
  `docs/design.md`; accepted limits to `docs/known-limits.md`; how surface
  changes are judged to `docs/evaluation.md`.
- The MCP tool schemas (`mcp/modern.go`) are the source of truth for the
  tool surface. Do not copy them into docs.
- Pasted external reviews are not committed.
