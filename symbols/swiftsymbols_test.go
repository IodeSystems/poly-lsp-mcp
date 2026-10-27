package symbols

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// swiftFixture reads one file of the Swift package fixture.
func swiftFixture(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "testdata", "fixtures", "swift", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestFileSymbolsSwiftNestingAndClasses(t *testing.T) {
	cases := map[string]map[string]string{
		"Sources/App/Greeter.swift": {
			"Foundation": "import",
			// protocol → interface; its requirements are members.
			"Greeting":        "interface",
			"Greeting.Output": "type",
			"Greeting.greet":  "method",
			"Greeting.name":   "field",
			"Greeter":         "class",
			"Greeter.Output":  "type",
			// Stored, static and computed properties are all fields.
			"Greeter.name":     "field",
			"Greeter.count":    "field",
			"Greeter.shared":   "field",
			"Greeter.greeting": "field",
			// Named as Swift spells them; overloads disambiguate.
			"Greeter.init[1]":   "ctor",
			"Greeter.init[2]":   "ctor",
			"Greeter.deinit":    "ctor",
			"Greeter.greet":     "method",
			"Greeter.subscript": "method",
			// Nested types, and a nested enum's cases.
			"Greeter.Config":            "struct",
			"Greeter.Config.loud":       "field",
			"Greeter.Config.Level":      "enum",
			"Greeter.Config.Level.low":  "const",
			"Greeter.Config.Level.high": "const",
			// Attributes are annotations of the symbol they mark.
			"Greeter.MainActor":           "annotation",
			"Greeter.name.objc":           "annotation",
			"Greeter.greet.objc":          "annotation",
			"Greeter.greet.who":           "argument",
			"Greeter.greet.times":         "argument",
			"Greeter.greet.String":        "return",
			"Greeter.subscript.index":     "argument",
			"Greeter.init[1].name":        "argument",
			"Greeter.subscript.Character": "return",
			// The same-file extension merged into the class.
			"Greeter.wave":        "method",
			"Greeter.wave.target": "argument",
			"Greeter.wave.Bool":   "return",
			"Greeter.shout":       "field",
			// An extension of a type declared elsewhere: filed under it.
			"String.shouted": "method",
		},
		"Sources/App/Models.swift": {
			"Direction":           "enum",
			"Direction.north":     "const",
			"Direction.south":     "const",
			"Direction.east":      "const",
			"Direction.other":     "const",
			"Direction.opposite":  "field",
			"Shape.circle":        "const",
			"Shape.rect":          "const",
			"Point":               "struct",
			"Point.x":             "field",
			"Point.+":             "method",
			"Point.move":          "method",
			"Point.move.delta":    "argument",
			"Callback":            "type",
			"origin":              "const",
			"makeGreeter":         "func",
			"makeGreeter.name":    "argument",
			"makeGreeter.Greeter": "return",
			// An actor is a class to a selector.
			"Counter":           "class",
			"Counter.value":     "field",
			"Counter.increment": "method",
		},
	}
	for file, want := range cases {
		syms, err := FileSymbols("swift", swiftFixture(t, file))
		if err != nil {
			t.Fatal(err)
		}
		for sym, class := range want {
			got := symByPath(syms, sym)
			if got == nil {
				t.Errorf("%s: missing %q; have %v", file, sym, sortedSyms(syms))
				continue
			}
			if got.Class != class {
				t.Errorf("%s: %q class = %q, want %q", file, sym, got.Class, class)
			}
		}
		// Every sym path is unique within the file: an address names one
		// node.
		seen := map[string]bool{}
		for _, s := range syms {
			if seen[s.Sym] {
				t.Errorf("%s: %q emitted twice", file, s.Sym)
			}
			seen[s.Sym] = true
		}
	}
}

// The ranges node_read and node_edit act on: the declaration owns its doc
// comment and its attributes, the name range is the identifier, and the
// body split is the line the body opens on.
func TestFileSymbolsSwiftRanges(t *testing.T) {
	src := string(swiftFixture(t, "Sources/App/Greeter.swift"))
	syms, err := FileSymbols("swift", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	decl := map[string]string{
		"Greeter.greet": "/* Greets someone. */\n" +
			"    @objc func greet(_ who: String, times: Int = 1) -> String {\n" +
			"        count += times\n" +
			"        return who\n" +
			"    }",
		"Greeter.name": "/// Who to greet by default.\n" +
			"    @objc dynamic var name: String = \"world\"",
		"Greeter.count":     "private(set) var count = 0 // greetings so far",
		"Greeting.greet":    "/// Says hello.\n    func greet(_ who: String, times: Int) -> String",
		"Greeter.wave":      "/// Waves.\n    func wave(at target: Greeter) async throws -> Bool {\n        return true\n    }",
		"Greeter.deinit":    "deinit {\n        print(\"bye\")\n    }",
		"Greeter.greet.who": "_ who: String",
	}
	for sym, want := range decl {
		s := symByPath(syms, sym)
		if s == nil {
			t.Errorf("missing %q", sym)
			continue
		}
		if got := spanTextOf(src, *s); got != want {
			t.Errorf("%s decl = %q, want %q", sym, got, want)
		}
	}
	name := map[string]string{
		"Greeter.greet":     "greet",
		"Greeter.name":      "name",
		"Greeter.greet.who": "who",
		"Greeter.Config":    "Config",
	}
	for sym, want := range name {
		s := symByPath(syms, sym)
		if s == nil {
			t.Errorf("missing %q", sym)
			continue
		}
		n := *s
		n.DeclStartLine, n.DeclStartCol, n.DeclEndLine, n.DeclEndCol = s.NameStartLine, s.NameStartCol, s.NameEndLine, s.NameEndCol
		if got := spanTextOf(src, n); got != want {
			t.Errorf("%s name = %q, want %q", sym, got, want)
		}
	}
	// init has no name token, only the keyword: the name range falls back
	// to the declaration so rename cannot pin on `init`.
	if s := symByPath(syms, "Greeter.init[1]"); s == nil || s.NameStartLine != s.DeclStartLine || s.NameEndLine != s.DeclEndLine {
		t.Errorf("init name range = %+v, want the declaration span", s)
	}
	// ::comment and ::body
	if s := symByPath(syms, "Greeter"); s == nil || s.CommentStartLine != 11 || s.CommentEndLine != 11 {
		t.Errorf("Greeter doc comment = %+v, want line 11", s)
	}
	if s := symByPath(syms, "Greeter.greet"); s == nil || s.BodyStartLine != 41 {
		t.Errorf("Greeter.greet body line = %+v, want 41", s)
	}
}

// Extension placement: merged into a type declared in the same file, so an
// overload split across the two is greet[1] / greet[2] rather than two
// colliding Greeter.greet; filed under the extended type otherwise, with
// several extensions of that type sharing one level.
func TestFileSymbolsSwiftExtensions(t *testing.T) {
	src := []byte(`class Greeter {
    func greet(_ who: String) {}
}

extension Greeter {
    func greet(times: Int) {}
    struct Inner { func f() {} }
}

extension String {
    func pad() -> String { self }
}

extension String {
    func pad(to n: Int) -> String { self }
    var loud: String { uppercased() }
}

extension Greeter.Inner {
    func g() {}
}
`)
	syms, err := FileSymbols("swift", src)
	if err != nil {
		t.Fatal(err)
	}
	for _, sym := range []string{
		"Greeter.greet[1]", "Greeter.greet[2]",
		"Greeter.Inner", "Greeter.Inner.f",
		// A nested type declared in a merged extension still takes its
		// own extensions.
		"Greeter.Inner.g",
		"String.pad[1]", "String.pad[2]", "String.loud",
	} {
		if symByPath(syms, sym) == nil {
			t.Errorf("missing %q; have %v", sym, sortedSyms(syms))
		}
	}
	if s := symByPath(syms, "String.pad[1]"); s != nil && s.Class != "method" {
		t.Errorf("String.pad[1] class = %q, want method", s.Class)
	}
	// The extension block itself is not a node.
	for _, s := range syms {
		if s.Sym == "String" || s.Sym == "Greeter[2]" {
			t.Errorf("extension emitted as a node: %+v", s)
		}
	}
	// A merged member is a path child of its type, so the type precedes it
	// (the node tree is built parent-first).
	idx := map[string]int{}
	for i, s := range syms {
		idx[s.Sym] = i
	}
	if idx["Greeter"] > idx["Greeter.greet[2]"] {
		t.Errorf("merged member emitted before its type")
	}
}

func TestClassifySwiftWalksThroughParseErrors(t *testing.T) {
	if got := classifySwift("ERROR", "class_body"); got != roleContainer {
		t.Errorf("ERROR role = %v, want container", got)
	}
}

func TestSwiftExtractorSkipsCommentsAndStrings(t *testing.T) {
	src := []byte(`import UIKit
// mentions ghostInComment
let label = "ghostInString \(interpolated)"
func scroll(up: Bool) -> Int { [1].map { $0 + 1 }.first! }
`)
	names := hitNames(DefaultExtractor("swift").Extract(src))
	for _, want := range []string{"UIKit", "label", "interpolated", "scroll", "up", "map"} {
		if !names[want] {
			t.Errorf("missing %q; have %v", want, names)
		}
	}
	for _, unwanted := range []string{"ghostInComment", "ghostInString", "Bool", "Int", "$0"} {
		if names[unwanted] {
			t.Errorf("%q leaked into the index", unwanted)
		}
	}
}

// Swift's call_expression is Kotlin's shape (callee first, arguments in a
// call_suffix), so the same positional rule classifies its calls.
func TestSiteKindsClassifiesSwiftCalls(t *testing.T) {
	// 1 class C {
	// 2   func m(s: String, helper: () -> Void) -> Int {
	// 3     helper()
	// 4     self.other(s)
	// 5     run(helper)
	// 6     a.b.c()
	// 7     return 0
	// 8   }
	// 9 }
	src := "class C {\n" +
		"  func m(s: String, helper: () -> Void) -> Int {\n" +
		"    helper()\n" +
		"    self.other(s)\n" +
		"    run(helper)\n" +
		"    a.b.c()\n" +
		"    return 0\n" +
		"  }\n" +
		"}\n"
	wants := []struct {
		line, col  int
		kind, what string
	}{
		{3, 5, "call", "bare invocation"},
		{4, 10, "call", "invocation through a receiver"},
		{5, 5, "call", "the callee of run(helper)"},
		{5, 9, "", "helper as an ARGUMENT is not a call"},
		{6, 9, "call", "only the LAST navigation suffix is the callee"},
		{2, 13, "type", "String in a parameter is a type"},
	}
	positions := make([][2]int, len(wants))
	for i, w := range wants {
		positions[i] = [2]int{w.line, w.col}
	}
	got := SiteKinds("swift", []byte(src), positions)
	lines := strings.Split(src, "\n")
	for i, w := range wants {
		if got[i].Kind != w.kind {
			t.Errorf("%d:%d (%s)\n  line: %s\n  want kind %q, got %q", w.line, w.col, w.what, lines[w.line-1], w.kind, got[i].Kind)
		}
	}
}
