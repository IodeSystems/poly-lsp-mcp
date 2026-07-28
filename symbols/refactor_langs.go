package symbols

import (
	"fmt"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
)

// Signature-refactor support for the languages added after the original
// go/typescript/python trio. Each arm answers the same eight questions
// the langOps contract asks; the interesting differences are in where a
// grammar hides the name, the parameter list and the result type.
//
// SQL, XML and Markdown are deliberately absent: a signature refactor
// rewrites a callable AND its call sites, and those three have no call
// expression to rewrite. A SQL function comes closest, but its callers
// are strings inside other statements, not resolvable call nodes.

// ---------- Java ----------

var javaLangOps = &langOps{
	isSignatureNode: func(n *sitter.Node) bool {
		switch n.Type() {
		case "method_declaration", "constructor_declaration",
			"compact_constructor_declaration":
			return true
		}
		return false
	},
	extractSignature: extractJavaSignature,
	callNodeType:     "method_invocation",
	extractCallSite:  extractJavaCallSite,
	formatParams:     formatTypeFirstParams,
	formatResultReplace: func(typ string) string {
		return strings.TrimSpace(typ)
	},
	// A Java method always declares a result, so the insert path is only
	// reached for a CONSTRUCTOR, where the return type belongs
	// immediately before the name.
	insertResult: func(sig *FunctionSignature, typ string) (int, string) {
		return sig.Name.Start, strings.TrimSpace(typ) + " "
	},
	zeroValue: javaZeroValue,
}

func extractJavaSignature(decl *sitter.Node, _ []byte) (*FunctionSignature, error) {
	sig := &FunctionSignature{Type: decl.Type()}
	name := decl.ChildByFieldName("name")
	if name == nil {
		return nil, fmt.Errorf("%s missing name field", decl.Type())
	}
	sig.Name = nodeRange(name)
	params := decl.ChildByFieldName("parameters")
	if params == nil {
		return nil, fmt.Errorf("%s missing parameters field", decl.Type())
	}
	sig.Params = nodeRange(params)
	// Constructors have no `type`, which is correctly an empty Result.
	if result := decl.ChildByFieldName("type"); result != nil {
		sig.Result = nodeRange(result)
	}
	body := decl.ChildByFieldName("body")
	if body == nil {
		// An abstract or interface method is a signature with no body;
		// its declaration still ends at the semicolon.
		sig.BodyStart = int(decl.EndByte())
	} else {
		sig.BodyStart = int(body.StartByte())
	}
	return sig, nil
}

func extractJavaCallSite(call *sitter.Node, content []byte, name string) (CallSite, bool) {
	n := call.ChildByFieldName("name")
	if n == nil || n.Content(content) != name {
		return CallSite{}, false
	}
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return CallSite{}, false
	}
	// Java has no spread at the CALL site — varargs is a declaration-side
	// feature — so there is no spread node type to guard against.
	return collectCallSite(call, args, content), true
}

func javaZeroValue(typ string) string {
	switch strings.TrimSpace(typ) {
	case "boolean":
		return "false"
	case "char":
		return "'\\0'"
	case "byte", "short", "int", "long":
		return "0"
	case "float", "double":
		return "0"
	case "void":
		return ""
	}
	return "null"
}

// ---------- Kotlin ----------

var kotlinLangOps = &langOps{
	isSignatureNode: func(n *sitter.Node) bool {
		return n.Type() == "function_declaration"
	},
	extractSignature: extractKotlinSignature,
	callNodeType:     "call_expression",
	extractCallSite:  extractKotlinCallSite,
	formatParams:     formatNameColonParams,
	formatResultReplace: func(typ string) string {
		return strings.TrimSpace(typ)
	},
	// No declared result: append `: T` after the parameter list.
	insertResult: func(sig *FunctionSignature, typ string) (int, string) {
		return sig.Params.End, ": " + strings.TrimSpace(typ)
	},
	zeroValue: kotlinZeroValue,
}

func extractKotlinSignature(decl *sitter.Node, _ []byte) (*FunctionSignature, error) {
	// This grammar exposes no field names, so the three pieces are told
	// apart by position — see kotlinFuncParts.
	_, name, result := kotlinFuncParts(decl)
	if name == nil {
		return nil, fmt.Errorf("kotlin function_declaration missing name")
	}
	sig := &FunctionSignature{Type: decl.Type(), Name: nodeRange(name)}
	params := firstNamedChildOfType(decl, "function_value_parameters")
	if params == nil {
		return nil, fmt.Errorf("kotlin function_declaration missing parameter list")
	}
	sig.Params = nodeRange(params)
	if result != nil {
		sig.Result = nodeRange(result)
	}
	if body := firstNamedChildOfType(decl, "function_body"); body != nil {
		sig.BodyStart = int(body.StartByte())
	} else {
		sig.BodyStart = int(decl.EndByte())
	}
	return sig, nil
}

