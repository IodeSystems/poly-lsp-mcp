package bindings

import (
	"bytes"
	"context"
	"regexp"
	"sort"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/java"
	"github.com/smacker/go-tree-sitter/kotlin"

	"github.com/iodesystems/poly-lsp-mcp/symbols"
)

// ApplyAndroid binds Android resource names to the Java and Kotlin sites that
// address them. On Android the cross-language contract is a STRING, not a
// symbol:
//
//	TermuxPreferenceConstants.java     KEY_SCROLL_BEHAVIOUR = "scroll_behaviour"
//	termux_terminal_io_preferences.xml app:key="scroll_behaviour"
//	TerminalIOPreferencesFragment.java case "scroll_behaviour":
//
// Nothing checks that those three agree. The compiler cannot: two of them are
// string literals and the third is an XML attribute value. A typo compiles,
// ships, and the setting silently never applies — exactly the class of bug the
// declared tier exists to make visible.
//
// The code side is invisible to the normal index by design: the tree-sitter
// extractor drops identifier-shaped tokens inside string literals, which is
// right for Go and wrong for Android. Rather than widening the Java/Kotlin
// query to capture every literal (which would flood the index with "UTF-8" and
// ""), this binds only literals whose value is a resource name the XML side
// already declares — the same want-set gate ApplyDerived uses for gat
// operationIds.
//
// Returns the resource roots that were declared, for the caller's log.
func (r *Resolver) ApplyAndroid(idx *symbols.Index) []DerivRoot {
	// 1. Resource names the XML side declares or keys on.
	xmlSites := map[string][]symbols.Site{}
	walkFiles(r.root, r.ignores, func(path string, data []byte) {
		if !hasSuffix(path, ".xml") {
			return
		}
		for _, h := range androidResourceSites(data) {
			xmlSites[h.value] = append(xmlSites[h.value], symbols.Site{
				File: path, Line: h.line, Col: h.col,
				Language: "xml", Confidence: symbols.ConfidenceDeclared,
			})
		}
	})
	if len(xmlSites) == 0 {
		return nil
	}

	// 2. Java / Kotlin string literals naming one of them.
	codeSites := map[string][]symbols.Site{}
	walkFiles(r.root, r.ignores, func(path string, data []byte) {
		lang, hits := codeStringLiteralSites(path, data)
		if lang == "" {
			return
		}
		for _, h := range hits {
			if _, ok := xmlSites[h.value]; !ok {
				continue
			}
			codeSites[h.value] = append(codeSites[h.value], symbols.Site{
				File: path, Line: h.line, Col: h.col,
				Language: lang, Confidence: symbols.ConfidenceDeclared,
			})
		}
	})

	// 3. Only a name that appears on BOTH sides is a cross-language binding.
	// A resource no code addresses is just a resource, and the lexical tier
	// already has it — declaring it here would add tens of thousands of sites
	// that carry no cross-language information.
	var roots []DerivRoot
	for name, csites := range codeSites {
		for _, s := range xmlSites[name] {
			idx.InsertDeclared(name, s.File, s.Language, s.Line, s.Col)
		}
		for _, s := range csites {
			idx.InsertDeclared(name, s.File, s.Language, s.Line, s.Col)
		}
		roots = append(roots, DerivRoot{
			Name:   name,
			Kind:   "android-resource",
			Source: xmlSites[name][0],
		})
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].Name < roots[j].Name })
	return roots
}

type androidHit struct {
	value string
	line  int
	col   int
	// precise marks a match from an attribute that exists specifically to be
	// read back by code (app:key, android:name, …). Those bind unconditionally.
	// A generic `name="x"` or `@string/x` is weaker evidence and additionally
	// has to look like an identifier someone chose, not a common word.
	precise bool
}

// androidDistinctive reports whether a name is specific enough to be worth
// binding on weak evidence. Resource names people actually cross-reference are
// snake_case, dotted, or CamelCase; bare short words like "color", "key" or
// "layout" collide with unrelated Java literals by coincidence.
func androidDistinctive(name string) bool {
	if strings.ContainsAny(name, "_.") || len(name) >= 12 {
		return true
	}
	for _, c := range name {
		if c >= 'A' && c <= 'Z' {
			return true
		}
	}
	return false
}

var (
	// name="foo" on a resource element: <string name=…>, <string-array name=…>,
	// <color name=…>, and every other values/ declaration.
	androidResNameRe = regexp.MustCompile(`\bname="([A-Za-z_][A-Za-z0-9_.]*)"`)
	// @+id/foo declares an id; @string/foo, @array/foo, … reference a resource.
	androidResRefRe = regexp.MustCompile(`@\+?(?:id|string|array|drawable|color|style|dimen|layout|integer|bool|plurals|xml|menu|anim)/([A-Za-z_][A-Za-z0-9_.]*)`)
	// Preference keys and their stored values are the settings-screen contract
	// with Java: app:key="x" is read back by a Java constant holding "x".
	androidKeyRe = regexp.MustCompile(`\b(?:app|android):(?:key|defaultValue|entryValues|fragment|name)="([^"]+)"`)
)

