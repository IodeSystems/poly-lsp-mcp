package symbols

import (
	"bytes"
	"encoding/xml"
	"io"
	"regexp"
	"strings"
)

// XMLExtractor indexes the names an XML file actually DECLARES or REFERENCES,
// rather than every identifier-shaped token in it.
//
// The lexical extractor it replaces emitted a hit for every word, so an Android
// layout indexed `android`, `layout_width`, `wrap_content`, `match_parent` and
// `vertical` hundreds of times while burying the one name a caller was looking
// for. Measured on an agent working an Android task: it gave up on the
// structured tools for XML entirely and shelled out to `cat` and `grep` — 8.6
// cat calls per run against layouts, strings and preference screens.
//
// What is worth indexing in XML is narrow and predictable:
//
//	element names            LinearLayout, ListPreference, com.foo.CustomView
//	declared ids             android:id="@+id/scroll_settings_button"
//	resource names           <string name="action_open_settings">
//	preference keys          app:key="scroll_behaviour"
//	resource references      @string/foo, @drawable/bar, @style/baz
//
// Those are exactly the cross-file join points: a layout declares @+id/x that
// Java resolves as R.id.x, a preference screen declares a key that a
// preferences class reads back. internal/bindings pairs them up.
//
// Why encoding/xml and not tree-sitter: there is no vendored XML grammar, and
// the html grammar is not a substitute. Measured on this repo it produced 4
// ERROR nodes on a layout (fully-qualified tag names like
// com.termux.app.TermuxActivityRootView split at the first dot, since HTML tag
// names cannot contain them) and 27 on strings.xml (entities and inline
// markup). encoding/xml handles namespaces, entities, comments and CDATA
// correctly because it is an actual XML parser.
type XMLExtractor struct{}

// resRefRe matches a resource reference or declaration: @+id/name, @id/name,
// @string/name, @android:string/name. Captures the trailing name.
var resRefRe = regexp.MustCompile(`@\+?(?:[A-Za-z_][A-Za-z0-9_]*:)?[A-Za-z_][A-Za-z0-9_]*/([A-Za-z_][A-Za-z0-9_.]*)`)

// nameAttrs are attributes whose value is a DECLARED name rather than a value:
// a resource name, a preference key, a fragment/class target.
var nameAttrs = map[string]bool{
	"name": true, // <string name="…">, <style name="…">, <item name="…">
	"key":  true, // app:key / android:key on a Preference
}

