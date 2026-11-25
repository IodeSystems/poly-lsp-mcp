# Test plan: st_1m7cmcxh5eqd0xgt6gyx05

## Re-measure two worktrees of one repo under the fleet cap (issue i_1m5r2t7k3dczk49mtwctmv spike)

Runs `TestMeasureWorktreeSharingFleet` (`-tags measure`): two roots of one git repo (the repo + a detached worktree) in one daemon Registry, with the child-LSP fleet at the daemon's defaults (MaxChildren 4, RSSBudgetPct 25, IdleStop 10m). Records fleet status (running count, summed RSS vs budget) after each root's resolved query, per-gopls RSS, the second worktree's open time, and the cost of re-querying the first root (i.e. gopls wake cost if the fleet stopped it).

```sh produces=fleet-measure.log inputs=daemon/measure_test.go
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
go test ./daemon/ -tags measure -run 'TestMeasureWorktreeSharingFleet' -v -count=1 -timeout 600s 2>&1 | tee "$OUT/fleet-measure.log"
```
