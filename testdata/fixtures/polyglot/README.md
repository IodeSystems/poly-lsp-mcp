# polyglot

Three services share a `UserID` identifier:

- Go: `main.go` defines `UserID` and `GreetUser`.
- TypeScript: `client.ts` re-declares `UserID` and fetches users.
- Python: `worker.py` re-declares `UserID` and processes them.

Cross-language references to `UserID` also appear in `config.yaml` and
`package.json` as string-literal config values — those are the hard cases
for tree-sitter-driven rename.