// androidResourceSites extracts the resource names an XML file declares or
// keys on, with 1-based positions.
func androidResourceSites(data []byte) []androidHit {
	var out []androidHit
	add := func(re *regexp.Regexp, precise bool) {
		for _, m := range re.FindAllSubmatchIndex(data, -1) {
			value := string(data[m[2]:m[3]])
			for _, v := range androidBindableValues(value) {
				if !precise && !androidDistinctive(v) {
					continue
				}
				line, col := lineColAt(data, m[2])
				out = append(out, androidHit{value: v, line: line, col: col, precise: precise})
			}
		}
	}
	add(androidResNameRe, false)
	add(androidResRefRe, false)
	add(androidKeyRe, true)
	return out
}

// androidBindableValues normalises one attribute value into the names worth
// binding. A fully-qualified `app:fragment="com.termux.app.X"` yields both the
// whole path and its leaf class, since the Java side answers to the leaf.
// Values that carry no cross-language meaning (numbers, booleans, anything with
// whitespace) are dropped.
func androidBindableValues(value string) []string {
	value = strings.TrimSpace(value)
	if len(value) < 3 || strings.ContainsAny(value, " \t\n\"'<>") {
		return nil
	}
	switch value {
	case "true", "false", "null", "wrap_content", "match_parent":
		return nil
	}
	if isNumeric(value) {
		return nil
	}
	out := []string{value}
	if i := strings.LastIndexByte(value, '.'); i >= 0 && i+1 < len(value) {
		if leaf := value[i+1:]; len(leaf) >= 3 {
			out = append(out, leaf)
		}
	}
	return out
}

func isNumeric(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && c != '.' && c != '-' && c != '+' {
			return false
		}
	}
	return true
}

// codeStringLiteralSites dispatches to the per-language literal extractor for
// a source file and returns the language tag to record with each site. An
// unhandled extension returns "" and is skipped.
func codeStringLiteralSites(path string, data []byte) (string, []androidHit) {
	switch {
	case hasSuffix(path, ".java"):
		return "java", javaStringLiteralSites(data)
	case hasSuffix(path, ".kt", ".kts"):
		return "kotlin", kotlinStringLiteralSites(data)
	}
	return "", nil
}

var javaStringLiteralQuery = mustQuery(`(string_literal) @s`, java.GetLanguage())

// javaStringLiteralSites returns every string literal in a Java file with the
// position of its first character (inside the opening quote).
func javaStringLiteralSites(content []byte) []androidHit {
	p := sitter.NewParser()
	p.SetLanguage(java.GetLanguage())
	tree, err := p.ParseCtx(context.Background(), nil, content)
	if err != nil || tree == nil {
		return nil
	}
	defer tree.Close()

	cur := sitter.NewQueryCursor()
	defer cur.Close()
	cur.Exec(javaStringLiteralQuery, tree.RootNode())

	var out []androidHit
	for {
		m, ok := cur.NextMatch()
		if !ok {
			break
		}
		for _, c := range m.Captures {
			value := strings.Trim(c.Node.Content(content), `"`)
			if value == "" {
				continue
			}
			pt := c.Node.StartPoint()
			out = append(out, androidHit{
				value: value,
				line:  int(pt.Row) + 1,
				col:   int(pt.Column) + 2, // skip the opening quote
			})
		}
	}
	return out
}

var kotlinStringLiteralQuery = mustQuery(`(string_literal) @s`, kotlin.GetLanguage())

// kotlinStringLiteralSites returns every NON-INTERPOLATED Kotlin string
// literal with the position of its first character.
//
// The interpolation check is what makes this safe. Kotlin models `"a${x}b"` as
// a string_literal with several children, of which the string_content pieces
// are only FRAGMENTS: reading one would bind the resource `prefix_only` to
// `"prefix_only$suffix"`, a value that never equals it at runtime. So only a
// literal whose sole child is one string_content counts, which also drops the
// empty literal (no children at all). Raw `"""x"""` strings pass and report
// the right column, because the position comes from the content node rather
// than from the quote.
func kotlinStringLiteralSites(content []byte) []androidHit {
	p := sitter.NewParser()
	p.SetLanguage(kotlin.GetLanguage())
	tree, err := p.ParseCtx(context.Background(), nil, content)
	if err != nil || tree == nil {
		return nil
	}
	defer tree.Close()

	cur := sitter.NewQueryCursor()
	defer cur.Close()
	cur.Exec(kotlinStringLiteralQuery, tree.RootNode())

	var out []androidHit
	for {
		m, ok := cur.NextMatch()
		if !ok {
			break
		}
		for _, c := range m.Captures {
			if c.Node.NamedChildCount() != 1 {
				continue
			}
			body := c.Node.NamedChild(0)
			if body.Type() != "string_content" {
				continue
			}
			value := body.Content(content)
			if value == "" {
				continue
			}
			pt := body.StartPoint()
			out = append(out, androidHit{
				value: value,
				line:  int(pt.Row) + 1,
				col:   int(pt.Column) + 1,
			})
		}
	}
	return out
}

// lineColAt converts a byte offset to a 1-based line and column.
func lineColAt(data []byte, off int) (int, int) {
	if off > len(data) {
		off = len(data)
	}
	line := bytes.Count(data[:off], []byte("\n")) + 1
	col := off
	if nl := bytes.LastIndexByte(data[:off], '\n'); nl >= 0 {
		col = off - nl - 1
	}
	return line, col + 1
}

func mustQuery(q string, lang *sitter.Language) *sitter.Query {
	query, err := sitter.NewQuery([]byte(q), lang)
	if err != nil {
		panic("bindings: bad query " + q + ": " + err.Error())
	}
	return query
}
