package symbols

import (
	"strings"
	"testing"
)

func symByPath(syms []Symbol, sym string) *Symbol {
	for i := range syms {
		if syms[i].Sym == sym {
			return &syms[i]
		}
	}
	return nil
}

func TestFileSymbolsGoNestingAndClasses(t *testing.T) {
	src := []byte(`package main

const Pi = 3.14

type Server struct {
	Name string
}

func (s *Server) Start() error { return nil }

func Free() {}
`)
	syms, err := FileSymbols("go", src)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"Pi":           "const",
		"Server":       "struct",
		"Server.Name":  "field",
		"Server.Start": "method",
		"Free":         "func",
	}
	for sym, class := range cases {
		got := symByPath(syms, sym)
		if got == nil {
			t.Errorf("missing %q; have %+v", sym, syms)
			continue
		}
		if got.Class != class {
			t.Errorf("%q class = %q, want %q", sym, got.Class, class)
		}
		if got.DeclStartLine < 1 || got.DeclEndLine < got.DeclStartLine {
			t.Errorf("%q decl range malformed: %+v", sym, got)
		}
		if got.NameStartLine < 1 {
			t.Errorf("%q name range malformed: %+v", sym, got)
		}
	}
}

func TestFileSymbolsDisambiguatesSameNameSiblings(t *testing.T) {
	src := []byte("package main\n\nfunc init() {}\n\nfunc init() {}\n")
	syms, err := FileSymbols("go", src)
	if err != nil {
		t.Fatal(err)
	}
	if symByPath(syms, "init[1]") == nil || symByPath(syms, "init[2]") == nil {
		t.Errorf("expected init[1] and init[2]; have %+v", syms)
	}
	if symByPath(syms, "init") != nil {
		t.Errorf("bare init should not be emitted when there are duplicates")
	}
}

func TestFileSymbolsTypeScriptClassMembers(t *testing.T) {
	src := []byte(`export class UserService {
  name: string;
  constructor() {}
  getUser() { return ""; }
}
export enum Color { Red, Green }
`)
	syms, err := FileSymbols("typescript", src)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"UserService":             "class",
		"UserService.name":        "field",
		"UserService.constructor": "ctor",
		"UserService.getUser":     "method",
		"Color":                   "enum",
		"Color.Red":               "field",
	}
	for sym, class := range cases {
		got := symByPath(syms, sym)
		if got == nil {
			t.Errorf("missing %q; have %+v", sym, syms)
			continue
		}
		if got.Class != class {
			t.Errorf("%q class = %q, want %q", sym, got.Class, class)
		}
	}
}

func TestFileSymbolsUnsupportedLanguageErrors(t *testing.T) {
	if _, err := FileSymbols("yaml", []byte("a: 1")); err == nil {
		t.Error("expected error for language without a grammar")
	}
}

// ------------------------------------------------------- .argument nodes

// wantArgs asserts each sym path exists with class "argument" and a
// sane, non-degenerate range.
func wantArgs(t *testing.T, syms []Symbol, paths ...string) {
	t.Helper()
	for _, p := range paths {
		got := symByPath(syms, p)
		if got == nil {
			t.Errorf("missing argument %q; have %s", p, symPaths(syms))
			continue
		}
		if got.Class != "argument" {
			t.Errorf("%q class = %q, want argument", p, got.Class)
		}
		if got.DeclStartLine < 1 || got.DeclEndLine < got.DeclStartLine {
			t.Errorf("%q decl range malformed: %+v", p, got)
		}
		if got.NameStartLine < 1 {
			t.Errorf("%q name range malformed: %+v", p, got)
		}
	}
}

func symPaths(syms []Symbol) string {
	out := make([]string, 0, len(syms))
	for _, s := range syms {
		out = append(out, s.Sym+":"+s.Class)
	}
	return strings.Join(out, " ")
}

func TestFileSymbolsGoArguments(t *testing.T) {
	src := []byte(`package main

func Add(a, b int, name string, opts ...Opt) (int, error) { return 0, nil }

type Server struct{}

func (s *Server) Start(ctx context.Context, retries int) error { return nil }

func NoParams() {}
`)
	syms, err := FileSymbols("go", src)
	if err != nil {
		t.Fatal(err)
	}
	// Multi-name ("a, b int"), plain, and variadic params all land.
	wantArgs(t, syms, "Add.a", "Add.b", "Add.name", "Add.opts",
		"Server.Start.ctx", "Server.Start.retries")

	// The method RECEIVER is not a parameter — it lives on Go's
	// separate `receiver` field and must not be indexed as an argument.
	if got := symByPath(syms, "Server.Start.s"); got != nil {
		t.Errorf("receiver leaked in as an argument: %+v", got)
	}
	// A param list with no params yields no argument children.
	for _, s := range syms {
		if s.Class == "argument" && strings.HasPrefix(s.Sym, "NoParams.") {
			t.Errorf("NoParams should have no arguments, got %q", s.Sym)
		}
	}
	// Multi-name params share one parameter_declaration; their spans
	// must not overlap (each is its own identifier).
	a, b := symByPath(syms, "Add.a"), symByPath(syms, "Add.b")
	if a.DeclStartCol == b.DeclStartCol && a.DeclStartLine == b.DeclStartLine {
		t.Errorf("Add.a and Add.b have identical spans: %+v / %+v", a, b)
	}
}

