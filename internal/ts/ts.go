// Package ts is the one place this module touches a tree-sitter runtime.
//
// There are two, chosen by the cgo build constraint so that a plain
// `go install` picks the right one with no flags:
//
//   - cgo (ts_cgo.go): github.com/smacker/go-tree-sitter, the C runtime. It
//     is opt-in: `-tags treesitter_cgo` with cgo enabled.
//     Type aliases only, so behaviour is exactly smacker's.
//   - !cgo (ts_purego.go): github.com/odvcencio/gotreesitter, a pure-Go
//     runtime. It is the default
//     under every CGO setting, and what the tag falls back to without cgo. It is slower: measured
//     2026-10-24, BenchmarkParseOnly 6.9 ms vs 0.68 ms (decls=30) and 77.7 ms
//     vs 6.8 ms (decls=300), so ~10x per parse; a cold `index` of this
//     repository (379 files, 3.1 MB) 5.8 s vs 1.0 s, ~6x, since the rest of
//     the build does not slow down. That is why Runtime() exists: doctor
//     and the index budget error tell the user the fast build is one C
//     compiler away.
//
// The surface is smacker's shape (Node.Type(), Node.Content(src),
// QueryCursor.NextMatch, ...) because that is what the call sites were written
// against; the pure-Go side carries the language and source a node needs
// internally so no call site changes per runtime.
package ts

import "runtime/debug"

// runtimeID is name, plus "@version" of module when the binary's build info
// lists it (it does not in a test binary of this module's own packages built
// without module info; then the bare name is returned).
func runtimeID(name, module string) string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, d := range bi.Deps {
			if d.Path == module {
				if d.Replace != nil && d.Replace.Version != "" {
					return name + "@" + d.Replace.Version
				}
				return name + "@" + d.Version
			}
		}
	}
	return name
}

// FastPathHint is the advice shown wherever the pure-Go runtime's cost is
// visible to a user. One string so doctor and the index budget error agree.
// It ends at "rebuild": the command depends on how this binary was built,
// which doctor works out and appends.
const FastPathHint = "this binary parses with pure-Go tree-sitter (built without the treesitter_cgo tag), ~10x slower to parse and ~6x slower to index than a cgo build; " +
	"for the fast path install a C compiler (Linux: gcc / build-essential; macOS: xcode-select --install) and rebuild with -tags treesitter_cgo"
