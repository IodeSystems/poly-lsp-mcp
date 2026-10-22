//go:build !linux && !darwin

package multiplex

import "errors"

// No RSS or PSI source here: the memory and pressure rules never fire.
func readVmRSS(int) (int64, error) {
	return 0, errors.New("RSS: unsupported on this platform")
}

func readMemoryPSI() (float64, error) {
	return 0, errors.New("memory PSI: unsupported on this platform")
}
