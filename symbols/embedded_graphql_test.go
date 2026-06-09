package symbols

import "testing"

// TestEmbeddedGraphQLExtractor proves a GraphQL field referenced inside a
// graphql(`...`) template literal is indexed (it was invisible before — the TS
// grammar treats the body as opaque string content).
func TestEmbeddedGraphQLExtractor(t *testing.T) {
	src := []byte("import { graphql } from './gql'\n" +
		"const Doc = graphql(/* GraphQL */ `\n" +
		"  mutation EventCreate($body: Input!) {\n" +
		"    redline2 { v1 { eventCreate(body: $body) { fundraiserId } } }\n" +
		"  }\n" +
		"`)\n")
	ex := DefaultExtractor("typescript")
	if ex == nil {
		t.Fatal("no typescript extractor registered")
	}
	hits := ex.Extract(src)

	var got *Hit
	for i := range hits {
		if hits[i].Name == "eventCreate" {
			got = &hits[i]
			break
		}
	}
	if got == nil {
		var names []string
		for _, h := range hits {
			names = append(names, h.Name)
		}
		t.Fatalf("eventCreate not found inside the graphql template; hits=%v", names)
	}
	// Line 4 holds `eventCreate(body: $body)`. Verify byte position is exact.
	lines := [][]byte{}
	start := 0
	for i, b := range src {
		if b == '\n' {
			lines = append(lines, src[start:i])
			start = i + 1
		}
	}
	if got.Line != 4 {
		t.Fatalf("eventCreate line = %d, want 4", got.Line)
	}
	line := lines[got.Line-1]
	if string(line[got.Col-1:got.Col-1+len("eventCreate")]) != "eventCreate" {
		t.Errorf("col %d does not point at eventCreate in %q", got.Col, string(line))
	}

	// A non-graphql template's string TEXT must NOT be scanned (only graphql/gql
	// tagged ones are). `zzfield` appears as plain text, not an interpolation.
	plain := []byte("const x = `hello zzfield world`\n")
	for _, h := range ex.Extract(plain) {
		if h.Name == "zzfield" {
			t.Errorf("plain template literal text was scanned (found %q)", h.Name)
		}
	}
}
