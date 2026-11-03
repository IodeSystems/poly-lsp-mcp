//go:build !linux && !darwin

package multiplex

// lowerPriority: no priority control here; children run at normal priority.
func lowerPriority(int) error { return nil }