func extractKotlinCallSite(call *sitter.Node, content []byte, name string) (CallSite, bool) {
	if call.NamedChildCount() == 0 {
		return CallSite{}, false
	}
	callee := call.NamedChild(0)
	switch callee.Type() {
	case "simple_identifier":
		if callee.Content(content) != name {
			return CallSite{}, false
		}
	case "navigation_expression":
		// obj.method(...) — the called name is the navigation suffix.
		suffix := lastNamedChildOfType(callee, "navigation_suffix")
		if suffix == nil {
			return CallSite{}, false
		}
		id := firstNamedChildOfType(suffix, "simple_identifier")
		if id == nil || id.Content(content) != name {
			return CallSite{}, false
		}
	default:
		return CallSite{}, false
	}
	suffix := firstNamedChildOfType(call, "call_suffix")
	if suffix == nil {
		return CallSite{}, false
	}
	args := firstNamedChildOfType(suffix, "value_arguments")
	if args == nil {
		// A trailing-lambda call (`items.map { … }`) has no parenthesised
		// argument list to rewrite.
		return CallSite{}, false
	}
	return collectCallSite(call, args, content), true
}

func kotlinZeroValue(typ string) string {
	t := strings.TrimSpace(typ)
	if strings.HasSuffix(t, "?") {
		return "null"
	}
	switch t {
	case "Boolean":
		return "false"
	case "Byte", "Short", "Int", "Long":
		return "0"
	case "Float", "Double":
		return "0"
	case "Char":
		return "' '"
	case "String":
		return `""`
	case "Unit":
		return ""
	}
	return "null"
}

// ---------- Groovy ----------

var groovyLangOps = &langOps{
	isSignatureNode: func(n *sitter.Node) bool {
		switch n.Type() {
		case "function_definition", "function_declaration":
			return true
		}
		return false
	},
	extractSignature: extractGroovySignature,
	callNodeType:     "function_call",
	extractCallSite:  extractGroovyCallSite,
	formatParams:     formatTypeFirstParams,
	formatResultReplace: func(typ string) string {
		return strings.TrimSpace(typ)
	},
	// Groovy writes the type (or `def`) before the name.
	insertResult: func(sig *FunctionSignature, typ string) (int, string) {
		return sig.Name.Start, strings.TrimSpace(typ) + " "
	},
	zeroValue: func(typ string) string {
		switch strings.TrimSpace(typ) {
		case "boolean":
			return "false"
		case "byte", "short", "int", "long", "float", "double":
			return "0"
		case "void", "def":
			return ""
		}
		return "null"
	},
}

func extractGroovySignature(decl *sitter.Node, _ []byte) (*FunctionSignature, error) {
	// Groovy names a callable through `function`, not `name`.
	name := decl.ChildByFieldName("function")
	if name == nil {
		return nil, fmt.Errorf("%s missing function field", decl.Type())
	}
	sig := &FunctionSignature{Type: decl.Type(), Name: nodeRange(name)}
	params := decl.ChildByFieldName("parameters")
	if params == nil {
		return nil, fmt.Errorf("%s missing parameters field", decl.Type())
	}
	sig.Params = nodeRange(params)
	if result := decl.ChildByFieldName("type"); result != nil {
		sig.Result = nodeRange(result)
	}
	if body := decl.ChildByFieldName("body"); body != nil {
		sig.BodyStart = int(body.StartByte())
	} else {
		sig.BodyStart = int(decl.EndByte())
	}
	return sig, nil
}

func extractGroovyCallSite(call *sitter.Node, content []byte, name string) (CallSite, bool) {
	fn := call.ChildByFieldName("function")
	if fn == nil || fn.Content(content) != name {
		return CallSite{}, false
	}
	args := call.ChildByFieldName("args")
	if args == nil {
		return CallSite{}, false
	}
	return collectCallSite(call, args, content), true
}

// ---------- C / C++ ----------

