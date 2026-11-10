package multiplex

import "testing"

func TestDefaultMaxRSS(t *testing.T) {
	const gib = int64(1) << 30
	for _, tc := range []struct{ total, want int64 }{
		{0, 3072 << 20},
		{8 * gib, 8 * gib / 100 * 15},
		{64 * gib, 3072 << 20},
	} {
		if got := DefaultMaxRSS(tc.total); got != tc.want {
			t.Errorf("DefaultMaxRSS(%d) = %d, want %d", tc.total, got, tc.want)
		}
	}
}

func TestTotalRAM(t *testing.T) {
	if r := TotalRAM(); r < 0 {
		t.Fatalf("TotalRAM = %d", r)
	}
}
