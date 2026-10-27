package setup

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// sourcekit-lsp and its index.
//
// sourcekit-lsp answers references and cross-module definitions from an
// index store. From Swift 6.1 it builds that index in the background by
// itself (measured with swift.org's 6.4.1 on Linux: a three-file package's
// cross-file definition resolved about two seconds after the file opened).
// Before 6.1 nothing builds it but a build: `swift build` for a package, an
// Xcode build for a project, or the opt-in backgroundIndexing setting 6.0
// added. Until then references come back empty.
//
// sourcekit-lsp has no --version; the toolchain's `swift --version` names
// the release. A swift.org toolchain keeps swift beside sourcekit-lsp, and
// so do the xcrun shims in macOS's /usr/bin; Nix packages them apart, so
// PATH's swift is the fallback.

// SwiftVersion returns the first line of `swift --version` for the
// toolchain holding the sourcekit-lsp at lspPath, or "" when no swift runs.
func SwiftVersion(lspPath string) string {
	for _, swift := range []string{filepath.Join(filepath.Dir(lspPath), "swift"), "swift"} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		out, err := exec.CommandContext(ctx, swift, "--version").Output()
		cancel()
		if err == nil {
			line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
			return line
		}
	}
	return ""
}

var swiftVersionRe = regexp.MustCompile(`Swift version (\d+)\.(\d+)`)

// SourceKitIndexHint says what a Swift workspace needs before sourcekit-lsp
// can answer references, given the toolchain's version line: nothing from
// 6.1 on, a build (or the opt-in setting) before it. An unreadable version
// gets the hint too, qualified.
func SourceKitIndexHint(version string) string {
	const build = "cross-file references and definitions need an index: run `swift build` in a package (an Xcode build for a project) after changes"
	m := swiftVersionRe.FindStringSubmatch(version)
	if m == nil {
		return "swift: could not read the toolchain version; if it is older than Swift 6.1, sourcekit-lsp does not index in the background and " + build
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	if major > 6 || major == 6 && minor >= 1 {
		return ""
	}
	hint := fmt.Sprintf("swift: sourcekit-lsp from Swift %s.%s does not index in the background, so %s", m[1], m[2], build)
	if major == 6 {
		hint += `, or turn on its background indexing: {"backgroundIndexing": true} in .sourcekit-lsp/config.json`
	}
	return hint
}