// identOnlyRe guards attribute-derived names: a declared name is a plain
// identifier. Anything with spaces or punctuation is a value, not a name.
var identOnlyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*$`)

func (XMLExtractor) Extract(content []byte) []Hit {
	var hits []Hit
	dec := xml.NewDecoder(bytes.NewReader(content))
	// Android XML is namespaced but the prefixes are declared inline; being
	// strict about them buys nothing for an index.
	dec.Strict = false
	dec.AutoClose = xml.HTMLAutoClose
	dec.Entity = xml.HTMLEntity

	lc := newLineCol(content)
	var prev int64
	for {
		start := prev
		tok, err := dec.Token()
		prev = dec.InputOffset()
		if err != nil {
			// A malformed tail should not lose the names already found: XML in
			// the wild includes templated and partial files.
			if err == io.EOF {
				break
			}
			break
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		span := content[clampInt64(start, len(content)):clampInt64(prev, len(content))]
		line, col := lc.at(int(start))

		// The element name itself. Local() drops the namespace prefix, which is
		// what a caller searches for.
		if name := se.Name.Local; identOnlyRe.MatchString(name) {
			l, c := line, col
			if off := bytes.Index(span, []byte(name)); off >= 0 {
				l, c = lc.at(int(start) + off)
			}
			hits = append(hits, Hit{Name: name, Line: l, Col: c})
		}

		for _, a := range se.Attr {
			val := strings.TrimSpace(a.Value)
			if val == "" {
				continue
			}
			// position of this attribute's value inside the tag
			at := func(needle string) (int, int) {
				if off := bytes.Index(span, []byte(needle)); off >= 0 {
					return lc.at(int(start) + off)
				}
				return line, col
			}
			// A declared name: name="…" / key="…".
			if nameAttrs[a.Name.Local] && identOnlyRe.MatchString(val) {
				l, c := at(val)
				hits = append(hits, Hit{Name: val, Line: l, Col: c})
				continue
			}
			// A resource declaration or reference: @+id/x, @string/y.
			for _, m := range resRefRe.FindAllStringSubmatch(val, -1) {
				l, c := at(m[1])
				hits = append(hits, Hit{Name: m[1], Line: l, Col: c})
			}
		}
	}
	return hits
}

func clampInt64(v int64, max int) int {
	if v < 0 {
		return 0
	}
	if int(v) > max {
		return max
	}
	return int(v)
}

// lineCol converts a byte offset to 1-based line/column. Built once per file:
// an XML file yields many hits and rescanning from the top for each is
// quadratic on the big generated resource files.
type lineCol struct{ starts []int }

func newLineCol(content []byte) *lineCol {
	starts := []int{0}
	for i, b := range content {
		if b == '\n' {
			starts = append(starts, i+1)
		}
	}
	return &lineCol{starts: starts}
}

func (l *lineCol) at(off int) (int, int) {
	if off < 0 {
		off = 0
	}
	lo, hi := 0, len(l.starts)-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if l.starts[mid] <= off {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo + 1, off - l.starts[lo] + 1
}

// ── file symbols ───────────────────────────────────────────────────

// XMLFileSymbols builds an addressable symbol list for an XML file.
//
// FileSymbols is otherwise tree-sitter-only, so a grammar-less language gets a
// single whole-file entry: node_read on a 300-line layout returns the whole
// thing and nothing inside it can be addressed or edited. That is why an agent
// working an Android task bypassed the structured tools for XML and drove `cat`
// and `grep` instead.
//
// Only NAME-BEARING elements become symbols. Tag names alone are not addresses
// — a layout has eleven LinearLayouts and addressing "the third LinearLayout"
// is worse than useless — but @+id/x, name="x" and key="x" are unique within a
// file and are exactly what callers look up. Classes map into the fixed
// selector vocabulary (mcp/query.go selectorClasses); anything outside it is an
// unknown class to a selector, so:
//
//	@+id/x on an element   -> field   (mirrors the generated R.id.x field)
//	name="x" / key="x"     -> const   (a named immutable resource or key)
//
// The Decl span covers the whole element including its close tag, so node_edit
// on the symbol replaces the element; the Name span covers just the id token.
func XMLFileSymbols(content []byte) []Symbol {
	type frame struct {
		sym      string // name this element declared ("" = unnamed)
		class    string
		start    int64
		nameL    int
		nameC    int
		nameLEnd int
		nameCEnd int
	}
	var stack []frame
	var out []Symbol

	dec := xml.NewDecoder(bytes.NewReader(content))
	dec.Strict = false
	dec.AutoClose = xml.HTMLAutoClose
	dec.Entity = xml.HTMLEntity
	lc := newLineCol(content)

	var prev int64
	for {
		tokStart := prev
		tok, err := dec.Token()
		prev = dec.InputOffset()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			f := frame{start: tokStart}
			span := content[clampInt64(tokStart, len(content)):clampInt64(prev, len(content))]
			for _, a := range t.Attr {
				val := strings.TrimSpace(a.Value)
				if val == "" {
					continue
				}
				var name string
				switch {
				case nameAttrs[a.Name.Local] && identOnlyRe.MatchString(val):
					name, f.class = val, "const"
				case strings.HasPrefix(val, "@+"):
					// a DECLARATION (@+id/x); a bare @id/x is a reference
					if m := resRefRe.FindStringSubmatch(val); m != nil {
						name, f.class = m[1], "field"
					}
				}
				if name == "" || f.sym != "" {
					continue
				}
				f.sym = name
				f.nameL, f.nameC = lc.at(int(tokStart))
				if off := bytes.Index(span, []byte(name)); off >= 0 {
					f.nameL, f.nameC = lc.at(int(tokStart) + off)
					f.nameLEnd, f.nameCEnd = lc.at(int(tokStart) + off + len(name))
				}
			}
			stack = append(stack, f)
		case xml.EndElement:
			if len(stack) == 0 {
				continue
			}
			f := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if f.sym == "" {
				continue
			}
			// Nest under the nearest named ancestor so a dotted path exists,
			// while the leaf stays directly addressable.
			path := f.sym
			for i := len(stack) - 1; i >= 0; i-- {
				if stack[i].sym != "" {
					path = stack[i].sym + "." + f.sym
					break
				}
			}
			dsl, dsc := lc.at(int(f.start))
			del, dec2 := lc.at(clampInt64(prev, len(content)))
			out = append(out, Symbol{
				Sym: path, Class: f.class,
				DeclStartLine: dsl, DeclStartCol: dsc,
				DeclEndLine: del, DeclEndCol: dec2,
				NameStartLine: f.nameL, NameStartCol: f.nameC,
				NameEndLine: f.nameLEnd, NameEndCol: f.nameCEnd,
			})
		}
	}
	return out
}
