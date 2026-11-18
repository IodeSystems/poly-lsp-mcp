//go:build !unix

package main

import "os"

// termSize: no terminal query off unix; --help wraps at 80.
func termSize(*os.File) (int, bool) { return 0, false }