func TestFileSymbolsGoArgumentsAnonymousAndDuplicate(t *testing.T) {
	// An unnamed param is anonymous ("[n]"); reuse of renderSegment
	// means duplicate names disambiguate with [n] too.
	syms, err := FileSymbols("go", []byte("package main\n\nfunc H(http.ResponseWriter) {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	wantArgs(t, syms, "H.[1]")
}

func TestFileSymbolsTypeScriptArguments(t *testing.T) {
	src := []byte(`function greet(name: string, age?: number, ...rest: any[]) {}
class C {
  method(a: string, b = 3) {}
  constructor(x: number) {}
}
`)
	syms, err := FileSymbols("typescript", src)
	if err != nil {
		t.Fatal(err)
	}
	wantArgs(t, syms, "greet.name", "greet.age", "greet.rest",
		"C.method.a", "C.method.b", "C.constructor.x")
}

func TestFileSymbolsTSXArguments(t *testing.T) {
	// .tsx files map to the "typescript" language name (backed by the
	// tsx grammar), so JSX-bearing content runs the same codepath.
	src := []byte(`function Comp({title, id}: Props, ref: Ref) {
  return <div>{title}</div>;
}
`)
	syms, err := FileSymbols("typescript", src)
	if err != nil {
		t.Fatal(err)
	}
	// A destructured param binds no single name → anonymous "[1]".
	wantArgs(t, syms, "Comp.[1]", "Comp.ref")
}

func TestFileSymbolsPythonArguments(t *testing.T) {
	src := []byte(`def add(a, b: int = 3, *args, **kwargs):
    pass

class C:
    def __init__(self, x: int):
        pass

    def meth(self, y):
        pass
`)
	syms, err := FileSymbols("python", src)
	if err != nil {
		t.Fatal(err)
	}
	wantArgs(t, syms, "add.a", "add.b", "add.args", "add.kwargs",
		"C.__init__.self", "C.__init__.x", "C.meth.self", "C.meth.y")
}

func TestFileSymbolsArgumentsNestUnderOwner(t *testing.T) {
	// An argument's sym path is always its owner's path + one segment,
	// which is what makes `.func:has(.argument#x)` / :has_parent work.
	syms, err := FileSymbols("go", []byte("package main\n\nfunc F(x int) {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	owner := symByPath(syms, "F")
	arg := symByPath(syms, "F.x")
	if owner == nil || arg == nil {
		t.Fatalf("missing F or F.x: %s", symPaths(syms))
	}
	// The argument's decl range sits inside its owner's.
	if arg.DeclStartLine < owner.DeclStartLine || arg.DeclEndLine > owner.DeclEndLine {
		t.Errorf("arg range %+v not contained in owner range %+v", arg, owner)
	}
}

func TestFileSymbolsJavaClassMembers(t *testing.T) {
	src := []byte(`package com.termux.view;

import android.view.KeyEvent;

public interface TerminalViewClient {
    int SCROLL_KEYS_ARROWS = 0;

    int getScrollKeysBehaviour();
}

class TerminalView {
    private int mTopRow;
    private static final String KEY = "scroll_behaviour";

    TerminalView(Context context) {}

    public void scrollOneUnit(boolean up, int keys) {}
}

enum Mode { ARROWS, PAGES }
`)
	syms, err := FileSymbols("java", src)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		// Leaf segment, matching Go's package_clause: the bare package
		// name, not the fully-qualified path.
		"view":                                  "module",
		"KeyEvent":                              "import",
		"TerminalViewClient":                    "interface",
		"TerminalViewClient.SCROLL_KEYS_ARROWS": "field",
		"TerminalViewClient.getScrollKeysBehaviour": "method",
		"TerminalView":               "class",
		"TerminalView.mTopRow":       "field",
		"TerminalView.KEY":           "field",
		"TerminalView.TerminalView":  "ctor",
		"TerminalView.scrollOneUnit": "method",
		"Mode":                       "enum",
		"Mode.ARROWS":                "const",
	}
	for sym, class := range cases {
		got := symByPath(syms, sym)
		if got == nil {
			t.Errorf("missing %q; have %+v", sym, syms)
			continue
		}
		if got.Class != class {
			t.Errorf("%q class = %q, want %q", sym, got.Class, class)
		}
	}
}

func TestFileSymbolsJavaArguments(t *testing.T) {
	src := []byte(`class A {
    void scroll(boolean up, int keys) {}
    void varargs(String... parts) {}
}
`)
	syms, err := FileSymbols("java", src)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"A.scroll.up", "A.scroll.keys", "A.varargs.parts"} {
		if symByPath(syms, want) == nil {
			t.Errorf("missing argument %q; have %+v", want, syms)
		}
	}
}

func TestFileSymbolsCNestingAndClasses(t *testing.T) {
	src := []byte(`#include <stdio.h>
#include "app/widget.h"

#define MAX_SIZE 128
#define SQUARE(x) ((x) * (x))

typedef struct Point {
	int x;
	int y;
} Point;

typedef unsigned long ulong_t;

enum Color { RED, GREEN, BLUE };

union Value {
	int i;
	float f;
};

static int counter = 0;
int a, b;

// adds two numbers
int add(int lhs, int rhs) { return lhs + rhs; }

struct Point *make_point(void);
`)
	syms, err := FileSymbols("c", src)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		// #include answers to the header's base name, extension cut —
		// a dot would read as a nested path.
		"stdio":    "import",
		"widget":   "import",
		"MAX_SIZE": "const",
		"SQUARE":   "func",
		// A typedef'd struct IS the type declaration in C's dominant
		// idiom, so it takes the underlying kind and keeps its fields.
		"Point":     "struct",
		"Point.x":   "field",
		"Point.y":   "field",
		"ulong_t":   "type",
		"Color":     "enum",
		"Color.RED": "const",
		// No union class in the vocabulary — a union answers to struct.
		"Value":   "struct",
		"Value.i": "field",
		"counter": "var",
		// `int a, b;` is ONE declaration with two declarators; both are
		// symbols, which is why declarators (not declarations) are.
		"a":          "var",
		"b":          "var",
		"add":        "func",
		"add.lhs":    "argument",
		"add.rhs":    "argument",
		"make_point": "func",
	}
	for sym, class := range cases {
		got := symByPath(syms, sym)
		if got == nil {
			t.Errorf("missing %q; have %+v", sym, syms)
			continue
		}
		if got.Class != class {
			t.Errorf("%q class = %q, want %q", sym, got.Class, class)
		}
	}
	// `int add(...)` — the decl range starts at the doc comment, not at
	// the type, and `(void)` is not a parameter.
	if got := symByPath(syms, "add"); got != nil && got.DeclStartLine != 24 {
		t.Errorf("add decl starts at line %d, want 24 (the doc comment)", got.DeclStartLine)
	}
	if got := symByPath(syms, "make_point.[1]"); got != nil {
		t.Errorf("(void) became an argument node: %+v", got)
	}
	// Return types answer to the bare type name; `struct Point` keeps
	// its full spelling as the alias.
	ret := symByPath(syms, "make_point.Point")
	if ret == nil || ret.Class != "return" {
		t.Fatalf("missing return node make_point.Point; have %+v", syms)
	}
	if ret.Alias != "struct Point" {
		t.Errorf("return alias = %q, want %q", ret.Alias, "struct Point")
	}
}

func TestFileSymbolsCppNestingAndClasses(t *testing.T) {
	src := []byte(`#include <vector>
namespace app {

using Alias = int;

class Widget : public Base {
public:
	Widget(int id);
	~Widget();
	virtual int area() const;
	static Widget *create(const std::string &name, int count = 0);
	int width;
private:
	std::vector<int> items_;
};

int Widget::area() const { return width; }

enum class Mode { Fast, Slow };

int free_fn(int a, int b) { return a + b; }

}
`)
	syms, err := FileSymbols("cpp", src)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"vector":                 "import",
		"app":                    "module",
		"app.Alias":              "type",
		"app.Widget":             "class",
		"app.Widget.Widget":      "ctor",
		"app.Widget.~Widget":     "ctor",
		"app.Widget.area":        "method",
		"app.Widget.create":      "method",
		"app.Widget.create.name": "argument",
		"app.Widget.width":       "field",
		"app.Widget.items_":      "field",
		"app.Mode":               "enum",
		"app.Mode.Fast":          "const",
		"app.free_fn":            "func",
		"app.free_fn.a":          "argument",
	}
	for sym, class := range cases {
		got := symByPath(syms, sym)
		if got == nil {
			t.Errorf("missing %q; have %+v", sym, syms)
			continue
		}
		if got.Class != class {
			t.Errorf("%q class = %q, want %q", sym, got.Class, class)
		}
	}
	// The out-of-line definition owns the same path as the in-class
	// declaration — both are real, so both are emitted; the definition
	// is the one carrying a body.
	var defs int
	for _, s := range syms {
		if s.Sym == "app.Widget.area" {
			defs++
		}
	}
	if defs != 2 {
		t.Errorf("app.Widget.area emitted %d times, want 2 (declaration + out-of-line definition)", defs)
	}
	// A qualified return type answers to its leaf, full spelling aliased.
	ret := symByPath(syms, "app.Widget.create.Widget")
	if ret == nil || ret.Class != "return" {
		t.Errorf("missing return node app.Widget.create.Widget; have %+v", syms)
	}
}

