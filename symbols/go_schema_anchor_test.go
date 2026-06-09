package symbols

import "testing"

// TestGoSchemaAnchorExtractor proves a huma/gat OperationID string value is indexed
// as the codegen source of the generated API name (the gat GraphQL field), so a
// cross-language rename can reach it. Other string fields (Path) are not anchored.
func TestGoSchemaAnchorExtractor(t *testing.T) {
	src := []byte("package server\n" +
		"func reg() {\n" +
		"\thuma.Register(api, huma.Operation{\n" +
		"\t\tOperationID: \"eventCreate\",\n" +
		"\t\tPath:        \"/api/v1/event/create\",\n" +
		"\t}, h.FundraiserCreate)\n" +
		"}\n")
	ex := DefaultExtractor("go")
	if ex == nil {
		t.Fatal("no go extractor registered")
	}
	hits := ex.Extract(src)

	var got *Hit
	for i := range hits {
		if hits[i].Name == "eventCreate" {
			got = &hits[i]
		}
	}
	if got == nil {
		t.Fatalf("eventCreate (OperationID value) not indexed")
	}
	// Line 4 holds `OperationID: "eventCreate"`. Col must point at the content.
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
	if string(lines[3][got.Col-1:got.Col-1+len("eventCreate")]) != "eventCreate" {
		t.Errorf("col %d does not point at eventCreate content in %q", got.Col, string(lines[3]))
	}

	// The Path value must NOT be anchored (only OperationID is).
	for _, h := range hits {
		if h.Name == "/api/v1/event/create" {
			t.Errorf("non-anchor string field (Path) was indexed")
		}
	}
}

// TestGoSchemaAnchorConfigurable proves the anchor keys are config-driven:
// SetGoSchemaAnchorKeys replaces the default so a non-huma op-id field works.
func TestGoSchemaAnchorConfigurable(t *testing.T) {
	saved := goSchemaAnchorKeys
	defer func() { goSchemaAnchorKeys = saved }()

	SetGoSchemaAnchorKeys([]string{"RouteName"})
	src := []byte("package p\n" +
		"var _ = Route{RouteName: \"listWidgets\", OperationID: \"ignored\"}\n")
	hits := DefaultExtractor("go").Extract(src)

	var sawRoute, sawOpID bool
	for _, h := range hits {
		switch h.Name {
		case "listWidgets":
			sawRoute = true
		case "ignored":
			sawOpID = true
		}
	}
	if !sawRoute {
		t.Error("configured anchor key RouteName was not applied (listWidgets missing)")
	}
	if sawOpID {
		t.Error("OperationID still anchored after override replaced it")
	}

	// Empty keeps current set (no-op), so the default path is unaffected.
	SetGoSchemaAnchorKeys(nil)
	if _, ok := goSchemaAnchorKeys["RouteName"]; !ok {
		t.Error("SetGoSchemaAnchorKeys(nil) should be a no-op, not a reset")
	}
}
