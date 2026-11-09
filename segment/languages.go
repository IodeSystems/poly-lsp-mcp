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

// extLang maps a lowercase file extension (no dot) to a language name. It
// mirrors the default extension table of package config for the languages
// that have a grammar; segment may not import config.
var extLang = map[string]string{
	"go": "go",
	"ts": "typescript", "tsx": "typescript", "js": "typescript",
	"jsx": "typescript", "mjs": "typescript", "cjs": "typescript",
	"py": "python", "pyi": "python",
	"md": "markdown", "markdown": "markdown",
	"sql": "sql", "psql": "sql",
	"java": "java",
	"kt":   "kotlin", "kts": "kotlin",
	"groovy": "groovy", "gradle": "groovy", "gvy": "groovy", "gy": "groovy",
	"c":   "c",
	"cpp": "cpp", "cc": "cpp", "cxx": "cpp", "c++": "cpp", "h": "cpp",
	"hpp": "cpp", "hh": "cpp", "hxx": "cpp", "h++": "cpp", "ipp": "cpp",
	"tcc": "cpp", "inl": "cpp",
	"swift": "swift",
}

// Languages lists, sorted, the languages Symbols can parse.
func Languages() []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range extLang {
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out
}

// ForPath returns the language for a file path by its extension
// (case-insensitive). ok is false when the extension maps to no grammar.
func ForPath(p string) (lang string, ok bool) {
	ext := strings.ToLower(path.Ext(strings.ReplaceAll(p, `\`, "/")))
	if ext == "" {
		return "", false
	}
	lang, ok = extLang[ext[1:]]
	return lang, ok
}