var cLangOps = &langOps{
	isSignatureNode: func(n *sitter.Node) bool {
		switch n.Type() {
		case "function_definition":
			return true
		case "declaration", "field_declaration":
			// A prototype or an in-class method declaration: only a
			// declarator that is a FUNCTION qualifies.
			return cFunctionDeclarator(n) != nil
		}
		return false
	},
	extractSignature: extractCSignature,
	callNodeType:     "call_expression",
	extractCallSite:  extractCCallSite,
	formatParams:     formatTypeFirstParams,
	formatResultReplace: func(typ string) string {
		return strings.TrimSpace(typ)
	},
	// The result type precedes the whole declarator in C.
	insertResult: func(sig *FunctionSignature, typ string) (int, string) {
		return sig.Name.Start, strings.TrimSpace(typ) + " "
	},
	zeroValue: cZeroValue,
}

func extractCSignature(decl *sitter.Node, content []byte) (*FunctionSignature, error) {
	fd := cFunctionDeclarator(decl)
	if fd == nil {
		return nil, fmt.Errorf("%s has no function declarator", decl.Type())
	}
	name := cInnermostDeclarator(fd)
	if name == nil {
		return nil, fmt.Errorf("%s has no declared name", decl.Type())
	}
	// An out-of-line definition names itself `Widget::area`. The rename
	// target is the LEAF only — spanning the qualified name would delete
	// the scope and silently detach the definition from its class.
	if name.Type() == "qualified_identifier" {
		if _, leaf := cQualifiedParts(name, content); leaf != nil {
			name = leaf
		}
	}
	sig := &FunctionSignature{Type: decl.Type(), Name: nodeRange(name)}
	params := fd.ChildByFieldName("parameters")
	if params == nil {
		return nil, fmt.Errorf("%s missing parameter list", decl.Type())
	}
	sig.Params = nodeRange(params)
	// Pointer and reference depth lives in the declarator, not here, so a
	// `char *f()` reports a result of `char`. Rewriting the result
	// replaces that token only, which is what the caller asked for.
	if result := decl.ChildByFieldName("type"); result != nil {
		sig.Result = nodeRange(result)
	}
	if body := decl.ChildByFieldName("body"); body != nil {
		sig.BodyStart = int(body.StartByte())
	} else {
		sig.BodyStart = int(decl.EndByte())
	}
	return sig, nil
}

func extractCCallSite(call *sitter.Node, content []byte, name string) (CallSite, bool) {
	fn := call.ChildByFieldName("function")
	if fn == nil {
		return CallSite{}, false
	}
	switch fn.Type() {
	case "identifier", "qualified_identifier":
		if leafName(fn.Content(content), "::") != name {
			return CallSite{}, false
		}
	case "field_expression":
		// obj.method(...) / p->method(...)
		f := fn.ChildByFieldName("field")
		if f == nil || f.Content(content) != name {
			return CallSite{}, false
		}
	default:
		return CallSite{}, false
	}
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return CallSite{}, false
	}
	return collectCallSite(call, args, content), true
}

func cZeroValue(typ string) string {
	t := strings.TrimSpace(typ)
	if strings.HasSuffix(t, "*") {
		return "NULL"
	}
	switch t {
	case "bool":
		return "false"
	case "void":
		return ""
	case "char", "short", "int", "long", "unsigned", "size_t",
		"float", "double":
		return "0"
	}
	return "0"
}

// ---------- shared parameter renderers ----------

// formatTypeFirstParams renders `Type name`, the C/Java/Groovy order. An
// entry with no type degrades to the bare name, which Groovy allows.
func formatTypeFirstParams(params []Param) string {
	if len(params) == 0 {
		return "()"
	}
	parts := make([]string, 0, len(params))
	for _, p := range params {
		switch {
		case p.Type == "":
			parts = append(parts, p.Name)
		case p.Name == "":
			parts = append(parts, p.Type)
		default:
			parts = append(parts, p.Type+" "+p.Name)
		}
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

// formatNameColonParams renders `name: Type`, the Kotlin order.
func formatNameColonParams(params []Param) string {
	if len(params) == 0 {
		return "()"
	}
	parts := make([]string, 0, len(params))
	for _, p := range params {
		if p.Type == "" {
			parts = append(parts, p.Name)
			continue
		}
		parts = append(parts, p.Name+": "+p.Type)
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

// leafName returns the last segment of a qualified name.
func leafName(s, sep string) string {
	if i := strings.LastIndex(s, sep); i >= 0 {
		return s[i+len(sep):]
	}
	return s
}