func TestFileSymbolsCppHeaderGuardAndExternC(t *testing.T) {
	// A header wraps its whole body in an #ifndef guard and often an
	// extern "C" block; without descending through both, a .h file
	// indexes as empty.
	src := []byte(`#ifndef APP_WIDGET_H
#define APP_WIDGET_H

#ifdef __cplusplus
extern "C" {
#endif

int c_fn(void);

#ifdef __cplusplus
}
#endif

struct Pair { int x, y; };
int App::counter = 5;

#endif
`)
	syms, err := FileSymbols("cpp", src)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"APP_WIDGET_H": "const",
		"c_fn":         "func",
		"Pair":         "struct",
		"Pair.x":       "field",
		"Pair.y":       "field",
		// An out-of-line member definition is owned by its qualified
		// scope, not by the file — the C++ analogue of a Go receiver.
		"App.counter": "var",
	}
	for sym, class := range cases {
		got := symByPath(syms, sym)
		if got == nil {
			t.Errorf("missing %q; have %+v", sym, syms)
			continue
		}
		if got.Class != class {
			t.Errorf("%q class = %q, want %q", sym, got.Class, class)
		}
	}
}

func TestFileSymbolsKotlinNestingAndClasses(t *testing.T) {
	src := []byte(`package com.example.app

import android.view.KeyEvent
import kotlin.math.max as maximum

const val TOP_LEVEL = 1

typealias Handler = (String) -> Unit

interface Client {
    val scrollKeys: Int
    fun onKey(event: KeyEvent): Boolean
}

// A widget.
@Suppress("unused")
class Widget(val id: Int, private var name: String) : Client {
    companion object {
        const val MAX = 10
        fun create(): Widget = Widget(0, "")
    }

    private val items = mutableListOf<Int>()
    var width: Int = 0
        get() = field

    constructor(id: Int) : this(id, "")

    override fun onKey(event: KeyEvent): Boolean = true
}

enum class Mode { FAST, SLOW }

object Registry {
    fun get(): Int = 1
}

data class Pair2(val a: Int, val b: String)

fun String.shout(): String = this.uppercase()

fun freeFn(a: Int, vararg rest: String): Int = a
`)
	syms, err := FileSymbols("kotlin", src)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		// Leaf segment, matching Go's package_clause and Java's
		// package_declaration.
		"app": "module",
		// An aliased import answers to the alias — the name the rest of
		// the file writes.
		"KeyEvent":  "import",
		"maximum":   "import",
		"TOP_LEVEL": "const",
		"Handler":   "type",
		// `interface` is an anonymous token in this grammar, read off
		// the unnamed children.
		"Client":            "interface",
		"Client.scrollKeys": "field",
		"Client.onKey":      "method",
		"Widget":            "class",
		// A primary-constructor `val` declares a PROPERTY, so it is a
		// field on the class, not just a parameter.
		"Widget.id":   "field",
		"Widget.name": "field",
		// companion object members land directly on the class — how the
		// code addresses them (Widget.MAX).
		"Widget.MAX":      "field",
		"Widget.create":   "method",
		"Widget.items":    "field",
		"Widget.width":    "field",
		"Widget.Widget":   "ctor",
		"Widget.onKey":    "method",
		"Mode":            "enum",
		"Mode.FAST":       "const",
		"Registry":        "class",
		"Registry.get":    "method",
		"Pair2":           "struct",
		"Pair2.a":         "field",
		"String.shout":    "method",
		"freeFn":          "func",
		"freeFn.a":        "argument",
		"freeFn.rest":     "argument",
		"Widget.Suppress": "annotation",
	}
	for sym, class := range cases {
		got := symByPath(syms, sym)
		if got == nil {
			t.Errorf("missing %q; have %+v", sym, syms)
			continue
		}
		if got.Class != class {
			t.Errorf("%q class = %q, want %q", sym, got.Class, class)
		}
	}
	// A custom accessor is a SIBLING of its property in this grammar and
	// carries no name; emitting it would add anonymous noise.
	for _, s := range syms {
		if strings.HasPrefix(s.Sym, "Widget.[") {
			t.Errorf("anonymous node emitted for a getter/setter: %+v", s)
		}
	}
	// Kotlin spells doc comments line_comment, so the shared comment
	// logic has to accept both; without it the decl range stops at the
	// annotation.
	if got := symByPath(syms, "Widget"); got != nil && got.DeclStartLine != 15 {
		t.Errorf("Widget decl starts at line %d, want 15 (the doc comment)", got.DeclStartLine)
	}
}

