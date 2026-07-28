package symbols

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
)

// Signature refactor for XML.
//
// XML has no tree-sitter grammar here (see XMLExtractor for why) and no
// call expression, so it reaches the refactor surface through its own
// path rather than through langOps' grammar walk. The mapping onto the
// shared contract is:
//
//	Name    the name the element DECLARES — the `name`/`key` attribute
//	        value, or an `@+id/…` id — falling back to the tag name.
//	Params  the element's ATTRIBUTE LIST. An element's attributes are
//	        its parameter list: the same "replace the whole list in one
//	        structured edit" operation, spelled `k="v"` instead of
//	        `T name`. Param.Name is the attribute name and Param.Type
//	        carries its VALUE.
//	Result  none. XML has no return type, and asking for one is an
//	        error rather than a silent no-op.
//
// RENAME is deliberately NOT handled here: a rename of an XML name is a
// workspace-wide operation that already works through the index path,
// because XMLExtractor indexes the declaration `<string name="x">` and
// every `@string/x` reference under the same name. Verified end-to-end —
// one call rewrites both files.

// xmlSignature locates the innermost element containing (line, col),
// 1-based, and exposes the ranges a refactor needs. Returns nil when no
// element covers the position.
func xmlSignature(content []byte, line, col int) (*FunctionSignature, error) {
	lc := newLineCol(content)
	target := -1
	// Resolve (line, col) to a byte offset by scanning the line table.
	for off := 0; off <= len(content); off++ {
		l, c := lc.at(off)
		if l == line && c == col {
			target = off
			break
		}
		if l > line {
			break
		}
	}
	if target < 0 {
		return nil, nil
	}

	dec := xml.NewDecoder(bytes.NewReader(content))
	dec.Strict = false
	dec.AutoClose = xml.HTMLAutoClose
	dec.Entity = xml.HTMLEntity

	type frame struct {
		start, end int
	}
	var stack []frame
	var best *frame

	var prev int64
	for {
		tokStart := int(prev)
		tok, err := dec.Token()
		tokEnd := int(dec.InputOffset())
		prev = dec.InputOffset()
		if err != nil {
			break
		}
		switch tok.(type) {
		case xml.StartElement:
			f := frame{start: tokStart, end: tokEnd}
			// The OPEN TAG is what a refactor edits, so an element whose
			// open tag contains the position wins immediately; the
			// innermost such element is the answer.
			if target >= f.start && target < f.end {
				cp := f
				best = &cp
			}
			stack = append(stack, f)
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
		if tokEnd > len(content) {
			break
		}
	}
	if best == nil {
		return nil, nil
	}
	return xmlSignatureFromTag(content, best.start, best.end)
}

// xmlSignatureFromTag carves an open tag `<tag a="1" b="2">` into the
// ranges the contract wants. Offsets are absolute.
func xmlSignatureFromTag(content []byte, start, end int) (*FunctionSignature, error) {
	if start < 0 || end > len(content) || start >= end {
		return nil, fmt.Errorf("xml: element span out of range")
	}
	raw := string(content[start:end])
	lt := strings.IndexByte(raw, '<')
	if lt < 0 {
		return nil, fmt.Errorf("xml: element does not start with '<'")
	}
	i := lt + 1
	for i < len(raw) && (raw[i] == ' ' || raw[i] == '\t' || raw[i] == '\n') {
		i++
	}
	nameStart := i
	for i < len(raw) && !isXMLTagBreak(raw[i]) {
		i++
	}
	nameEnd := i
	if nameEnd == nameStart {
		return nil, fmt.Errorf("xml: element has no tag name")
	}

	// The attribute region runs from just after the tag name to the
	// closing '>' (excluding a self-closing '/').
	closeIdx := strings.LastIndexByte(raw, '>')
	if closeIdx < 0 {
		return nil, fmt.Errorf("xml: element has no '>'")
	}
	attrsEnd := closeIdx
	for attrsEnd > nameEnd && (raw[attrsEnd-1] == '/' || raw[attrsEnd-1] == ' ' ||
		raw[attrsEnd-1] == '\t' || raw[attrsEnd-1] == '\n') {
		attrsEnd--
	}
	attrsStart := nameEnd
	for attrsStart < attrsEnd && (raw[attrsStart] == ' ' || raw[attrsStart] == '\t' || raw[attrsStart] == '\n') {
		attrsStart++
	}

	sig := &FunctionSignature{
		Language:  "xml",
		Type:      "element",
		Name:      ByteRange{Start: start + nameStart, End: start + nameEnd},
		Params:    ByteRange{Start: start + attrsStart, End: start + attrsEnd},
		BodyStart: end,
	}
	// Prefer the name the element DECLARES over its tag name, so a
	// rename lands on `action_open_settings`, not on `string`.
	if ns, ne, ok := xmlDeclaredNameSpan(raw); ok {
		sig.Name = ByteRange{Start: start + ns, End: start + ne}
	}
	return sig, nil
}

func isXMLTagBreak(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '/', '>':
		return true
	}
	return false
}

