package mcp

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBuildSlotsOneAtATime(t *testing.T) {
	sem := NewBuildSlots(1)
	var running, peak atomic.Int32
	var wg sync.WaitGroup
	for range 3 {
		s := New(nil, "", nil, nil)
		s.SetBuildSlots(sem)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.runBuild(func() error {
				n := running.Add(1)
				for {
					p := peak.Load()
					if n <= p || peak.CompareAndSwap(p, n) {
						break
					}
				}
				time.Sleep(30 * time.Millisecond)
				running.Add(-1)
				return nil
			})
		}()
	}
	wg.Wait()
	if peak.Load() != 1 {
		t.Fatalf("peak concurrent builds = %d, want 1", peak.Load())
	}
}