func TestFileSymbolsKotlinReturnsAndReceivers(t *testing.T) {
	src := []byte(`fun ids(): List<Int> = listOf()
fun String.shout(): kotlin.text.Regex? = null
class A {
    fun plain() {}
}
`)
	syms, err := FileSymbols("kotlin", src)
	if err != nil {
		t.Fatal(err)
	}
	// Generics and the package qualifier come off the segment; the full
	// spelling is kept as the alias.
	ret := symByPath(syms, "ids.List")
	if ret == nil {
		t.Fatalf("missing return node ids.List; have %+v", syms)
	}
	if ret.Class != "return" || ret.Alias != "List<Int>" {
		t.Errorf("ids return = %+v, want class=return alias=List<Int>", ret)
	}
	// An extension function is filed under its receiver, and the type
	// BEFORE the name must not be mistaken for a result.
	if got := symByPath(syms, "String.shout.Regex"); got == nil {
		t.Errorf("missing String.shout.Regex; have %+v", syms)
	}
	// No declared return type: no return node at all.
	for _, s := range syms {
		if s.Class == "return" && strings.HasPrefix(s.Sym, "A.plain") {
			t.Errorf("synthesized a return node for an implicit-Unit function: %+v", s)
		}
	}
}

func TestFileSymbolsGroovyNestingAndClasses(t *testing.T) {
	src := []byte(`package com.example.app

import groovy.transform.CompileStatic
import java.util.List as JList

String greeting = "hi"

interface Client {
    int onKey(String event)
}

// A widget.
@CompileStatic
class Widget implements Client {
    static final int MAX = 10
    private String name

    int onKey(String event) { return 1 }

    def scroll(boolean up, int keys = 0) {}
}

enum Mode { FAST, SLOW }
`)
	syms, err := FileSymbols("groovy", src)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"app":           "module",
		"CompileStatic": "import",
		// An aliased import answers to the alias.
		"JList":    "import",
		"greeting": "var",
		// `class` and `interface` are unnamed keyword TOKENS here, not
		// modifiers, so the kind is read off the unnamed children.
		"Client":               "interface",
		"Client.onKey":         "method",
		"Client.onKey.event":   "argument",
		"Client.onKey.int":     "return",
		"Widget":               "class",
		"Widget.CompileStatic": "annotation",
		"Widget.MAX":           "field",
		"Widget.name":          "field",
		"Widget.onKey":         "method",
		"Widget.scroll":        "method",
		"Widget.scroll.up":     "argument",
		"Widget.scroll.keys":   "argument",
		// The grammar models neither `enum` nor `trait`, reading the
		// keyword as a type name; the declaration is still real.
		"Mode": "enum",
	}
	for sym, class := range cases {
		got := symByPath(syms, sym)
		if got == nil {
			t.Errorf("missing %q; have %+v", sym, syms)
			continue
		}
		if got.Class != class {
			t.Errorf("%q class = %q, want %q", sym, got.Class, class)
		}
	}
	// `def` occupies the type field but means "no declared type"; a
	// return node named def would answer return#def for every
	// dynamically-typed method in the file.
	for _, s := range syms {
		if s.Class == "return" && strings.HasSuffix(s.Sym, ".def") {
			t.Errorf("def was emitted as a return type: %+v", s)
		}
	}
	// The class carries an `implements` clause, which this grammar
	// parses as an ERROR node under the SAME `name` field as the real
	// identifier. The name must still be the class.
	if got := symByPath(syms, "Widget"); got != nil && got.DeclStartLine != 12 {
		t.Errorf("Widget decl starts at line %d, want 12 (the doc comment)", got.DeclStartLine)
	}
}

