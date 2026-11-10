//go:build !linux && !darwin

package multiplex

import "errors"

// No source for a process's start time here, so ReapOrphans kills nothing.
func readProcIdent(int) (procIdent, error) {
	return procIdent{}, errors.New("process identity: unsupported on this platform")
}

func groupMembers(int) []int { return nil }
