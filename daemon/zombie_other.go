//go:build !linux && !darwin

package daemon

// pidZombie has no implementation here: a zombie counts as alive.
func pidZombie(int) bool { return false }
