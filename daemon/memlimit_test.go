package daemon

import (
	"runtime/debug"
	"testing"
)

func TestApplyMemoryLimit(t *testing.T) {
	prev := debug.SetMemoryLimit(-1)
	defer debug.SetMemoryLimit(prev)

	t.Setenv("GOMEMLIMIT", "")
	applyMemoryLimit(64)
	if got := debug.SetMemoryLimit(-1); got != 64<<20 {
		t.Fatalf("limit = %d, want %d", got, int64(64<<20))
	}

	// $GOMEMLIMIT set: left alone.
	t.Setenv("GOMEMLIMIT", "1GiB")
	applyMemoryLimit(32)
	if got := debug.SetMemoryLimit(-1); got != 64<<20 {
		t.Fatalf("limit changed to %d with $GOMEMLIMIT set", got)
	}
}

func TestAutoMemoryLimit(t *testing.T) {
	for _, c := range []struct{ ram, want int64 }{
		{0, 0}, {8 << 30, (8 << 30) / 10}, {64 << 30, 2 << 30},
	} {
		if got := autoMemoryLimit(c.ram); got != c.want {
			t.Errorf("autoMemoryLimit(%d) = %d, want %d", c.ram, got, c.want)
		}
	}
}