func TestFileSymbolsGroovyDeclinesDSLJuxtaposition(t *testing.T) {
	// A declarative Jenkins pipeline has no declarations, only DSL
	// calls — but `agent any` parses as a `declaration` with type=agent
	// and name=any, exactly like `String x`. Emitting it would invent a
	// variable named `any` in every Jenkinsfile.
	src := []byte(`pipeline {
  agent any
  def buildVersion = "1.0"
  stages {
    stage('Build') {
      steps { sh 'make' }
    }
  }
}
`)
	syms, err := FileSymbols("groovy", src)
	if err != nil {
		t.Fatal(err)
	}
	if got := symByPath(syms, "any"); got != nil {
		t.Errorf("DSL juxtaposition emitted a symbol: %+v", got)
	}
	// A REAL declaration in the same block still surfaces.
	got := symByPath(syms, "buildVersion")
	if got == nil || got.Class != "var" {
		t.Errorf("buildVersion = %+v, want a var; have %+v", got, syms)
	}
}

func TestClassifyKotlinWalksThroughParseErrors(t *testing.T) {
	// A single unparsable statement deep inside a method re-labels the
	// whole enclosing class_body as ERROR, and the class then keeps its
	// name and loses EVERY member — while the ERROR node still holds all
	// of them as recovered children. Walking through ERROR reattaches
	// them.
	//
	// This asserts the routing decision, not an end-to-end recovery: the
	// failure was found on a live 504-file Kotlin repo
	// (E2ETestPlatform.kt, 22 -> 44 symbols with this in place) and does
	// NOT reduce to a minimal fixture — every candidate construct
	// extracted from that file parses cleanly on its own, so the trigger
	// needs whole-file context that cannot be checked in here.
	if got := classifyKotlin("ERROR", "class_declaration"); got != roleContainer {
		t.Errorf("classifyKotlin(ERROR) = %v, want roleContainer", got)
	}
	// The cost of that choice, stated so it is not mistaken for a bug:
	// when the broken region sits inside a method, that method's locals
	// flatten into the same ERROR and surface as fields of the class.
}

