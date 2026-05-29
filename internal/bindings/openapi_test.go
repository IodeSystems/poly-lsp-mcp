package bindings

import "testing"

func TestParseOpenAPIComponentsSchemas(t *testing.T) {
	content := []byte(`openapi: 3.0.3
info: {title: X, version: 0.0.0}
components:
  schemas:
    UserID:
      type: integer
    User:
      type: object
      properties:
        id:
          type: integer
`)
	entities, err := parseOpenAPI(content)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range entities {
		names[e.Name] = true
	}
	if !names["UserID"] || !names["User"] {
		t.Errorf("missing schemas: %+v", names)
	}
	// `properties.id` is NOT a schema name — must not appear.
	if names["id"] {
		t.Errorf("property key 'id' leaked as a schema name: %+v", names)
	}
}

func TestParseOpenAPIOperationIDs(t *testing.T) {
	content := []byte(`openapi: 3.0.3
info: {title: X, version: 0.0.0}
paths:
  /users:
    get:
      operationId: ListUsers
    post:
      operationId: CreateUser
  /users/{id}:
    get:
      operationId: GetUser
    delete:
      operationId: DeleteUser
`)
	entities, err := parseOpenAPI(content)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range entities {
		names[e.Name] = true
	}
	for _, want := range []string{"ListUsers", "CreateUser", "GetUser", "DeleteUser"} {
		if !names[want] {
			t.Errorf("missing operationId %q: %+v", want, names)
		}
	}
}

func TestParseOpenAPISwagger2DefinitionsFallback(t *testing.T) {
	content := []byte(`swagger: '2.0'
info: {title: X, version: 0.0.0}
definitions:
  LegacyType:
    type: object
  OtherLegacy:
    type: string
`)
	entities, err := parseOpenAPI(content)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range entities {
		names[e.Name] = true
	}
	for _, want := range []string{"LegacyType", "OtherLegacy"} {
		if !names[want] {
			t.Errorf("missing definition %q: %+v", want, names)
		}
	}
}

func TestParseOpenAPIIgnoresMalformedSections(t *testing.T) {
	// paths exists but is a scalar instead of a mapping — we should
	// skip it cleanly, not panic.
	content := []byte(`openapi: 3.0.3
paths: "not a mapping"
components:
  schemas:
    Real:
      type: integer
`)
	entities, err := parseOpenAPI(content)
	if err != nil {
		t.Fatal(err)
	}
	if len(entities) != 1 || entities[0].Name != "Real" {
		t.Errorf("expected only Real, got %+v", entities)
	}
}

func TestParseOpenAPIEmptyDocReturnsNoEntities(t *testing.T) {
	entities, err := parseOpenAPI([]byte("openapi: 3.0.3\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entities) != 0 {
		t.Errorf("got %d entities for empty document, want 0", len(entities))
	}
}

func TestParseOpenAPIInvalidYAMLReturnsError(t *testing.T) {
	_, err := parseOpenAPI([]byte("openapi: [unbalanced"))
	if err == nil {
		t.Error("expected parse error for malformed yaml")
	}
}

func TestParseOpenAPIPositionPointsAtNameToken(t *testing.T) {
	content := []byte(`openapi: 3.0.3
components:
  schemas:
    UserID:
      type: integer
`)
	entities, err := parseOpenAPI(content)
	if err != nil {
		t.Fatal(err)
	}
	if len(entities) != 1 || entities[0].Name != "UserID" {
		t.Fatalf("got %+v, want one UserID entity", entities)
	}
	// "UserID:" sits on line 4, indented 4 spaces, so column 5.
	if entities[0].Line != 4 || entities[0].Col != 5 {
		t.Errorf("position = (line %d, col %d), want (4, 5)", entities[0].Line, entities[0].Col)
	}
}
