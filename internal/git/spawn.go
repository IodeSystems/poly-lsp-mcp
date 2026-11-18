package git

import (
	"os"
	"time"
)

// SpawnHook, if set, is called after every git subprocess this package
// runs, with the repository root it ran in, its wall time, and its user
// plus system CPU time (zero when the process did not start). The mcp
// package sets it once, at init, to count git spawns per workspace root.
var SpawnHook func(root string, wall, cpu time.Duration)

// noteSpawn reports one finished git run to SpawnHook.
func noteSpawn(root string, start time.Time, ps *os.ProcessState) {
	if SpawnHook == nil {
		return
	}
	var cpu time.Duration
	if ps != nil {
		cpu = ps.UserTime() + ps.SystemTime()
	}
	SpawnHook(root, time.Since(start), cpu)
}