func TestTypeSegmentStripsGenericsAndArrays(t *testing.T) {
	// A `.return` node answers to a bare NAME. Measured on a 492-file
	// Java tree: 531 of 10,738 return nodes carried `<...>` or `[]` in
	// their path segment with an EMPTY alias, so `return#Field` could
	// not match a `Field<String>` result.
	cases := []struct{ in, seg, alias string }{
		{"String", "String", ""},
		{"Field<String>", "Field", "Field<String>"},
		{"Field<byte[]>", "Field", "Field<byte[]>"},
		{"Long[]", "Long", "Long[]"},
		{"String[][]", "String", "String[][]"},
		{"java.util.Map.Entry", "Entry", "java.util.Map.Entry"},
		{"kotlin.text.Regex?", "Regex", "kotlin.text.Regex?"},
		{"List<Map<String, Int>>", "List", "List<Map<String, Int>>"},
		{"RestResponse<DataSetResponse<AccountView>>", "RestResponse", "RestResponse<DataSetResponse<AccountView>>"},
		// A union has no single leaf to answer to; inventing one would
		// misstate which type the callable returns.
		{"string | null", "string | null", ""},
	}
	for _, c := range cases {
		seg, alias := typeSegment(c.in)
		if seg != c.seg || alias != c.alias {
			t.Errorf("typeSegment(%q) = (%q, %q), want (%q, %q)", c.in, seg, alias, c.seg, c.alias)
		}
	}
}

func TestFileSymbolsJavaGenericReturnAnswersToBareName(t *testing.T) {
	src := []byte(`class Repo {
    Field<String> nameField() { return null; }
    Long[] ids() { return null; }
}
`)
	syms, err := FileSymbols("java", src)
	if err != nil {
		t.Fatal(err)
	}
	got := symByPath(syms, "Repo.nameField.Field")
	if got == nil || got.Alias != "Field<String>" {
		t.Errorf("generic return = %+v, want path Repo.nameField.Field aliased Field<String>; have %+v", got, syms)
	}
	if got := symByPath(syms, "Repo.ids.Long"); got == nil || got.Alias != "Long[]" {
		t.Errorf("array return = %+v, want path Repo.ids.Long aliased Long[]", got)
	}
}

func TestFileSymbolsPythonIndexesBindings(t *testing.T) {
	// Python declares its module constants and its dataclass/pydantic
	// model fields as plain assignments. Measured on a live 194-file
	// tree: 891 such declarations were invisible while every other
	// language arm indexed its consts and fields.
	src := []byte(`import os

MAX_SIZE = 128
logger = get_logger(__name__)
a, b = compute()

class Settings(BaseSettings):
    api_title: str = "TTS API"
    port: int = 8880

    def reload(self) -> None:
        scratch = 1
        self.port = 0
`)
	syms, err := FileSymbols("python", src)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"MAX_SIZE":           "var",
		"logger":             "var",
		"Settings":           "class",
		"Settings.api_title": "field",
		"Settings.port":      "field",
		"Settings.reload":    "method",
	}
	for sym, class := range cases {
		got := symByPath(syms, sym)
		if got == nil {
			t.Errorf("missing %q; have %+v", sym, syms)
			continue
		}
		if got.Class != class {
			t.Errorf("%q class = %q, want %q", sym, got.Class, class)
		}
	}
	// No `const` class: Python has no const keyword, and UPPER_CASE is a
	// convention the parser cannot verify.
	if got := symByPath(syms, "MAX_SIZE"); got != nil && got.Class == "const" {
		t.Errorf("MAX_SIZE was guessed to be a const from its casing")
	}
	// Tuple unpacking binds no single name — DECLINED rather than given
	// a made-up segment.
	for _, s := range syms {
		if strings.HasPrefix(s.Sym, "a,") || s.Sym == "a" || s.Sym == "b" {
			t.Errorf("tuple unpacking emitted a symbol: %+v", s)
		}
	}
	// A function body is not walked, so its locals and its self-attribute
	// writes must not surface as module vars or class fields.
	for _, bad := range []string{"scratch", "Settings.reload.scratch", "Settings.port[2]"} {
		if got := symByPath(syms, bad); got != nil {
			t.Errorf("function-body statement leaked as a declaration: %+v", got)
		}
	}
}

func TestGoTypeSegmentKeepsDecorationButNotFalseLeaves(t *testing.T) {
	// Decoration is KEPT — `*Config` and `[]Schema` are how Go code names
	// the result, and existing sym paths depend on it.
	//
	// What is fixed is the blind dot-split reaching INSIDE a composite and
	// reporting its last identifier as the leaf: sitesByFile returns
	// map[string][]symbols.InvSite and used to answer to `return#InvSite`,
	// a type it does not return.
	cases := []struct{ in, seg, alias string }{
		{"error", "error", ""},
		{"*Config", "*Config", ""},
		{"[]Schema", "[]Schema", ""},
		{"io.Writer", "Writer", "io.Writer"},
		{"*sitter.Node", "Node", "*sitter.Node"},
		// Composites claim nothing.
		{"map[string][]symbols.InvSite", "map[string][]symbols.InvSite", ""},
		{"map[string]int", "map[string]int", ""},
		{"[]map[string]symbols.Hit", "[]map[string]symbols.Hit", ""},
		{"chan Result", "chan Result", ""},
		{"func() error", "func() error", ""},
		{"*[2]int", "*[2]int", ""},
		{"interface{}", "interface{}", ""},
	}
	for _, c := range cases {
		seg, alias := goTypeSegment(c.in)
		if seg != c.seg || alias != c.alias {
			t.Errorf("goTypeSegment(%q) = (%q, %q), want (%q, %q)", c.in, seg, alias, c.seg, c.alias)
		}
	}
}

