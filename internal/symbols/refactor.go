package symbols

import (
	"context"
	"fmt"
	"sort"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
)

// ByteRange is half-open: [Start, End). A zero range (Start==End) is a
// valid "not present" sentinel.
type ByteRange struct {
	Start int
	End   int
}

// Empty reports whether the range has no extent (Start == End).
func (r ByteRange) Empty() bool { return r.Start == r.End }

// Param is one entry on a function's parameter list — the name + a
// type expression in the surface syntax of the language. Type may be
// empty if the language allows un-annotated parameters (Python without
// a type hint, TypeScript with implicit `any`).
type Param struct {
	Name string
	Type string
}

// SignatureOps describes a refactor against a single function. Any
// non-empty field requests that change; empty (or nil for Params)
// means leave that aspect alone. Zero SignatureOps is a no-op.
type SignatureOps struct {
	Rename string  // "" means leave the name alone
	Params []Param // nil means leave the parameter list alone
	Return string  // "" means leave the return type alone
}

// NonEmpty reports whether at least one field requests a change.
func (o SignatureOps) NonEmpty() bool {
	return o.Rename != "" || o.Params != nil || o.Return != ""
}

// FunctionSignature locates a function declaration and exposes the
// byte ranges of the pieces a signature refactor needs to rewrite.
// Generic across languages — the Language field tells callers which
// grammar produced it.
//
//   - Name: the identifier naming the function.
//   - Params: the parameter list including its parens (`(...)`).
//   - Result: the return type as the grammar marks it. Empty for
//     void/missing-annotation. Languages differ on whether the
//     surrounding tokens (`:` in TS, `->` in Python) are included —
//     callers should treat this as opaque and use RewriteSignature
//     rather than building the new text themselves.
//   - BodyStart: byte offset of the first byte of the body (e.g.,
//     `{` in Go/TS, the first byte of the Python block).
//   - Receiver: Go method_declaration only; empty otherwise.
//
// Byte offsets are 0-based; 1-based positions go through
// FindFunctionSignature's line/col args.
type FunctionSignature struct {
	Language string
	Type     string
	Name     ByteRange
	Params   ByteRange
	Result   ByteRange
	BodyStart int
	Receiver ByteRange
}

// CallSite describes one resolved call to a function in a single file
// — where it lives + the byte range INSIDE its `(...)`. Used by
// call-site rewriting after a signature refactor that changes arity.
type CallSite struct {
	Line, Col      int
	ArgsInnerStart int
	ArgsInnerEnd   int
	CurrentArgs    []string
	Skipped        string // non-empty: skip reason
	HasSpread      bool
}

// FindFunctionSignature parses content with the language's grammar
// and returns the signature of the function declaration that contains
// (line, col). 1-based positions; nil result (with nil error) when
// no function declaration covers the position.
func FindFunctionSignature(language string, content []byte, line, col int) (*FunctionSignature, error) {
	ops, ok := langOpsFor(language)
	if !ok {
		return nil, fmt.Errorf("signature refactor not supported for language %q", language)
	}
	g := LanguageByName(language)
	if g == nil {
		return nil, fmt.Errorf("no tree-sitter grammar for %q", language)
	}
	parser := sitter.NewParser()
	parser.SetLanguage(g)
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

	// Walk the tree depth-first looking for the smallest declaration
	// node that contains (row, column). Per-language ops decide what
	// node types qualify (e.g. function_declaration, arrow_function).
	var found *sitter.Node
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if !pointInRange(row, column, n.StartPoint(), n.EndPoint()) {
			return
		}
		if ops.isSignatureNode(n) {
			found = n
		}
		count := int(n.NamedChildCount())
		for i := range count {
			walk(n.NamedChild(i))
		}
	}
	walk(root)
	if found == nil {
		return nil, nil
	}
	sig, err := ops.extractSignature(found, content)
	if err != nil {
		return nil, err
	}
	if sig != nil {
		sig.Language = language
	}
	return sig, nil
}

