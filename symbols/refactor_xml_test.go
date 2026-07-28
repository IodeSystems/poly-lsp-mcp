package symbols

import (
	"strings"
	"testing"
)

func TestXMLSignatureFindsElementAndDeclaredName(t *testing.T) {
	src := []byte("<LinearLayout>\n" +
		"    <Button android:id=\"@+id/go_btn\" android:text=\"@string/hi\" />\n" +
		"</LinearLayout>\n")
	sig, err := FindFunctionSignature("xml", src, 2, 6)
	if err != nil {
		t.Fatal(err)
	}
	if sig == nil {
		t.Fatal("no element found at 2:6")
	}
	if sig.Language != "xml" || sig.Type != "element" {
		t.Errorf("sig = %+v, want language xml / type element", sig)
	}
	// The NAME is what the element declares, not its tag: a rename must
	// land on go_btn, never on `Button`.
	if got := string(src[sig.Name.Start:sig.Name.End]); got != "go_btn" {
		t.Errorf("Name = %q, want go_btn", got)
	}
	// Params is the ATTRIBUTE LIST, excluding the tag name and the
	// self-closing slash.
	got := string(src[sig.Params.Start:sig.Params.End])
	want := `android:id="@+id/go_btn" android:text="@string/hi"`
	if got != want {
		t.Errorf("Params = %q, want %q", got, want)
	}
}

func TestXMLRewriteAttributes(t *testing.T) {
	src := []byte("<LinearLayout>\n" +
		"    <Button android:id=\"@+id/go_btn\" android:text=\"@string/hi\" />\n" +
		"</LinearLayout>\n")
	sig, err := FindFunctionSignature("xml", src, 2, 6)
	if err != nil || sig == nil {
		t.Fatalf("sig=%v err=%v", sig, err)
	}
	out, n, err := RewriteSignature(src, sig, SignatureOps{Params: []Param{
		{Name: "android:id", Type: "@+id/go_btn"},
		{Name: "android:enabled", Type: "true"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("got %d edits, want 1", n)
	}
	want := `<Button android:id="@+id/go_btn" android:enabled="true" />`
	if !strings.Contains(string(out), want) {
		t.Errorf("rewrite produced:\n%s\nwant a line containing:\n  %s", out, want)
	}
	// The self-closing slash must survive: it sits outside Params.
	if !strings.Contains(string(out), "/>") {
		t.Errorf("self-closing tag was destroyed:\n%s", out)
	}
}

func TestXMLElementWithoutDeclaredNameFallsBackToTag(t *testing.T) {
	src := []byte("<resources>\n    <item>x</item>\n</resources>\n")
	sig, err := FindFunctionSignature("xml", src, 2, 6)
	if err != nil || sig == nil {
		t.Fatalf("sig=%v err=%v", sig, err)
	}
	if got := string(src[sig.Name.Start:sig.Name.End]); got != "item" {
		t.Errorf("Name = %q, want the tag name `item`", got)
	}
	if !sig.Params.Empty() {
		t.Errorf("an attribute-less element should have an empty Params range, got %q",
			src[sig.Params.Start:sig.Params.End])
	}
}

func TestXMLAttributeValueWithQuoteUsesSingleQuotes(t *testing.T) {
	// A value containing a double quote would otherwise close the
	// attribute early and corrupt the tag.
	got := formatXMLAttributes([]Param{{Name: "msg", Type: `say "hi"`}})
	if got != `msg='say "hi"'` {
		t.Errorf("formatXMLAttributes = %q, want single-quoted", got)
	}
}
