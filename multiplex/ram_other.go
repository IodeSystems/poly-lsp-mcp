//go:build !linux && !darwin

package multiplex

// TotalRAM: no source on this platform; 0 = unknown.
func TotalRAM() int64 { return 0 }
