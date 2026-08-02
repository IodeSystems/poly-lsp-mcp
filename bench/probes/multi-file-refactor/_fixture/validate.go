package userstore

import "strings"

// ValidID reports whether id is well-formed: non-empty and space-free.
func ValidID(id UserID) bool {
	s := string(id)
	return s != "" && !strings.Contains(s, " ")
}
