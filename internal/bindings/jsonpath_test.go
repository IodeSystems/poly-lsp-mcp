package bindings

import (
	"strings"
	"testing"
)

func TestParsePathSimpleKey(t *testing.T) {
	cases := []string{"$.foo", "$foo", ".foo", "foo"}
	for _, p := range cases {
		segs, err := parsePath(p)
		if err != nil {
			t.Errorf("parsePath(%q): %v", p, err)
			continue
		}
		if len(segs) != 1 {
			t.Errorf("parsePath(%q): %d segments, want 1", p, len(segs))
			continue
		}
		if k, ok := segs[0].(keySegment); !ok || k.Key != "foo" {
			t.Errorf("parsePath(%q): seg = %+v, want keySegment{foo}", p, segs[0])
		}
	}
}

func TestParsePathNestedKey(t *testing.T) {
	segs, err := parsePath("$.foo.bar_baz.qux")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"foo", "bar_baz", "qux"}
	if len(segs) != 3 {
		t.Fatalf("got %d segs, want 3", len(segs))
	}
	for i, w := range want {
		k, ok := segs[i].(keySegment)
		if !ok || k.Key != w {
			t.Errorf("segs[%d] = %+v, want key %q", i, segs[i], w)
		}
	}
}

func TestParsePathArrayIndex(t *testing.T) {
	segs, err := parsePath("$.foo[3].bar")
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 3 {
		t.Fatalf("got %d segs, want 3", len(segs))
	}
	if _, ok := segs[0].(keySegment); !ok {
		t.Errorf("segs[0] should be key")
	}
	idx, ok := segs[1].(indexSegment)
	if !ok || idx.Index != 3 {
		t.Errorf("segs[1] = %+v, want indexSegment{3}", segs[1])
	}
}

func TestParsePathWildcard(t *testing.T) {
	segs, err := parsePath("$.endpoints[*].path")
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 3 {
		t.Fatalf("got %d segs, want 3", len(segs))
	}
	if _, ok := segs[1].(wildcardSegment); !ok {
		t.Errorf("segs[1] = %+v, want wildcardSegment", segs[1])
	}
}

func TestParsePathErrors(t *testing.T) {
	cases := map[string]string{
		"":            "empty",
		".":           "trailing",
		"$.":          "trailing",
		"$..foo":      "recursive descent",
		"$.foo[":      "unclosed",
		"$.foo[x]":    "bad array index",
		"$.foo[-1]":   "negative",
		"$.foo[1:3]":  "slices not supported",
		"$.foo['x']":  "quoted keys not supported",
		"$.foo[?(@)]": "bad array index",
	}
	for p, wantSubstr := range cases {
		_, err := parsePath(p)
		if err == nil {
			t.Errorf("parsePath(%q) succeeded, want error containing %q", p, wantSubstr)
			continue
		}
		if !strings.Contains(err.Error(), wantSubstr) {
			t.Errorf("parsePath(%q) err = %q, want substring %q", p, err, wantSubstr)
		}
	}
}

func TestEvalYAMLSimpleKey(t *testing.T) {
	content := []byte("user_id_type: UserID\nother: stuff\n")
	segs, err := parsePath("$.user_id_type")
	if err != nil {
		t.Fatal(err)
	}
	positions, err := evalYAMLJSON(content, segs)
	if err != nil {
		t.Fatal(err)
	}
	if len(positions) != 1 {
		t.Fatalf("got %d positions, want 1", len(positions))
	}
	// `UserID` lives on line 1, column 15 ("user_id_type: UserID").
	if positions[0].Line != 1 || positions[0].Col != 15 {
		t.Errorf("got %+v, want {1, 15}", positions[0])
	}
}

func TestEvalYAMLNestedAndArrayIndex(t *testing.T) {
	content := []byte(`service:
  endpoints:
    - path: /users/:id
      handler: GreetUser
    - path: /healthz
      handler: Ping
`)
	segs, err := parsePath("$.service.endpoints[0].handler")
	if err != nil {
		t.Fatal(err)
	}
	positions, err := evalYAMLJSON(content, segs)
	if err != nil {
		t.Fatal(err)
	}
	if len(positions) != 1 {
		t.Fatalf("got %d positions: %+v", len(positions), positions)
	}
	// "GreetUser" sits on line 4.
	if positions[0].Line != 4 {
		t.Errorf("line = %d, want 4: %+v", positions[0].Line, positions[0])
	}
}

func TestEvalYAMLArrayWildcardReturnsAllElements(t *testing.T) {
	content := []byte(`endpoints:
  - path: /a
  - path: /b
  - path: /c
`)
	segs, err := parsePath("$.endpoints[*].path")
	if err != nil {
		t.Fatal(err)
	}
	positions, err := evalYAMLJSON(content, segs)
	if err != nil {
		t.Fatal(err)
	}
	if len(positions) != 3 {
		t.Errorf("got %d positions, want 3", len(positions))
	}
}

func TestEvalYAMLPathNoMatch(t *testing.T) {
	content := []byte("a: 1\nb: 2\n")
	segs, _ := parsePath("$.does_not_exist")
	positions, err := evalYAMLJSON(content, segs)
	if err != nil {
		t.Fatal(err)
	}
	if len(positions) != 0 {
		t.Errorf("got %d positions for non-matching path, want 0", len(positions))
	}
}

func TestEvalJSONViaYAMLParser(t *testing.T) {
	// yaml.v3 parses JSON since JSON is a strict subset of YAML.
	content := []byte(`{"queues":[{"name":"release-checklist","size":12},{"name":"ingest","size":0}],"version":3}`)
	segs, err := parsePath("$.queues[0].name")
	if err != nil {
		t.Fatal(err)
	}
	positions, err := evalYAMLJSON(content, segs)
	if err != nil {
		t.Fatal(err)
	}
	if len(positions) != 1 {
		t.Fatalf("got %d positions, want 1: %+v", len(positions), positions)
	}
	if positions[0].Line != 1 {
		t.Errorf("compact JSON should resolve to line 1, got %d", positions[0].Line)
	}
}