func TestFileSymbolsGoMapReturnClaimsNoLeaf(t *testing.T) {
	src := []byte(`package x

import "io"

func Sites() map[string][]io.Writer { return nil }
`)
	syms, err := FileSymbols("go", src)
	if err != nil {
		t.Fatal(err)
	}
	// The old dot-split produced Sites.Writer — asserting the function
	// returns a Writer, which it does not.
	if got := symByPath(syms, "Sites.Writer"); got != nil {
		t.Errorf("map return claimed the leaf %q of its VALUE type: %+v", "Writer", got)
	}
	if got := symByPath(syms, "Sites.map[string][]io.Writer"); got == nil {
		t.Errorf("map return missing; have %+v", syms)
	}
}

func TestFileSymbolsSQLDeclarationKinds(t *testing.T) {
	// A migration tree is not just CREATE TABLE. Measured on a 34-file
	// Flyway corpus: 48 create_function, 80 create_trigger and 128
	// create_sequence went unindexed, leaving six files — every PL/pgSQL
	// function and trigger file — completely empty.
	src := []byte(`CREATE TABLE redline.account (
  account_id bigint NOT NULL,
  balance numeric
);

CREATE SEQUENCE redline.account_account_id_seq START WITH 1;

CREATE OR REPLACE FUNCTION redline.set_updated_at() RETURNS TRIGGER AS $$
BEGIN
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER account_updated_at BEFORE UPDATE ON redline.account
  FOR EACH ROW EXECUTE PROCEDURE redline.set_updated_at();
`)
	syms, err := FileSymbols("sql", src)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		// A schema-qualified name answers to its LEAF, like every other
		// language here — redline.account is `account`.
		"account":                "struct",
		"account.account_id":     "field",
		"account_account_id_seq": "type",
		"set_updated_at":         "func",
		"account_updated_at":     "type",
	}
	for sym, class := range cases {
		got := symByPath(syms, sym)
		if got == nil {
			t.Errorf("missing %q; have %+v", sym, syms)
			continue
		}
		if got.Class != class {
			t.Errorf("%q class = %q, want %q", sym, got.Class, class)
		}
	}
	// The schema must never become the symbol.
	if got := symByPath(syms, "redline"); got != nil {
		t.Errorf("schema qualifier was indexed as a symbol: %+v", got)
	}
}

func TestFileSymbolsSQLAddConstraint(t *testing.T) {
	// ADD CONSTRAINT is the largest declaration a migration tree carries
	// that the arm used to drop: 492 of 620 alter_table statements in a
	// real Flyway corpus, 246 distinct names. A constraint name is a
	// cross-language contract — Postgres reports it in violation errors
	// that application code catches by name.
	src := []byte(`CREATE TABLE redline.account (
  account_id bigint NOT NULL
);

ALTER TABLE ONLY redline.account
  ADD CONSTRAINT account_pkey PRIMARY KEY (account_id);

ALTER TABLE ONLY redline.account
  ADD CONSTRAINT account_user_fkey FOREIGN KEY (user_id) REFERENCES redline."USER"(user_id);

ALTER TABLE ONLY redline.account ALTER COLUMN account_id SET DEFAULT 1;
`)
	syms, err := FileSymbols("sql", src)
	if err != nil {
		t.Fatal(err)
	}
	// A constraint is filed under the table it is added to, the same way
	// a Go method is filed under its receiver.
	for _, want := range []string{"account.account_pkey", "account.account_user_fkey"} {
		got := symByPath(syms, want)
		if got == nil {
			t.Errorf("missing %q; have %+v", want, syms)
			continue
		}
		if got.Class != "type" {
			t.Errorf("%q class = %q, want type", want, got.Class)
		}
	}
	// ALTER COLUMN modifies an existing column and declares nothing, so
	// it must not emit a symbol.
	for _, s := range syms {
		if s.Class != "field" && strings.HasSuffix(s.Sym, "account_id") && s.DeclStartLine > 5 {
			t.Errorf("ALTER COLUMN emitted a declaration: %+v", s)
		}
	}
}

