//go:build !linux && !darwin

package mcp

// lowerThreadPriority: no thread priority control on this platform.
func lowerThreadPriority() {}
