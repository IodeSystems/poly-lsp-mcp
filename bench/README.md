# bench — poly-lsp measures itself

We own what we measure. corrallm's `llm-bench` is the HARNESS — runner, scoring,
judge, journal, report, 429 queue-wait subtraction, model iteration — and it
knows nothing about poly-lsp. This directory is the poly-lsp half: the probes,
their fixtures, and the toolsets that name our binary.

The split is not tidiness. A probe that asks "did the model reach for
`node_query`" is a statement about poly-lsp's behaviour, and it has to live in
the repo whose behaviour it describes, or it drifts: the tool changes here and
the probe that judges it changes somewhere else, reviewed by someone with no
reason to care.

## Running

```
llm-bench run --config bench/llm-bench.yaml
```

`probeDirs: [./probes]` in the config points at this directory, so `--tasks-dir`
is optional. Pass it to narrow to a subset for a one-off; it overrides
`probeDirs` outright rather than adding to it.

Both binaries must be on `$PATH`: `poly-lsp-mcp` (this repo) and `llm-bench`
plus its `llm-bench-mcp` helper (corrallm).

## Registering with a corrallm box

To have UI-driven runs pick these up, name the directory once in the box's
`<corrallm-home>/llm-bench.yaml`:

```yaml
probeDirs:
  - ~/local/src/iodesystems/poly-lsp-mcp/bench/probes
```

It is resolved fresh on every run and every catalog read, so editing a probe
here changes what that box runs — no restart, nothing copied. Naming any
directory replaces corrallm's built-in library, which is deliberate: you get
these probes and no others, so a malformed built-in cannot fail a run that never
asked for it.

## Probes

| probe | what it isolates |
| --- | --- |
| `codebase-navigation` | read-only structural questions with exact answers — the reference graph against grep |
| `find-render-entrypoints` | a "where does this text come from?" trace over a REAL codebase, not a toy fixture |
| `cross-language-rename` | a rename whose call sites cross go/ts/yaml — the cross-language index |
| `multi-file-refactor` | a signature change that has to land in every caller at once |

`_fixture/` holds each probe's seed workspace. The `_` prefix keeps Go tooling
out (`./...` never descends into it) and `gomod.probe` is a renamed `go.mod` so
a fixture is not a nested module until the harness materializes it.

`find-render-entrypoints`'s fixture is a PINNED SNAPSHOT of this repo's own
`mcp` package, not a live reference. A probe has to be reproducible, and a
fixture that tracked the working tree would score differently every commit.
Refresh it deliberately, as its own change, when the snapshot gets old enough to
stop resembling the code — and expect the numbers to move when you do.

## What is NOT here

The harness, and the probes that measure MODELS rather than tools —
`capability-*`, `adversarial-*`, `compaction-continuation`, `edit-safety-*`,
`fix-failing-test`. Those are corrallm's: a gateway benchmarking the models it
serves. `baseline` and `mcpshell` stay there too, as the neutral toolsets any
probe can use.
