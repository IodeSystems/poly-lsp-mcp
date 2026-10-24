//go:build !release

package main

// releaseBuild is false for every build except the release workflow's
// (-tags release); see release_on.go.
const releaseBuild = false
