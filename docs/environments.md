# Environments

Requirements a dev environment must meet before automated work on this
project is safe. Each section is a spec: it states the property, the
mechanism that satisfies it, and the test that attests it.

## 2b — Two worktrees must not collide

Two worktrees of this project can run the test suite at once and both
pass. Capacity above 1 is a claim until this is attested by a recorded
run of the proof test.

### The collision

Two worktrees of one user, run at once, share `os.TempDir()` (default
`/tmp`). `TestTempDirRemovedOnFailedRun` in `server/` and `multiplex/`
scans the whole temp dir for prefixed directories
(`poly-lsp-mcp-srv-test-*`, `poly-lsp-mcp-mp-test-*`), failing if any
new such directory appears during the run. A second worktree creates its
own prefixed directory, which the first run sees and counts as "left
behind." The Go build cache (`GOCACHE`) is shared too, but Go makes it
safe under concurrent access (content-addressed, file-locked), so it is
not the breaker.

### The fix

Each run gets a distinct `TMPDIR`. This is what the per-session TMPDIR
isolation installs for every aw4-spawned process; setting `TMPDIR`
per run has the same effect and is what the proof test exercises.

### Proof test

`internal/isolation/isolation_test.go` — `TestTwoConcurrentTempDirScanRuns`.
It spawns two concurrent `go test -run TestTempDirRemovedOnFailedRun`
subprocesses of the `server` and `multiplex` packages:

- **Positive control:** each run gets its own `TMPDIR`. Both pass.
- **Negative control:** both runs share one `TMPDIR`. At least one
  collides with "left a temp dir behind", proving the collision is real
  and that distinct `TMPDIR` is what removes it.

Run it with:

    go test ./internal/isolation/ -run TestTwoConcurrentTempDirScanRuns -v
