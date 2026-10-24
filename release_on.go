//go:build release

package main

// releaseBuild marks a binary built by the release workflow (-tags release).
// doctor tells it from a `go install` build by this, since both lack a
// checkout and the update path differs: a download, not `go install`.
const releaseBuild = true
