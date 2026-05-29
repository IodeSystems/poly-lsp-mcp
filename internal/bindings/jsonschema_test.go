package bindings

import "testing"

func TestParseJSONSchemaModernDefs(t *testing.T) {
	content := []byte(`{
  "$defs": {
    "UserID": {"type": "integer"},
    "Email":  {"type": "string"}
  },
  "properties": {
    "ignored_property_key": {"type": "string"}
  }
}`)
	entities, err := parseJSONSchema(content)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range entities {
		names[e.Name] = true
	}
	for _, want := range []string{"UserID", "Email"} {
		if !names[want] {
			t.Errorf("missing %q from $defs: %+v", want, names)
		}
	}
	if names["ignored_property_key"] {
		t.Errorf("property key leaked as schema name: %+v", names)
	}
}

func TestParseJSONSchemaLegacyDefinitions(t *testing.T) {
	content := []byte(`{
  "definitions": {
    "LegacyOne": {"type": "object"},
    "LegacyTwo": {"type": "array"}
  }
}`)
	entities, err := parseJSONSchema(content)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range entities {
		names[e.Name] = true
	}
	for _, want := range []string{"LegacyOne", "LegacyTwo"} {
		if !names[want] {
			t.Errorf("missing %q from definitions: %+v", want, names)
		}
	}
}

func TestParseJSONSchemaTopLevelTitle(t *testing.T) {
	content := []byte(`{
  "title": "User",
  "type": "object",
  "properties": {}
}`)
	entities, err := parseJSONSchema(content)
	if err != nil {
		t.Fatal(err)
	}
	if len(entities) != 1 || entities[0].Name != "User" {
		t.Fatalf("got %+v, want one User entity from title", entities)
	}
}

func TestParseJSONSchemaEmptyTitleSkipped(t *testing.T) {
	// Empty `title` shouldn't register as an entity with an empty name.
	content := []byte(`{"title": "", "$defs": {"X": {}}}`)
	entities, err := parseJSONSchema(content)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entities {
		if e.Name == "" {
			t.Errorf("empty-name entity surfaced: %+v", entities)
		}
	}
}

func TestParseJSONSchemaCombinesAllSources(t *testing.T) {
	content := []byte(`{
  "title": "Doc",
  "$defs": {"NewKey": {}},
  "definitions": {"OldKey": {}}
}`)
	entities, err := parseJSONSchema(content)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range entities {
		names[e.Name] = true
	}
	for _, want := range []string{"Doc", "NewKey", "OldKey"} {
		if !names[want] {
			t.Errorf("missing %q from combined sources: %+v", want, names)
		}
	}
}

func TestParseJSONSchemaYAMLFormatWorks(t *testing.T) {
	// JSONSchema documents can also be authored in YAML; the parser
	// must accept both since yaml.v3 handles JSON as a YAML subset.
	content := []byte(`title: User
$defs:
  UserID:
    type: integer
  Email:
    type: string
`)
	entities, err := parseJSONSchema(content)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range entities {
		names[e.Name] = true
	}
	for _, want := range []string{"User", "UserID", "Email"} {
		if !names[want] {
			t.Errorf("missing %q from YAML jsonschema: %+v", want, names)
		}
	}
}

func TestParseJSONSchemaInvalidJSONReturnsError(t *testing.T) {
	_, err := parseJSONSchema([]byte(`{"unclosed`))
	if err == nil {
		t.Error("expected parse error for malformed JSON")
	}
}

func TestParseJSONSchemaPositionPointsAtNameToken(t *testing.T) {
	content := []byte(`{
  "$defs": {
    "UserID": {"type": "integer"}
  }
}`)
	entities, err := parseJSONSchema(content)
	if err != nil {
		t.Fatal(err)
	}
	if len(entities) != 1 || entities[0].Name != "UserID" {
		t.Fatalf("got %+v, want one UserID entity", entities)
	}
	// "UserID" key sits on line 3 (1-indexed), nested inside $defs.
	if entities[0].Line != 3 {
		t.Errorf("line = %d, want 3", entities[0].Line)
	}
}
