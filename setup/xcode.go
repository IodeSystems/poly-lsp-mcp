package setup

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Xcode projects and sourcekit-lsp.
//
// sourcekit-lsp reads a SwiftPM package (Package.swift) by itself. An Xcode
// project keeps its build settings in project.pbxproj, which sourcekit-lsp
// does not read: without a build server it resolves each file alone, so
// references across files are not resolved. xcode-build-server is that build
// server; `xcode-build-server config` writes a buildServer.json beside the
// project, which sourcekit-lsp finds. Driving xcodebuild is left to it.

// XcodeHints returns one hint per Swift project root that holds an Xcode
// project or workspace but no buildServer.json. xcode maps a root to the
// .xcodeproj / .xcworkspace names in it (symbols.ProjectRoots.Xcode); rel
// renders a root for the message.
func XcodeHints(xcode map[string][]string, rel func(string) string) []string {
	dirs := make([]string, 0, len(xcode))
	for d := range xcode {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	var out []string
	for _, dir := range dirs {
		if _, err := os.Stat(filepath.Join(dir, "buildServer.json")); err == nil {
			continue
		}
		out = append(out, xcodeHint(dir, rel(dir), xcode[dir]))
	}
	return out
}

// xcodeHint names the command for one root. A workspace wins over a
// project: with CocoaPods the workspace is what builds (the pods are
// another project in it). The scheme is filled in when the bundle shares
// exactly one.
func xcodeHint(dir, rel string, bundles []string) string {
	sort.Strings(bundles)
	bundle, flag := bundles[0], "-project"
	for _, b := range bundles {
		if strings.HasSuffix(b, ".xcworkspace") {
			bundle, flag = b, "-workspace"
			break
		}
	}
	scheme := "<scheme>"
	if s := sharedSchemes(dir, bundles); len(s) == 1 {
		scheme = s[0]
	}
	return fmt.Sprintf("swift: %s has an Xcode project (%s) and no buildServer.json; sourcekit-lsp resolves its files one at a time, "+
		"not across files. Add a build server (brew install xcode-build-server), then in %s: xcode-build-server config %s %s -scheme %s",
		rel, bundle, rel, flag, bundle, scheme)
}

// sharedSchemes lists the schemes committed under the bundles'
// xcshareddata/xcschemes. A user's own schemes live in xcuserdata and are
// not read.
func sharedSchemes(dir string, bundles []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, b := range bundles {
		ms, _ := filepath.Glob(filepath.Join(dir, b, "xcshareddata", "xcschemes", "*.xcscheme"))
		for _, m := range ms {
			name := strings.TrimSuffix(filepath.Base(m), ".xcscheme")
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	sort.Strings(out)
	return out
}
