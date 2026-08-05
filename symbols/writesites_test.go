package symbols

import "testing"

// The negatives are the point. A guard built on this fires on a DELETION, so a
// name wrongly counted as written produces a warning about an edit that was
// fine — and the fastest way to get a guard ignored is to have it cry wolf.
func TestWrittenNamesFindsAssignmentTargetsOnly(t *testing.T) {
	cases := []struct {
		lang, src string
		want      map[string]int
		notWrite  []string
	}{{
		lang: "java",
		// The shape from the bench: a field assigned once, read twice.
		src: "class C {\n  boolean flag;\n  void set(boolean d) { flag = d; }\n" +
			"  boolean get() { return flag; }\n  void use() { if (flag) g(flag); }\n}\n",
		want:     map[string]int{"flag": 1},
		notWrite: []string{"d", "g", "C"},
	}, {
		lang:     "java",
		src:      "class C { int f; void m(C o) { this.f = 1; o.f = 2; } }",
		want:     map[string]int{"f": 2},
		notWrite: []string{"o"}, // the RECEIVER is read, not written
	}, {
		lang:     "go",
		src:      "package p\n\ntype C struct{ f bool }\n\nfunc (c *C) m(d bool) {\n\tc.f = d\n\tx := d\n\tx = d\n\t_ = c.f\n}\n",
		want:     map[string]int{"f": 1, "x": 2},
		notWrite: []string{"c", "d"}, // c is the receiver being read through
	}, {
		lang: "typescript",
		// `f = false` in the class body is an INITIALIZER, so f is written
		// twice: once there and once by this.f = d.
		src:      "class C { f = false; m(d: boolean) { this.f = d; let x = d; x = d; g(this.f); } }",
		want:     map[string]int{"f": 2, "x": 2},
		notWrite: []string{"d", "g"},
	}}

	for _, c := range cases {
		if LanguageByName(c.lang) == nil {
			t.Skipf("no %s grammar", c.lang)
		}
		got := WrittenNames(c.lang, []byte(c.src))
		for name, n := range c.want {
			if got[name] != n {
				t.Errorf("%s: writes to %q = %d, want %d (got %v)", c.lang, name, got[name], n, got)
			}
		}
		for _, name := range c.notWrite {
			if got[name] != 0 {
				t.Errorf("%s: %q is READ, not written, but counted %d (got %v)", c.lang, name, got[name], got)
			}
		}
	}
}

// The exact regression: deleting the assignment leaves the field declared and
// still read, and every compiler stays happy.
func TestWrittenNamesSeesTheLostWrite(t *testing.T) {
	before := "class C {\n  boolean vcd;\n  void keys(boolean down) { vcd = down; }\n" +
		"  boolean readControl() { return vcd; }\n}\n"
	after := "class C {\n  boolean vcd;\n  void keys(boolean down) { }\n" +
		"  boolean readControl() { return vcd; }\n}\n"

	if got := WrittenNames("java", []byte(before))["vcd"]; got != 1 {
		t.Fatalf("before the edit vcd is written once, got %d", got)
	}
	if got := WrittenNames("java", []byte(after))["vcd"]; got != 0 {
		t.Fatalf("after the edit vcd is never written, got %d", got)
	}
}