// xmlDeclaredNameSpan finds the span of the value of the first
// name-bearing attribute (`name=`, `key=`), or of an `@+id/…` id. It
// returns offsets RELATIVE to raw.
func xmlDeclaredNameSpan(raw string) (int, int, bool) {
	for i := 0; i < len(raw); i++ {
		if raw[i] != '=' {
			continue
		}
		// Walk back over the attribute name.
		j := i
		for j > 0 && (raw[j-1] == ' ' || raw[j-1] == '\t') {
			j--
		}
		attrEnd := j
		for j > 0 && !isXMLTagBreak(raw[j-1]) && raw[j-1] != '=' {
			j--
		}
		attr := raw[j:attrEnd]
		if k := strings.IndexByte(attr, ':'); k >= 0 {
			attr = attr[k+1:]
		}
		// Find the quoted value.
		v := i + 1
		for v < len(raw) && (raw[v] == ' ' || raw[v] == '\t') {
			v++
		}
		if v >= len(raw) || (raw[v] != '"' && raw[v] != '\'') {
			continue
		}
		quote := raw[v]
		valStart := v + 1
		valEnd := valStart
		for valEnd < len(raw) && raw[valEnd] != quote {
			valEnd++
		}
		if valEnd >= len(raw) {
			continue
		}
		val := raw[valStart:valEnd]
		if nameAttrs[attr] && identOnlyRe.MatchString(val) {
			return valStart, valEnd, true
		}
		// android:id="@+id/foo" DECLARES foo; the name is the segment
		// after the slash, not the whole reference.
		if strings.HasPrefix(val, "@+") {
			if slash := strings.LastIndexByte(val, '/'); slash >= 0 {
				return valStart + slash + 1, valEnd, true
			}
		}
	}
	return 0, 0, false
}

var xmlLangOps = &langOps{
	// Never used: XML resolves its signature through xmlSignature, not
	// through the grammar walk.
	isSignatureNode:     func(*sitter.Node) bool { return false },
	extractSignature:    nil,
	callNodeType:        "",
	extractCallSite:     nil,
	formatParams:        formatXMLAttributes,
	formatResultReplace: func(typ string) string { return typ },
	insertResult: func(sig *FunctionSignature, typ string) (int, string) {
		// Unreachable: the MCP layer rejects `return` for XML, because a
		// silent no-op would report success for an impossible edit.
		return sig.BodyStart, ""
	},
	zeroValue: func(string) string { return "" },
}

// formatXMLAttributes renders `k="v"` pairs. Param.Type carries the
// attribute VALUE; an entry with no value renders as a bare attribute,
// which HTML-ish XML allows.
func formatXMLAttributes(params []Param) string {
	if len(params) == 0 {
		return ""
	}
	parts := make([]string, 0, len(params))
	for _, p := range params {
		if p.Type == "" {
			parts = append(parts, p.Name)
			continue
		}
		// A value containing a double quote is emitted in single quotes
		// rather than silently corrupting the tag.
		if strings.Contains(p.Type, `"`) {
			parts = append(parts, p.Name+"='"+p.Type+"'")
			continue
		}
		parts = append(parts, p.Name+`="`+p.Type+`"`)
	}
	return strings.Join(parts, " ")
}
