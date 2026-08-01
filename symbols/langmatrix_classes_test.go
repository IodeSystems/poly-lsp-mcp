package symbols

// The bug classes asserted by TestLanguageBugMatrix. Kept beside the harness
// so adding a class is one function and a line in the classes slice.

// trailingCommentClass: `Enabled bool // why` — the comment documents the
// declaration it sits on. Reported from dogfooding 2026-08-01, when node_edit
// could not match a field line written the way it appears on screen; behind it
// the comment was being credited to the declaration BELOW, which in
// typescript, kotlin and c pulled a neighbour's comment into the next span and
// destroyed it on delete. See plan/bugs.md.
func trailingCommentClass() bugClass {
	return bugClass{
		name:     "a declaration owns the comment trailing it",
		property: "a comment after code on a line belongs to that line's declaration, and to no other",
		na: map[string]string{
			"markdown": "a section owns its body as prose; there is no declaration for a comment to trail",
			"xml":      "elements are addressed by tag through a non-grammar walk; XML comments are not modelled as docs",
		},
		fixtures: map[string]spanFixture{
			"go": {
				src: "package p\n\ntype C struct {\n\tA string // doc for A\n\tB bool   // doc for B\n\tD int\n}\n",
				want: map[string]string{
					"C.A": "A string // doc for A",
					"C.B": "B bool   // doc for B",
					"C.D": "D int",
				},
			},
			"typescript": {
				src: "export class C {\n  a: string = \"\"; // doc for a\n  b = false; // doc for b\n  d = 0;\n}\n",
				want: map[string]string{
					"C.a": "a: string = \"\"; // doc for a",
					"C.b": "b = false; // doc for b",
					"C.d": "d = 0",
				},
			},
			"python": {
				src: "class C:\n    a = \"\"  # doc for a\n    b = False  # doc for b\n    d = 0\n",
				want: map[string]string{
					"C.a": "a = \"\"  # doc for a",
					"C.b": "b = False  # doc for b",
					"C.d": "d = 0",
				},
			},
			"java": {
				src: "class C {\n  String a; // doc for a\n  String b; // doc for b\n  String d;\n}\n",
				want: map[string]string{
					"C.a": "String a; // doc for a",
					"C.b": "String b; // doc for b",
					"C.d": "String d;",
				},
			},
			"kotlin": {
				src: "class C {\n  val a: String = \"\" // doc for a\n  val b = false // doc for b\n  val d = 0\n}\n",
				want: map[string]string{
					"C.a": "val a: String = \"\" // doc for a",
					"C.b": "val b = false // doc for b",
					"C.d": "val d = 0",
				},
			},
			"groovy": {
				// Typed groovy fields (`String a`) are not indexed at all —
				// a separate, pre-existing gap in the speculative groovy arm.
				src: "class C {\n  def a = 1 // doc for a\n  def b = 2 // doc for b\n  def d = 3\n}\n",
				want: map[string]string{
					"C.a": "def a = 1 // doc for a",
					"C.b": "def b = 2 // doc for b",
					"C.d": "def d = 3",
				},
			},
			"c": {
				src: "struct C {\n  char *a; // doc for a\n  int b; // doc for b\n  int d;\n};\n",
				want: map[string]string{
					"C.a": "char *a; // doc for a",
					"C.b": "int b; // doc for b",
					"C.d": "int d;",
				},
			},
			"cpp": {
				src: "struct C {\n  int a; // doc for a\n  int b; // doc for b\n  int d;\n};\n",
				want: map[string]string{
					"C.a": "int a; // doc for a",
					"C.b": "int b; // doc for b",
					"C.d": "int d;",
				},
			},
			"sql": {
				src: "CREATE TABLE t (\n  a int, -- doc for a\n  b int -- doc for b\n);\n",
				want: map[string]string{
					"t.a": "a int, -- doc for a",
					"t.b": "b int -- doc for b",
				},
			},
		},
	}
}

// docBlockAboveClass: the contiguous comment block directly above a
// declaration is that declaration's documentation, and lives inside its span —
// so node_read returns documented code and node_edit cannot strand a comment
// describing code it just replaced. The older half of the same rule; see
// declLineCols.
func docBlockAboveClass() bugClass {
	return bugClass{
		name:     "a declaration owns the doc block above it",
		property: "a comment block on its own lines directly above a declaration is inside that declaration's span",
		na: map[string]string{
			"markdown": "a heading's prose IS its body; there is no separate doc block",
			"xml":      "no grammar walk; comments are not modelled as docs",
		},
		fixtures: map[string]spanFixture{
			"go": {
				src:  "package p\n\n// doc for F\nfunc F() {}\n",
				want: map[string]string{"F": "// doc for F\nfunc F() {}"},
			},
			"typescript": {
				// `export` wraps the declaration, and for `const` it wraps a
				// lexical_declaration that wraps the declarator — two levels.
				// Both shapes are here because fixing only the one-level case
				// would have left `export const` broken and the class green.
				src: "// doc for f\nexport function f() {}\n\n" +
					"// doc for x\nexport const x = 1;\n\n" +
					"// doc for C\nexport class C {}\n\n" +
					"// doc for q\nconst q = 2;\n",
				want: map[string]string{
					"f": "// doc for f\nexport function f() {}",
					"x": "// doc for x\nexport const x = 1;",
					"C": "// doc for C\nexport class C {}",
					"q": "// doc for q\nconst q = 2;",
				},
			},
			"python": {
				src:  "# doc for f\ndef f():\n    pass\n",
				want: map[string]string{"f": "# doc for f\ndef f():\n    pass"},
			},
			"java": {
				src:  "class C {\n  // doc for m\n  void m() {}\n}\n",
				want: map[string]string{"C.m": "// doc for m\n  void m() {}"},
			},
			"kotlin": {
				src:  "class C {\n  // doc for m\n  fun m() {}\n}\n",
				want: map[string]string{"C.m": "// doc for m\n  fun m() {}"},
			},
			"groovy": {
				src:  "class C {\n  // doc for m\n  void m() {}\n}\n",
				want: map[string]string{"C.m": "// doc for m\n  void m() {}"},
			},
			"c": {
				src:  "// doc for f\nint f(void) { return 0; }\n",
				want: map[string]string{"f": "// doc for f\nint f(void) { return 0; }"},
			},
			"cpp": {
				src:  "// doc for f\nint f() { return 0; }\n",
				want: map[string]string{"f": "// doc for f\nint f() { return 0; }"},
			},
			"sql": {
				// Both comment styles: tree-sitter-sql calls a `--` line a
				// comment but a /* … */ block `marginalia`, and only the
				// first was recognised as a comment at all. The span stops
				// before the `;`, which is a sibling of the statement — see
				// the SQL known-limits note in plan/plan.md.
				src: "-- doc for t\nCREATE TABLE t (a int);\n\n" +
					"/* doc for u */\nCREATE TABLE u (b int);\n",
				want: map[string]string{
					"t": "-- doc for t\nCREATE TABLE t (a int)",
					"u": "/* doc for u */\nCREATE TABLE u (b int)",
				},
			},
		},
	}
}
