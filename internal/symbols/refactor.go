package symbols

import (
	"context"
	"fmt"

	sitter "github.com/smacker/go-tree-sitter"
)

// GoFunctionSignature locates a Go function or method declaration at a
// position and exposes the byte ranges of the pieces a signature
// refactor needs to rewrite:
//
//   - Name: the identifier (function_declaration) or field_identifier
//     (method_declaration) that names the function.
//   - Params: the parameter_list including its parens.
//   - Result: the result type expression. Zero ranges (Start==End) when
//     the function has no declared return type.
//   - BodyStart: the byte offset of the opening `{` of the body —
//     useful for inserting a return type when none currently exists.
//
// Byte offsets are 0-based; ranges are start-inclusive, end-exclusive.
// Lookup is positional (the position must land somewhere inside the
// function_declaration / method_declaration node), so callers can
// pass any range within the function, not just the name range.
type GoFunctionSignature struct {
	// Type is the tree-sitter node type — "function_declaration" or
	// "method_declaration". Callers that care about the receiver
	// (only method_declaration) should branch on this.
	Type string

	Name      ByteRange
	Params    ByteRange
	Result    ByteRange // zero when there's no declared result
	BodyStart int

	// Receiver is populated for method_declaration only; zero ranges
	// otherwise. Includes the parens — same convention as Params.
	Receiver ByteRange
}

// ByteRange is half-open: [Start, End). A zero range (Start==End) is a
// valid "not present" sentinel.
type ByteRange struct {
	Start int
	End   int
}

// Empty reports whether the range has no extent (Start == End). Used
// to detect "no current result type" cleanly.
func (r ByteRange) Empty() bool { return r.Start == r.End }

// FindGoFunctionSignature parses content with the Go grammar and
// returns the signature span for the function declaration that
// contains (line, col). 1-based positions; "contains" means the
// position is inside the function_declaration / method_declaration
// node.
//
// Returns nil if no such declaration exists at the position (e.g.,
// the position is inside a type_declaration or at file top level).
func FindGoFunctionSignature(content []byte, line, col int) (*GoFunctionSignature, error) {
	lang := LanguageByName("go")
	if lang == nil {
		return nil, fmt.Errorf("no tree-sitter grammar for go")
	}
	parser := sitter.NewParser()
	parser.SetLanguage(lang)
	tree, err := parser.ParseCtx(context.Background(), nil, content)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if tree == nil {
		return nil, fmt.Errorf("parse returned nil tree")
	}
	defer tree.Close()

	row, column := uint32(line-1), uint32(col-1)
	root := tree.RootNode()

	// Walk root's named children and find the function/method whose
	// range contains the position.
	count := int(root.NamedChildCount())
	for i := range count {
		child := root.NamedChild(i)
		switch child.Type() {
		case "function_declaration", "method_declaration":
		default:
			continue
		}
		sp := child.StartPoint()
		ep := child.EndPoint()
		if !pointInRange(row, column, sp, ep) {
			continue
		}
		return extractGoSignature(child)
	}
	return nil, nil
}

// GoCallSite describes one resolved call to a function in a single
// Go file: where it lives + the byte range of the contents inside its
// `(...)`. Used by call-site rewriting after a signature change.
//
// ArgsInnerStart / ArgsInnerEnd point at the bytes BETWEEN the parens
// (so `Foo()` has Inner = (X, X) where X is the byte after `(`).
// CurrentArgs is the list of argument expression source strings,
// trimmed. Variadics with `...` and the spread form are flagged in
// Skipped so the caller can leave them alone.
type GoCallSite struct {
	Line, Col       int
	ArgsInnerStart  int
	ArgsInnerEnd    int
	CurrentArgs     []string
	Skipped         string // non-empty: skip reason
	HasSpread       bool   // f(x...) form
}

// FindGoCallSites walks `content` and returns every call expression
// whose called function is an identifier or selector field matching
// `name`. Identifier callers (Foo()) and selector callers (x.Foo())
// both qualify so a renamed method shows up everywhere it's called.
//
// Doesn't traverse strings or comments by virtue of using tree-sitter
// (call_expression doesn't live inside string_literal or comment
// nodes).
func FindGoCallSites(content []byte, name string) ([]GoCallSite, error) {
	lang := LanguageByName("go")
	if lang == nil {
		return nil, fmt.Errorf("no tree-sitter grammar for go")
	}
	parser := sitter.NewParser()
	parser.SetLanguage(lang)
	tree, err := parser.ParseCtx(context.Background(), nil, content)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if tree == nil {
		return nil, nil
	}
	defer tree.Close()

	var sites []GoCallSite
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n.Type() == "call_expression" {
			if site, ok := callSiteForName(n, content, name); ok {
				sites = append(sites, site)
			}
		}
		count := int(n.NamedChildCount())
		for i := range count {
			walk(n.NamedChild(i))
		}
	}
	walk(tree.RootNode())
	return sites, nil
}

func callSiteForName(call *sitter.Node, content []byte, name string) (GoCallSite, bool) {
	fn := call.ChildByFieldName("function")
	if fn == nil {
		return GoCallSite{}, false
	}
	switch fn.Type() {
	case "identifier":
		if fn.Content(content) != name {
			return GoCallSite{}, false
		}
	case "selector_expression":
		field := fn.ChildByFieldName("field")
		if field == nil || field.Content(content) != name {
			return GoCallSite{}, false
		}
	default:
		return GoCallSite{}, false
	}
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return GoCallSite{}, false
	}
	// argument_list spans `(` … `)` — inner range strips the parens.
	innerStart := int(args.StartByte()) + 1
	innerEnd := int(args.EndByte()) - 1
	if innerEnd < innerStart {
		innerEnd = innerStart
	}

	site := GoCallSite{
		Line:           int(call.StartPoint().Row) + 1,
		Col:            int(call.StartPoint().Column) + 1,
		ArgsInnerStart: innerStart,
		ArgsInnerEnd:   innerEnd,
	}
	count := int(args.NamedChildCount())
	for i := range count {
		arg := args.NamedChild(i)
		txt := arg.Content(content)
		site.CurrentArgs = append(site.CurrentArgs, txt)
		if arg.Type() == "variadic_argument" {
			site.HasSpread = true
		}
	}
	if site.HasSpread {
		site.Skipped = "spread-args"
	}
	return site, true
}

func extractGoSignature(decl *sitter.Node) (*GoFunctionSignature, error) {
	sig := &GoFunctionSignature{Type: decl.Type()}

	if name := decl.ChildByFieldName("name"); name != nil {
		sig.Name = ByteRange{Start: int(name.StartByte()), End: int(name.EndByte())}
	} else {
		return nil, fmt.Errorf("%s missing name field", decl.Type())
	}
	if params := decl.ChildByFieldName("parameters"); params != nil {
		sig.Params = ByteRange{Start: int(params.StartByte()), End: int(params.EndByte())}
	} else {
		return nil, fmt.Errorf("%s missing parameters field", decl.Type())
	}
	if result := decl.ChildByFieldName("result"); result != nil {
		sig.Result = ByteRange{Start: int(result.StartByte()), End: int(result.EndByte())}
	}
	if body := decl.ChildByFieldName("body"); body != nil {
		sig.BodyStart = int(body.StartByte())
	} else {
		return nil, fmt.Errorf("%s missing body field", decl.Type())
	}
	if recv := decl.ChildByFieldName("receiver"); recv != nil {
		sig.Receiver = ByteRange{Start: int(recv.StartByte()), End: int(recv.EndByte())}
	}
	return sig, nil
}
