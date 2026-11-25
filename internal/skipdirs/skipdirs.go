// Package skipdirs holds the one list of directory names the index, the
// watcher and schema detection never descend into.
package skipdirs

import "strings"

// skipDirs are directories never descended into during Build. Hardcoded
// for now; promote to config when a real workspace needs it. Lives in a leaf package
// so config and symbols share one list (symbols imports config).
var skipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
	"__pycache__":  true,
	"dist":         true,
	"build":        true,
	".idea":        true,
	".vscode":      true,
	// Gradle's per-project cache. It was harmless while we indexed no
	// C/C++: on reposilite it holds a downloaded Node toolchain whose
	// 2,290 bundled .h files are 1.5M lexical sites — 35x the rest of
	// the repo put together, none of it the user's code.
	".gradle": true,
	// iOS / Swift. CocoaPods and Carthage vendor dependencies (Pods/ is
	// the iOS node_modules, often committed), SwiftPM checks them out
	// under .build/ and keeps Xcode state in .swiftpm/, Xcode writes
	// DerivedData/ (a project-local one when so configured) and per-user
	// UI state in xcuserdata/.
	"Pods":        true,
	"Carthage":    true,
	".build":      true,
	".swiftpm":    true,
	"DerivedData": true,
	"xcuserdata":  true,
	// Python tool dirs. A virtualenv is vendored third-party code, often 15x
	// the first-party lines (1.19M venv vs 77k in the 2026-10-22 dogfood).
	// site-packages and .eggs sit directly under .venv but are listed too:
	// pip install --target and tox put them outside any .venv parent. A
	// first-party dir named venv is the accepted risk; naming it or an
	// include pattern opens it, like every skip dir.
	".venv":          true,
	"venv":           true,
	".tox":           true,
	"site-packages":  true,
	".eggs":          true,
}

// skipDirSuffixes are directory-name suffixes skipped like skipDirs. An
// asset catalog (Assets.xcassets) is a tree of Contents.json files, one
// per image set, each naming "images", "idiom", "scale" and "filename":
// JSON the index would read as hundreds of identical names.
var skipDirSuffixes = []string{".xcassets"}

// Is reports whether a directory name is skipped.
func Is(name string) bool {
	if skipDirs[name] {
		return true
	}
	for _, s := range skipDirSuffixes {
		if len(name) > len(s) && strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
}

// IsWatchSkip is Is plus .claude, for walkers that open files in a child
// LSP or hold inotify watches (the watcher, proactive open, schema
// detection). Claude Code worktrees live under .claude/worktrees; each is a
// full checkout with its own node_modules, and one tsserver held 30k of the
// 65,536 inotify watches there. Is stays without .claude on purpose: the
// index walk and queries reach into it to report a nested worktree as
// "worktree of this repo" and to let --include walk one.
func IsWatchSkip(name string) bool { return name == ".claude" || Is(name) }