// FindCallSites walks `content` and returns every call expression
// whose target name matches `name`. Identifier calls and selector /
// attribute calls (`x.name(...)`) both qualify.
func FindCallSites(language string, content []byte, name string) ([]CallSite, error) {
	ops, ok := langOpsFor(language)
	if !ok {
		return nil, fmt.Errorf("call-site lookup not supported for language %q", language)
	}
	g := LanguageByName(language)
	if g == nil {
		return nil, fmt.Errorf("no tree-sitter grammar for %q", language)
	}
	parser := sitter.NewParser()
	parser.SetLanguage(g)
	tree, err := parser.ParseCtx(context.Background(), nil, content)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if tree == nil {
		return nil, nil
	}
	defer tree.Close()

	var sites []CallSite
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n.Type() == ops.callNodeType {
			if site, hit := ops.extractCallSite(n, content, name); hit {
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

// RewriteSignature applies ops to content and returns the new bytes
// plus the count of distinct edits made. Caller is responsible for
// writing the result to disk and refreshing any indices.
//
// Edits are applied in reverse byte order so offsets stay valid. A
// "Rename" op rewrites only the name range here — workspace-wide
// rename is the caller's responsibility (it crosses files).
func RewriteSignature(content []byte, sig *FunctionSignature, ops SignatureOps) ([]byte, int, error) {
	langOps, ok := langOpsFor(sig.Language)
	if !ok {
		return nil, 0, fmt.Errorf("signature rewrite not supported for language %q", sig.Language)
	}
	type edit struct {
		start, end int
		text       string
	}
	var edits []edit

	if ops.Rename != "" {
		edits = append(edits, edit{
			start: sig.Name.Start,
			end:   sig.Name.End,
			text:  ops.Rename,
		})
	}
	if ops.Params != nil {
		edits = append(edits, edit{
			start: sig.Params.Start,
			end:   sig.Params.End,
			text:  langOps.formatParams(ops.Params),
		})
	}
	if ops.Return != "" {
		if sig.Result.Empty() {
			start, text := langOps.insertResult(sig, ops.Return)
			edits = append(edits, edit{start: start, end: start, text: text})
		} else {
			edits = append(edits, edit{
				start: sig.Result.Start,
				end:   sig.Result.End,
				text:  langOps.formatResultReplace(ops.Return),
			})
		}
	}

	sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	out := append([]byte(nil), content...)
	for _, e := range edits {
		next := make([]byte, 0, len(out)-(e.end-e.start)+len(e.text))
		next = append(next, out[:e.start]...)
		next = append(next, e.text...)
		next = append(next, out[e.end:]...)
		out = next
	}
	return out, len(edits), nil
}

// RewriteCallSiteArgs computes the new inner-arg-list source for a
// single call site, given the target parameter list. Handles three
// cases:
//
//   - equal arity: keep existing args verbatim (no edit needed)
//   - shrinking: drop trailing args
//   - growing: pad with language-appropriate zero values
//
// Returns the new comma-joined inner text. Empty input + empty target
// produces "". Caller writes between the call's parens.
func RewriteCallSiteArgs(language string, currentArgs []string, params []Param) (string, error) {
	ops, ok := langOpsFor(language)
	if !ok {
		return "", fmt.Errorf("call-site rewrite not supported for language %q", language)
	}
	target := len(params)
	out := make([]string, 0, target)
	for i := 0; i < target; i++ {
		if i < len(currentArgs) {
			out = append(out, currentArgs[i])
		} else {
			out = append(out, ops.zeroValue(params[i].Type))
		}
	}
	return strings.Join(out, ", "), nil
}

// ZeroValue exposes the language's placeholder expression for a type.
// Useful when a caller wants to pre-compute the zero values rather
// than going through RewriteCallSiteArgs.
func ZeroValue(language string, typ string) string {
	if ops, ok := langOpsFor(language); ok {
		return ops.zeroValue(typ)
	}
	return "nil"
}

// langOps bundles the per-language operations FindFunctionSignature,
// FindCallSites, and RewriteSignature need. New languages add an
// entry to langOpsByName.
type langOps struct {
	isSignatureNode     func(*sitter.Node) bool
	extractSignature    func(*sitter.Node, []byte) (*FunctionSignature, error)
	callNodeType        string
	extractCallSite     func(*sitter.Node, []byte, string) (CallSite, bool)
	formatParams        func([]Param) string
	formatResultReplace func(string) string
	insertResult        func(*FunctionSignature, string) (int, string)
	zeroValue           func(string) string
}

func langOpsFor(language string) (*langOps, bool) {
	o, ok := langOpsByName[language]
	return o, ok
}

var langOpsByName = map[string]*langOps{
	"go":         goLangOps,
	"typescript": tsLangOps,
	"python":     pythonLangOps,
}

// ---------- Go ----------

var goLangOps = &langOps{
	isSignatureNode: func(n *sitter.Node) bool {
		switch n.Type() {
		case "function_declaration", "method_declaration":
			return true
		}
		return false
	},
	extractSignature: extractGoSignature,
	callNodeType:     "call_expression",
	extractCallSite:  extractGoCallSite,
	formatParams:     formatGoParams,
	formatResultReplace: func(typ string) string {
		return strings.TrimSpace(typ)
	},
	insertResult: func(sig *FunctionSignature, typ string) (int, string) {
		// Body starts at `{`; the byte before is a space. Insert
		// `<type> ` so we end up with `) <type> {`.
		return sig.BodyStart, strings.TrimSpace(typ) + " "
	},
	zeroValue: goZeroValueImpl,
}

func extractGoSignature(decl *sitter.Node, _ []byte) (*FunctionSignature, error) {
	sig := &FunctionSignature{Type: decl.Type()}
	if name := decl.ChildByFieldName("name"); name != nil {
		sig.Name = nodeRange(name)
	} else {
		return nil, fmt.Errorf("%s missing name field", decl.Type())
	}
	if params := decl.ChildByFieldName("parameters"); params != nil {
		sig.Params = nodeRange(params)
	} else {
		return nil, fmt.Errorf("%s missing parameters field", decl.Type())
	}
	if result := decl.ChildByFieldName("result"); result != nil {
		sig.Result = nodeRange(result)
	}
	if body := decl.ChildByFieldName("body"); body != nil {
		sig.BodyStart = int(body.StartByte())
	} else {
		return nil, fmt.Errorf("%s missing body field", decl.Type())
	}
	if recv := decl.ChildByFieldName("receiver"); recv != nil {
		sig.Receiver = nodeRange(recv)
	}
	return sig, nil
}

func extractGoCallSite(call *sitter.Node, content []byte, name string) (CallSite, bool) {
	fn := call.ChildByFieldName("function")
	if fn == nil {
		return CallSite{}, false
	}
	switch fn.Type() {
	case "identifier":
		if fn.Content(content) != name {
			return CallSite{}, false
		}
	case "selector_expression":
		field := fn.ChildByFieldName("field")
		if field == nil || field.Content(content) != name {
			return CallSite{}, false
		}
	default:
		return CallSite{}, false
	}
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return CallSite{}, false
	}
	return collectCallSite(call, args, content, "variadic_argument"), true
}

func formatGoParams(params []Param) string {
	if len(params) == 0 {
		return "()"
	}
	parts := make([]string, 0, len(params))
	for _, p := range params {
		if p.Name == "" {
			parts = append(parts, p.Type)
		} else {
			parts = append(parts, p.Name+" "+p.Type)
		}
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

func goZeroValueImpl(typ string) string {
	typ = strings.TrimSpace(typ)
	if strings.HasPrefix(typ, "*") {
		return "nil"
	}
	switch typ {
	case "string":
		return `""`
	case "bool":
		return "false"
	case "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64",
		"byte", "rune", "uintptr":
		return "0"
	case "float32", "float64", "complex64", "complex128":
		return "0"
	case "error", "any", "interface{}":
		return "nil"
	}
	if strings.HasPrefix(typ, "[]") || strings.HasPrefix(typ, "map[") ||
		strings.HasPrefix(typ, "chan ") || strings.HasPrefix(typ, "<-") ||
		strings.HasPrefix(typ, "func(") {
		return "nil"
	}
	return "*new(" + typ + ")"
}

// ---------- TypeScript / TSX / JSX ----------

var tsLangOps = &langOps{
	isSignatureNode: func(n *sitter.Node) bool {
		switch n.Type() {
		case "function_declaration", "method_definition", "arrow_function",
			"function_expression":
			return true
		}
		return false
	},
	extractSignature: extractTSSignature,
	callNodeType:     "call_expression",
	extractCallSite:  extractTSCallSite,
	formatParams:     formatTSParams,
	formatResultReplace: func(typ string) string {
		// The result range in TS includes the leading `: ` (it's a
		// `type_annotation` node). Replace verbatim with the same
		// shape.
		return ": " + strings.TrimSpace(typ)
	},
	insertResult: func(sig *FunctionSignature, typ string) (int, string) {
		// Params end at `)`. Insert `: <type>` immediately after to
		// land before the body or `=>` (arrow).
		return sig.Params.End, ": " + strings.TrimSpace(typ)
	},
	zeroValue: tsZeroValue,
}

func extractTSSignature(decl *sitter.Node, _ []byte) (*FunctionSignature, error) {
	sig := &FunctionSignature{Type: decl.Type()}
	if name := decl.ChildByFieldName("name"); name != nil {
		sig.Name = nodeRange(name)
	}
	// arrow_function has no name field — the name lives on the
	// enclosing variable_declarator. Walk up to find it.
	if sig.Name.Empty() && decl.Type() == "arrow_function" {
		if parent := decl.Parent(); parent != nil && parent.Type() == "variable_declarator" {
			if name := parent.ChildByFieldName("name"); name != nil {
				sig.Name = nodeRange(name)
			}
		}
	}
	if sig.Name.Empty() {
		return nil, fmt.Errorf("%s missing name (anonymous function not supported)", decl.Type())
	}
	if params := decl.ChildByFieldName("parameters"); params != nil {
		sig.Params = nodeRange(params)
	} else {
		return nil, fmt.Errorf("%s missing parameters field", decl.Type())
	}
	if result := decl.ChildByFieldName("return_type"); result != nil {
		sig.Result = nodeRange(result)
	}
	if body := decl.ChildByFieldName("body"); body != nil {
		sig.BodyStart = int(body.StartByte())
	} else {
		// Single-expression arrow has body as the expression itself
		// via the "body" field. If absent, fall back to params end.
		sig.BodyStart = sig.Params.End
	}
	return sig, nil
}

func extractTSCallSite(call *sitter.Node, content []byte, name string) (CallSite, bool) {
	fn := call.ChildByFieldName("function")
	if fn == nil {
		return CallSite{}, false
	}
	switch fn.Type() {
	case "identifier":
		if fn.Content(content) != name {
			return CallSite{}, false
		}
	case "member_expression":
		prop := fn.ChildByFieldName("property")
		if prop == nil || prop.Content(content) != name {
			return CallSite{}, false
		}
	default:
		return CallSite{}, false
	}
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return CallSite{}, false
	}
	return collectCallSite(call, args, content, "spread_element"), true
}

func formatTSParams(params []Param) string {
	if len(params) == 0 {
		return "()"
	}
	parts := make([]string, 0, len(params))
	for _, p := range params {
		if p.Type == "" {
			parts = append(parts, p.Name)
		} else {
			parts = append(parts, p.Name+": "+p.Type)
		}
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

func tsZeroValue(typ string) string {
	typ = strings.TrimSpace(typ)
	switch typ {
	case "string":
		return `""`
	case "number", "bigint":
		return "0"
	case "boolean":
		return "false"
	case "null":
		return "null"
	case "undefined", "void":
		return "undefined"
	case "any", "unknown", "object":
		return "null"
	}
	if strings.HasSuffix(typ, "[]") || strings.HasPrefix(typ, "Array<") || strings.HasPrefix(typ, "ReadonlyArray<") {
		return "[]"
	}
	if strings.HasPrefix(typ, "Record<") || strings.HasPrefix(typ, "Map<") || strings.HasPrefix(typ, "Set<") {
		return "null"
	}
	return "null"
}

// ---------- Python ----------

var pythonLangOps = &langOps{
	isSignatureNode: func(n *sitter.Node) bool {
		switch n.Type() {
		case "function_definition":
			return true
		}
		return false
	},
	extractSignature: extractPythonSignature,
	callNodeType:     "call",
	extractCallSite:  extractPythonCallSite,
	formatParams:     formatPythonParams,
	formatResultReplace: func(typ string) string {
		// Python's return_type range covers the type only; the `->`
		// is separate. Replacement is just the type.
		return strings.TrimSpace(typ)
	},
	insertResult: func(sig *FunctionSignature, typ string) (int, string) {
		// Insert ` -> <type>` right after `)`. The colon comes
		// after.
		return sig.Params.End, " -> " + strings.TrimSpace(typ)
	},
	zeroValue: pythonZeroValue,
}

func extractPythonSignature(decl *sitter.Node, _ []byte) (*FunctionSignature, error) {
	sig := &FunctionSignature{Type: decl.Type()}
	if name := decl.ChildByFieldName("name"); name != nil {
		sig.Name = nodeRange(name)
	} else {
		return nil, fmt.Errorf("%s missing name field", decl.Type())
	}
	if params := decl.ChildByFieldName("parameters"); params != nil {
		sig.Params = nodeRange(params)
	} else {
		return nil, fmt.Errorf("%s missing parameters field", decl.Type())
	}
	if result := decl.ChildByFieldName("return_type"); result != nil {
		sig.Result = nodeRange(result)
	}
	if body := decl.ChildByFieldName("body"); body != nil {
		sig.BodyStart = int(body.StartByte())
	} else {
		return nil, fmt.Errorf("%s missing body field", decl.Type())
	}
	return sig, nil
}

func extractPythonCallSite(call *sitter.Node, content []byte, name string) (CallSite, bool) {
	fn := call.ChildByFieldName("function")
	if fn == nil {
		return CallSite{}, false
	}
	switch fn.Type() {
	case "identifier":
		if fn.Content(content) != name {
			return CallSite{}, false
		}
	case "attribute":
		// `x.name(...)`: attribute field for the property.
		attr := fn.ChildByFieldName("attribute")
		if attr == nil || attr.Content(content) != name {
			return CallSite{}, false
		}
	default:
		return CallSite{}, false
	}
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return CallSite{}, false
	}
	// Python's spread forms: list_splat ("*args") and
	// dictionary_splat ("**kwargs"). Both make our positional
	// rewrite unsafe.
	return collectCallSite(call, args, content, "list_splat", "dictionary_splat"), true
}

func formatPythonParams(params []Param) string {
	if len(params) == 0 {
		return "()"
	}
	parts := make([]string, 0, len(params))
	for _, p := range params {
		if p.Type == "" {
			parts = append(parts, p.Name)
		} else {
			parts = append(parts, p.Name+": "+p.Type)
		}
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

func pythonZeroValue(typ string) string {
	typ = strings.TrimSpace(typ)
	switch typ {
	case "str":
		return `""`
	case "int":
		return "0"
	case "float":
		return "0.0"
	case "bool":
		return "False"
	case "bytes":
		return `b""`
	case "None", "":
		return "None"
	}
	if strings.HasPrefix(typ, "list") || strings.HasPrefix(typ, "List[") {
		return "[]"
	}
	if strings.HasPrefix(typ, "dict") || strings.HasPrefix(typ, "Dict[") {
		return "{}"
	}
	if strings.HasPrefix(typ, "tuple") || strings.HasPrefix(typ, "Tuple[") {
		return "()"
	}
	if strings.HasPrefix(typ, "set") || strings.HasPrefix(typ, "Set[") {
		return "set()"
	}
	if strings.HasPrefix(typ, "Optional[") {
		return "None"
	}
	return "None"
}

// ---------- shared helpers ----------

// nodeRange wraps StartByte/EndByte into a ByteRange.
func nodeRange(n *sitter.Node) ByteRange {
	return ByteRange{Start: int(n.StartByte()), End: int(n.EndByte())}
}

// collectCallSite reads the argument_list's children and returns a
// populated CallSite. spreadTypes is the set of named child types
// that mark a spread/variadic argument (language-specific); if any
// child matches, HasSpread is set and Skipped="spread-args".
func collectCallSite(call, args *sitter.Node, content []byte, spreadTypes ...string) CallSite {
	innerStart := int(args.StartByte()) + 1
	innerEnd := int(args.EndByte()) - 1
	if innerEnd < innerStart {
		innerEnd = innerStart
	}
	site := CallSite{
		Line:           int(call.StartPoint().Row) + 1,
		Col:            int(call.StartPoint().Column) + 1,
		ArgsInnerStart: innerStart,
		ArgsInnerEnd:   innerEnd,
	}
	count := int(args.NamedChildCount())
	spreadSet := make(map[string]struct{}, len(spreadTypes))
	for _, s := range spreadTypes {
		spreadSet[s] = struct{}{}
	}
	for i := range count {
		arg := args.NamedChild(i)
		site.CurrentArgs = append(site.CurrentArgs, arg.Content(content))
		if _, ok := spreadSet[arg.Type()]; ok {
			site.HasSpread = true
		}
	}
	if site.HasSpread {
		site.Skipped = "spread-args"
	}
	return site
}
