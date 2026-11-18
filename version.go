package main

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/iodesystems/poly-lsp-mcp/multiplex"
)

// The version comes from the build info the Go toolchain embeds, never from
// a stamp (TestNoBuildTargetStampsTheBinary).
func init() { multiplex.BuildLabel = versionLine(readBuildInfo()) }

// versionLine is the one line `--version` prints:
//
//	poly-lsp-mcp v0.5.2 (4530caa, 2026-11-15, release, linux/amd64)
//
// The version is the module version (a tag or pseudo-version; "(devel)" for
// an untagged tree), the revision is 7 characters with "-dirty" when the
// tree had edits, the date is the commit date, and the kind is release
// (-tags release), go install (no VCS settings) or checkout.
func versionLine(bi *debug.BuildInfo, release bool) string {
	ver := "unknown"
	var rev, at, dirty string
	if bi != nil {
		if bi.Main.Version != "" {
			ver = bi.Main.Version
		}
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.time":
				at = s.Value
			case "vcs.modified":
				if s.Value == "true" {
					dirty = "-dirty"
				}
			}
		}
	}
	// The toolchain marks a dirty tree "+dirty" in the version too; the
	// revision already says so.
	ver = strings.TrimSuffix(ver, "+dirty")
	kind := "checkout"
	switch {
	case release:
		kind = "release"
	case rev == "":
		kind = "go install"
	}
	if len(rev) > 7 {
		rev = rev[:7]
	}
	if len(at) >= 10 {
		at = at[:10] // the date of an RFC 3339 time
	}
	var parts []string
	if rev != "" {
		parts = append(parts, rev+dirty)
	}
	if at != "" {
		parts = append(parts, at)
	}
	parts = append(parts, kind, runtime.GOOS+"/"+runtime.GOARCH)
	return fmt.Sprintf("poly-lsp-mcp %s (%s)", ver, strings.Join(parts, ", "))
}
