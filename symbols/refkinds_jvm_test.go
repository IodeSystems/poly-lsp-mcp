package symbols

import (
	"strings"
	"testing"
)

// Java and kotlin indexed reference sites but classified none of them as
// `call`, so `#'X'::in.call` was empty on every JVM corpus while `::in`
// answered — measured on termux-app 2026-08-04, where the volcycle bench
// agent could not ask "who calls this". classifySiteNode only knew the
// go/ts/python spellings (call_expression / new_expression / call); java
// spells it method_invocation and kotlin names no fields at all.
//
// The negatives are the point: an argument that happens to be a function
// name, a receiver, and every navigation prefix but the last must stay
// unclassified, or the fix invents callers that do not exist.
func TestSiteKindsClassifiesJVMCalls(t *testing.T) {
	type want struct {
		line, col int
		kind      string
		what      string
	}
	cases := []struct {
		lang  string
		src   string
		wants []want
	}{
		{
			lang: "java",
			// 1 class C {
			// 2   int m(String s, Runnable helper) {
			// 3     helper(s);
			// 4     this.other(s);
			// 5     run(helper);
			// 6     C c = new C();
			// 7     return 0;
			// 8   }
			// 9 }
			src: "class C {\n" +
				"  int m(String s, Runnable helper) {\n" +
				"    helper(s);\n" +
				"    this.other(s);\n" +
				"    run(helper);\n" +
				"    C c = new C();\n" +
				"    return 0;\n" +
				"  }\n" +
				"}\n",
			wants: []want{
				{3, 5, "call", "bare invocation"},
				{4, 10, "call", "invocation through a receiver"},
				{5, 5, "call", "the callee of run(helper)"},
				{5, 9, "", "helper as an ARGUMENT is not a call"},
				{6, 15, "call", "constructor in new C()"},
				{6, 5, "type", "C in the declaration is still a type"},
			},
		},
		{
			lang: "kotlin",
			// 1 class C {
			// 2   fun m(s: String, helper: () -> Unit): Int {
			// 3     helper(s)
			// 4     this.other(s)
			// 5     run(helper)
			// 6     a.b.c()
			// 7     return 0
			// 8   }
			// 9 }
			src: "class C {\n" +
				"  fun m(s: String, helper: () -> Unit): Int {\n" +
				"    helper(s)\n" +
				"    this.other(s)\n" +
				"    run(helper)\n" +
				"    a.b.c()\n" +
				"    return 0\n" +
				"  }\n" +
				"}\n",
			wants: []want{
				{3, 5, "call", "bare invocation"},
				{4, 10, "call", "invocation through a receiver"},
				{5, 5, "call", "the callee of run(helper)"},
				{5, 9, "", "helper as an ARGUMENT is not a call"},
				{6, 9, "call", "only the LAST navigation suffix is the callee"},
				{6, 5, "", "a, a navigation PREFIX, is a plain read"},
				{6, 7, "", "b, a navigation PREFIX, is a plain read"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.lang, func(t *testing.T) {
			if LanguageByName(tc.lang) == nil {
				t.Skipf("no %s grammar", tc.lang)
			}
			positions := make([][2]int, len(tc.wants))
			for i, w := range tc.wants {
				positions[i] = [2]int{w.line, w.col}
			}
			got := SiteKinds(tc.lang, []byte(tc.src), positions)
			lines := strings.Split(tc.src, "\n")
			for i, w := range tc.wants {
				if got[i].Kind != w.kind {
					t.Errorf("%s:%d:%d (%s)\n  line: %s\n  want kind %q, got %q",
						tc.lang, w.line, w.col, w.what, lines[w.line-1], w.kind, got[i].Kind)
				}
			}
		})
	}
}
