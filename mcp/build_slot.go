package mcp

// BuildSlots bounds how many index builds run at once across the servers
// that share it (the daemon registry owns one). nil = unbounded.
type BuildSlots chan struct{}

// NewBuildSlots returns a semaphore of n slots; n < 1 is 1.
func NewBuildSlots(n int) BuildSlots {
	return make(BuildSlots, max(n, 1))
}

// SetBuildSlots makes this server's index builds take a slot of sem first.
func (s *Server) SetBuildSlots(sem BuildSlots) { s.buildSem = sem }

// runBuild runs f holding a build slot, on its own OS thread at lowered
// priority. The wait for a slot happens before f, so f's own clocks (the
// index budget) count build time only.
//
// f runs on a dedicated goroutine that exits while locked to its thread: Go
// then discards the thread, so the lowered priority never leaks into the
// pool. Restoring is not possible unprivileged on Linux (RLIMIT_NICE forbids
// lowering a nice value). Only that one thread is niced; goroutines f starts
// run on other, normal-priority threads (the build starts none today).
func (s *Server) runBuild(f func() error) error {
	if s.buildSem != nil {
		s.buildSem <- struct{}{}
		defer func() { <-s.buildSem }()
	}
	done := make(chan error, 1)
	go func() {
		lowerThreadPriority() // locks the thread; no unlock on purpose
		done <- f()
	}()
	return <-done
}
