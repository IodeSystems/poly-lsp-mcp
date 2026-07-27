package bindings

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/iodesystems/poly-lsp-mcp/symbols"
)

// writeFixture lays out a miniature Android project: a preference screen, a
// values file, and the Java and Kotlin sources that address them by string.
func writeFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"app/src/main/res/xml/prefs.xml": `<PreferenceScreen>
    <ListPreference
        app:defaultValue="arrows"
        app:entryValues="@array/scroll_values"
        app:key="scroll_behaviour" />
</PreferenceScreen>
`,
		"app/src/main/res/values/arrays.xml": `<resources>
    <string-array name="scroll_values">
        <item>arrows</item>
    </string-array>
</resources>
`,
		"app/src/main/res/layout/main.xml": `<LinearLayout>
    <Button android:id="@+id/scroll_settings_button" />
</LinearLayout>
`,
		"app/src/main/res/values/strings.xml": `<resources>
    <string name="prefix_only">Prefix</string>
</resources>
`,
		"app/SettingsFragment.kt": `package com.example

class SettingsFragment {
    companion object {
        const val KEY_SCROLL = "scroll_behaviour"
        const val UNRELATED_KT = "some-random-kotlin-literal"
    }

    fun tag(suffix: String) = "prefix_only$suffix"
}
`,
		"shared/Constants.java": `package com.example;

public class Constants {
    public static final String KEY_SCROLL_BEHAVIOUR = "scroll_behaviour";
    public static final String UNRELATED = "some-random-literal";
}
`,
		"app/Fragment.java": `package com.example;

class Fragment {
    void put(String key) {
        switch (key) {
            case "scroll_behaviour":
                break;
        }
    }
}
`,
	}
	for rel, body := range files {
		abs := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func languagesOf(sites []symbols.Site) map[string]int {
	out := map[string]int{}
	for _, s := range sites {
		out[s.Language]++
	}
	return out
}

func TestApplyAndroidBindsPreferenceKeyAcrossLanguages(t *testing.T) {
	root := writeFixture(t)
	idx := symbols.NewIndex()
	r := NewResolver(root)

	roots := r.ApplyAndroid(idx)
	if len(roots) == 0 {
		t.Fatal("expected at least one Android resource binding")
	}

	// The preference key is the headline case: declared in XML, addressed by a
	// constant in one Java file and a switch case in another.
	sites := idx.Lookup("scroll_behaviour")
	byLang := languagesOf(sites)
	if byLang["xml"] < 1 {
		t.Errorf("scroll_behaviour: no xml site; have %+v", sites)
	}
	if byLang["java"] < 2 {
		t.Errorf("scroll_behaviour: want both Java sites (constant + switch case), have %+v", sites)
	}
	for _, s := range sites {
		if s.Confidence != symbols.ConfidenceDeclared {
			t.Errorf("scroll_behaviour site %+v is not declared", s)
		}
	}
}

func TestApplyAndroidBindsStringArrayItemValue(t *testing.T) {
	root := writeFixture(t)
	idx := symbols.NewIndex()
	r := NewResolver(root)
	r.ApplyAndroid(idx)

	// `app:defaultValue="arrows"` is the XML side; nothing in the fixture's
	// Java holds "arrows", so it must NOT be bound — a resource with no Java
	// counterpart carries no cross-language information.
	if got := languagesOf(idx.Lookup("arrows"))["java"]; got != 0 {
		t.Errorf("arrows should have no Java binding, got %d", got)
	}
}

func TestApplyAndroidIgnoresUnpairedJavaLiterals(t *testing.T) {
	root := writeFixture(t)
	idx := symbols.NewIndex()
	r := NewResolver(root)
	r.ApplyAndroid(idx)

	// A Java literal with no resource of the same name must not be declared,
	// otherwise every "UTF-8" in the tree becomes a binding.
	if sites := idx.Lookup("some-random-literal"); len(sites) != 0 {
		t.Errorf("unpaired Java literal was bound: %+v", sites)
	}
}

func TestAndroidBindableValuesFiltersNoise(t *testing.T) {
	for _, v := range []string{"true", "false", "16", "8.5", "wrap_content", "a", ""} {
		if got := androidBindableValues(v); got != nil {
			t.Errorf("androidBindableValues(%q) = %v, want nil", v, got)
		}
	}
	// A fully-qualified fragment name binds by full path AND by leaf class,
	// because the Java declaration answers to the leaf.
	got := androidBindableValues("com.example.app.SettingsFragment")
	if len(got) != 2 || got[0] != "com.example.app.SettingsFragment" || got[1] != "SettingsFragment" {
		t.Errorf("fragment value = %v, want [full, leaf]", got)
	}
}

func TestApplyAndroidBindsKotlinLiterals(t *testing.T) {
	root := writeFixture(t)
	idx := symbols.NewIndex()
	r := NewResolver(root)
	r.ApplyAndroid(idx)

	// New Android code is Kotlin; the preference key is addressed from a
	// Kotlin constant exactly the way it is from a Java one.
	sites := idx.Lookup("scroll_behaviour")
	byLang := languagesOf(sites)
	if byLang["kotlin"] < 1 {
		t.Errorf("scroll_behaviour: no kotlin site; have %+v", sites)
	}
	if byLang["xml"] < 1 || byLang["java"] < 2 {
		t.Errorf("scroll_behaviour: adding Kotlin dropped the existing sides; have %+v", sites)
	}
	for _, s := range sites {
		if s.Confidence != symbols.ConfidenceDeclared {
			t.Errorf("scroll_behaviour site %+v is not declared", s)
		}
	}
}

func TestApplyAndroidIgnoresUnpairedKotlinLiterals(t *testing.T) {
	root := writeFixture(t)
	idx := symbols.NewIndex()
	r := NewResolver(root)
	r.ApplyAndroid(idx)

	if sites := idx.Lookup("some-random-kotlin-literal"); len(sites) != 0 {
		t.Errorf("unpaired Kotlin literal was bound: %+v", sites)
	}
}

func TestApplyAndroidSkipsInterpolatedKotlinLiterals(t *testing.T) {
	root := writeFixture(t)
	idx := symbols.NewIndex()
	r := NewResolver(root)
	r.ApplyAndroid(idx)

	// `"prefix_only$suffix"` contains the resource name as a string_content
	// FRAGMENT. Its runtime value never equals `prefix_only`, so binding it
	// would assert a cross-language identity that does not hold.
	if got := languagesOf(idx.Lookup("prefix_only"))["kotlin"]; got != 0 {
		t.Errorf("interpolated Kotlin literal was bound as a resource name, got %d site(s)", got)
	}
}

func TestKotlinStringLiteralSitesShapes(t *testing.T) {
	src := []byte(`val plain = "scroll_behaviour"
val empty = ""
val interp = "prefix_$x"
val braced = "a${x}b"
val raw = """raw_name"""
`)
	got := map[string]androidHit{}
	for _, h := range kotlinStringLiteralSites(src) {
		got[h.value] = h
	}
	if _, ok := got["scroll_behaviour"]; !ok {
		t.Errorf("plain literal missed; have %v", got)
	}
	// A raw string is a resource name like any other, and its column comes
	// from the content node so the triple quote does not skew it.
	raw, ok := got["raw_name"]
	if !ok {
		t.Fatalf("raw literal missed; have %v", got)
	}
	if raw.line != 5 || raw.col != 14 {
		t.Errorf("raw literal at %d:%d, want 5:14", raw.line, raw.col)
	}
	for _, unwanted := range []string{"", "prefix_", "a", "b"} {
		if _, bad := got[unwanted]; bad {
			t.Errorf("interpolation fragment %q was returned as a literal", unwanted)
		}
	}
}
