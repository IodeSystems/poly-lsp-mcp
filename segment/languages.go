package segment

import (
	"path"
	"sort"
	"strings"

	sitter "github.com/iodesystems/poly-lsp-mcp/internal/ts"
)

// LanguageByName returns the tree-sitter language object for one of the
// languages segment can parse, or nil for any other. symbols.LanguageByName
// delegates here.
func LanguageByName(name string) *sitter.Language {
	switch name {
	case "go":
		return sitter.Go()
	case "typescript":
		return sitter.TSX()
	case "python":
		return sitter.Python()
	case "sql":
		return sitter.SQL()
	case "java":
		return sitter.Java()
	case "c":
		return sitter.C()
	case "cpp":
		return sitter.Cpp()
	case "kotlin":
		return sitter.Kotlin()
	case "groovy":
		return sitter.Groovy()
	case "swift":
		return sitter.Swift()
	case "markdown":
		// The BLOCK grammar. It yields nested `section` nodes whose span
		// covers a heading AND its body, which is exactly the unit a
		// document is navigated by. The companion inline grammar (emphasis,
		// links, code spans) is a separate tree and is not needed for
		// structure.
		return sitter.Markdown()
	}
	return nil
}

// langExts is the single owner of the default extension -> language table
// (lowercase, no dot, in registry order). Package config builds its default
// registry from it through Extensions.
var langExts = []struct {
	lang string
	exts []string
}{
	{"go", []string{"go"}},
	{"typescript", []string{"ts", "tsx", "js", "jsx", "mjs", "cjs"}},
	{"python", []string{"py", "pyi"}},
	{"markdown", []string{"md", "markdown"}},
	{"sql", []string{"sql", "psql"}},
	{"java", []string{"java"}},
	{"kotlin", []string{"kt", "kts"}},
	{"groovy", []string{"groovy", "gradle", "gvy", "gy"}},
	{"c", []string{"c"}},
	{"cpp", []string{"cpp", "cc", "cxx", "c++", "h", "hpp", "hh", "hxx", "h++", "ipp", "tcc", "inl"}},
	{"swift", []string{"swift"}},
}

// Languages lists, sorted, the languages Symbols can parse.
func Languages() []string {
	out := make([]string, 0, len(langExts))
	for _, e := range langExts {
		out = append(out, e.lang)
	}
	sort.Strings(out)
	return out
}

// Extensions returns the default file extensions of lang (lowercase, no
// dot), or nil for a language without a grammar. The slice is a copy.
func Extensions(lang string) []string {
	for _, e := range langExts {
		if e.lang == lang {
			return append([]string(nil), e.exts...)
		}
	}
	return nil
}

// ForPath returns the language for a file path by its extension
// (case-insensitive). ok is false when the extension maps to no grammar.
func ForPath(p string) (lang string, ok bool) {
	ext := strings.ToLower(path.Ext(strings.ReplaceAll(p, `\`, "/")))
	if ext == "" {
		return "", false
	}
	for _, e := range langExts {
		for _, x := range e.exts {
			if x == ext[1:] {
				return e.lang, true
			}
		}
	}
	return "", false
}
