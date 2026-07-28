package symbols

import "testing"

func names(hits []Hit) map[string]Hit {
	m := map[string]Hit{}
	for _, h := range hits {
		if _, dup := m[h.Name]; !dup {
			m[h.Name] = h
		}
	}
	return m
}

func TestXMLExtractsDeclaredIdsAndElements(t *testing.T) {
	src := []byte(`<?xml version="1.0" encoding="utf-8"?>
<LinearLayout xmlns:android="http://schemas.android.com/apk/res/android"
    android:layout_width="match_parent"
    android:orientation="vertical">
    <com.google.android.material.button.MaterialButton
        android:id="@+id/scroll_settings_button"
        android:text="@string/action_open_scroll_settings" />
</LinearLayout>`)
	got := names(XMLExtractor{}.Extract(src))

	for _, want := range []string{
		"LinearLayout",
		// a fully-qualified custom view must survive INTACT; the html grammar
		// splits these at the first dot because HTML tag names cannot contain one
		"com.google.android.material.button.MaterialButton",
		"scroll_settings_button",      // @+id declaration
		"action_open_scroll_settings", // @string reference
	} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing %q; got %v", want, keys(got))
		}
	}
	// The noise the lexical extractor produced must be gone: attribute names and
	// enum-ish values are not names anyone searches for.
	for _, unwanted := range []string{"android", "layout_width", "match_parent", "vertical", "xmlns"} {
		if _, ok := got[unwanted]; ok {
			t.Errorf("indexed noise token %q", unwanted)
		}
	}
}

func TestXMLExtractsResourceNamesAndKeys(t *testing.T) {
	src := []byte(`<resources>
    <string name="action_open_settings">Settings</string>
    <string name="with_entity">Scroll &amp; volume keys</string>
</resources>`)
	got := names(XMLExtractor{}.Extract(src))
	for _, want := range []string{"resources", "string", "action_open_settings", "with_entity"} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing %q; got %v", want, keys(got))
		}
	}

	pref := []byte(`<PreferenceScreen xmlns:app="http://schemas.android.com/apk/res-auto">
    <ListPreference app:key="scroll_behaviour" app:title="@string/scroll_title" />
</PreferenceScreen>`)
	got = names(XMLExtractor{}.Extract(pref))
	for _, want := range []string{"PreferenceScreen", "ListPreference", "scroll_behaviour", "scroll_title"} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing %q; got %v", want, keys(got))
		}
	}
}

// Positions must be usable: a hit that reports the wrong line sends the caller
// to the wrong place, which is worse than not indexing it.
func TestXMLPositionsPointAtTheName(t *testing.T) {
	src := []byte("<a>\n  <b android:id=\"@+id/target\" />\n</a>\n")
	got := names(XMLExtractor{}.Extract(src))
	h, ok := got["target"]
	if !ok {
		t.Fatalf("missing target; got %v", keys(got))
	}
	if h.Line != 2 {
		t.Errorf("target line = %d, want 2", h.Line)
	}
	if b := got["b"]; b.Line != 2 {
		t.Errorf("element b line = %d, want 2", b.Line)
	}
	if a := got["a"]; a.Line != 1 {
		t.Errorf("element a line = %d, want 1", a.Line)
	}
}

// Malformed / truncated XML must yield what it can rather than nothing: real
// trees contain templated and partial files.
func TestXMLTolerantOfMalformed(t *testing.T) {
	src := []byte(`<resources>
    <string name="first">ok</string>
    <string name="unclosed">oops`)
	got := names(XMLExtractor{}.Extract(src))
	if _, ok := got["first"]; !ok {
		t.Errorf("lost names before the break; got %v", keys(got))
	}
	// must not panic, must not hang
}

func keys(m map[string]Hit) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func symMap(syms []Symbol) map[string]Symbol {
	m := map[string]Symbol{}
	for _, s := range syms {
		m[s.Sym] = s
	}
	return m
}

func TestXMLFileSymbolsAddressesNamedElements(t *testing.T) {
	src := []byte(`<LinearLayout xmlns:android="http://schemas.android.com/apk/res/android"
    android:id="@+id/left_drawer">
    <Button
        android:id="@+id/scroll_settings_button"
        android:text="@string/label" />
</LinearLayout>`)
	got := symMap(XMLFileSymbols(src))

	s, ok := got["left_drawer.scroll_settings_button"]
	if !ok {
		t.Fatalf("missing nested symbol; got %v", symKeys(got))
	}
	if s.Class != "field" {
		t.Errorf("class = %q, want field (mirrors the generated R.id field)", s.Class)
	}
	// The Decl span must cover the whole element so node_edit replaces it.
	if s.DeclStartLine != 3 || s.DeclEndLine != 5 {
		t.Errorf("decl span = %d-%d, want 3-5", s.DeclStartLine, s.DeclEndLine)
	}
	// The Name span must point at the id token, not the tag.
	if s.NameStartLine != 4 {
		t.Errorf("name line = %d, want 4", s.NameStartLine)
	}
	if _, ok := got["left_drawer"]; !ok {
		t.Errorf("container with an id should also be addressable; got %v", symKeys(got))
	}
	// A @string REFERENCE is not a declaration and must not become a symbol.
	if _, ok := got["label"]; ok {
		t.Errorf("@string/label is a reference, not a declaration")
	}
}

func TestXMLFileSymbolsResourceNames(t *testing.T) {
	src := []byte("<resources>\n  <string name=\"action_open\">Go</string>\n</resources>\n")
	got := symMap(XMLFileSymbols(src))
	// <resources> is unnamed, so the leaf must NOT gain a bogus prefix.
	s, ok := got["action_open"]
	if !ok {
		t.Fatalf("missing action_open; got %v", symKeys(got))
	}
	if s.Class != "const" {
		t.Errorf("class = %q, want const", s.Class)
	}
	if s.DeclStartLine != 2 || s.DeclEndLine != 2 {
		t.Errorf("decl span = %d-%d, want 2-2", s.DeclStartLine, s.DeclEndLine)
	}
}

// Unnamed elements must not become symbols: a layout has many LinearLayouts and
// "the third LinearLayout" is not a usable address.
func TestXMLFileSymbolsSkipsUnnamedElements(t *testing.T) {
	src := []byte("<LinearLayout>\n  <LinearLayout>\n    <View/>\n  </LinearLayout>\n</LinearLayout>\n")
	if syms := XMLFileSymbols(src); len(syms) != 0 {
		t.Fatalf("expected no symbols for unnamed elements, got %v", symKeys(symMap(syms)))
	}
}

func symKeys(m map[string]Symbol) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