func TestFileSymbolsSQLConstraintWithoutItsTable(t *testing.T) {
	// The usual migration shape: the table was created in an EARLIER
	// file, so the prefix has no parent node here. The constraint still
	// carries the table in its path rather than landing bare at file
	// scope — the same behaviour as a Go method whose receiver type is
	// declared in another file.
	src := []byte(`ALTER TABLE ONLY redline.account
  ADD CONSTRAINT account_email_key UNIQUE (email);
`)
	syms, err := FileSymbols("sql", src)
	if err != nil {
		t.Fatal(err)
	}
	if got := symByPath(syms, "account.account_email_key"); got == nil {
		t.Errorf("missing account.account_email_key; have %+v", syms)
	}
	if got := symByPath(syms, "account_email_key"); got != nil {
		t.Errorf("constraint landed at file scope without its table: %+v", got)
	}
}

func TestFileSymbolsMarkdownSections(t *testing.T) {
	src := []byte(`# Top

Intro prose.

## Alpha

Alpha body with ` + "`UserID`" + `.

### Nested

deep

## Beta

Beta body.
`)
	syms, err := FileSymbols("markdown", src)
	if err != nil {
		t.Fatal(err)
	}
	// A document's outline IS its node tree: sections nest, and each
	// span covers the heading plus everything under it.
	want := map[string][2]int{
		"Top":              {1, 16},
		"Top.Alpha":        {5, 13},
		"Top.Alpha.Nested": {9, 13},
		"Top.Beta":         {13, 16},
	}
	for sym, span := range want {
		got := symByPath(syms, sym)
		if got == nil {
			t.Errorf("missing section %q; have %+v", sym, syms)
			continue
		}
		if got.Class != "heading" {
			t.Errorf("%q class = %q, want heading", sym, got.Class)
		}
		if got.DeclStartLine != span[0] || got.DeclEndLine != span[1] {
			t.Errorf("%q spans L%d-%d, want L%d-%d — a section owns its BODY, not just its heading",
				sym, got.DeclStartLine, got.DeclEndLine, span[0], span[1])
		}
	}
	if len(syms) != 4 {
		t.Errorf("got %d symbols, want 4 sections (paragraphs are content, not siblings): %+v", len(syms), syms)
	}
}

func TestFileSymbolsMarkdownUntitledPreambleDeclined(t *testing.T) {
	// Text above the first heading titles nothing. It must not become an
	// anonymous "[1]" node; its text still belongs to the file.
	src := []byte("Loose intro line.\n\n# Real\n\nbody\n")
	syms, err := FileSymbols("markdown", src)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range syms {
		if strings.Contains(s.Sym, "[") {
			t.Errorf("untitled preamble emitted an anonymous node: %+v", s)
		}
	}
	if got := symByPath(syms, "Real"); got == nil {
		t.Errorf("missing the titled section; have %+v", syms)
	}
}

func TestFileSymbolsJavaAnnotations(t *testing.T) {
	// Java is the language where annotations carry the most meaning, and
	// it was the only one of six with a node model that had none — so
	// `method:any(annotation#Transactional)` matched nothing.
	src := []byte(`@Entity
@Table(name = "users")
public class User {
    @Id
    @Column(name = "user_id")
    private Long id;

    @Override
    @com.example.Audited
    public String getName() { return null; }
}
`)
	syms, err := FileSymbols("java", src)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"User.Entity", "User.Table",
		// A FIELD is the wrinkle: the symbol is the variable_declarator
		// but the modifiers hang off the enclosing field_declaration.
		"User.id.Id", "User.id.Column",
		"User.getName.Override", "User.getName.Audited",
	} {
		got := symByPath(syms, want)
		if got == nil {
			t.Errorf("missing annotation %q; have %+v", want, syms)
			continue
		}
		if got.Class != "annotation" {
			t.Errorf("%q class = %q, want annotation", want, got.Class)
		}
	}
	// A qualified annotation answers to its leaf, keeping what was
	// written as the alias.
	if got := symByPath(syms, "User.getName.Audited"); got != nil && got.Alias != "com.example.Audited" {
		t.Errorf("qualified annotation alias = %q, want com.example.Audited", got.Alias)
	}
}

func TestFileSymbolsJavaModuleInfo(t *testing.T) {
	// module-info.java was the ONLY file of JDK 21's 3,498-file
	// java.base that indexed as completely empty.
	src := []byte(`module java.base {
    exports java.lang;
    exports java.io;
    requires transitive java.xml;
}
`)
	syms, err := FileSymbols("java", src)
	if err != nil {
		t.Fatal(err)
	}
	// The module answers to its LEAF, like a package declaration.
	got := symByPath(syms, "base")
	if got == nil {
		t.Fatalf("missing the module declaration; have %+v", syms)
	}
	if got.Class != "module" {
		t.Errorf("module class = %q, want module", got.Class)
	}
	// The directives are deliberately NOT symbols: their identity is a
	// full dotted path (java.lang, java.io) and a sym-path segment
	// cannot hold the dots, so they would collapse to generic leaves —
	// `lang`, `io`, `xml` — that collide and bury real names.
	if len(syms) != 1 {
		t.Errorf("got %d symbols, want exactly 1 (the module, not its directives): %+v",
			len(syms), syms)
	}
}
