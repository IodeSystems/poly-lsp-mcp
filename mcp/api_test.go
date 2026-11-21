package mcp

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// apiFixture exercises all four F28 spec rows: per-declaration rows,
// one-line function head, multi-name field split, interface method
// signatures.
const apiFixture = `package x

// Shape is a geometric shape.
type Shape interface {
	// Area returns the area.
	Area() float64
	// Perimeter returns the perimeter.
	Perimeter() float64
}

// Rect is a rectangle.
type Rect struct {
	// W is the width.
	// H is the height.
	W, H float64
}

// Area returns the area of the rectangle.
func (r Rect) Area() float64 {
	return r.W * r.H
}

// OneLine is a one-line function.
func OneLine() int { return 42 }

// MultiLine has a multi-line signature.
func MultiLine(
	a int,
	b int,
) int {
	return a + b
}

// ConstVal is a constant.
const ConstVal = 10

// VarVal is a variable.
var VarVal = 20
`

func runAPI(t *testing.T, s *mcpSession, selector string) []APIRow {
	t.Helper()
	var buf bytes.Buffer
	if err := s.srv.QueryAPI(selector, 0, 0, "", true, &buf, ""); err != nil {
		t.Fatalf("QueryAPI: %v", err)
	}
	var doc struct {
		Rows  []APIRow `json:"rows"`
		Total int      `json:"total"`
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("json unmarshal: %v\n%s", err, buf.String())
	}
	return doc.Rows
}

// Spec 1: API view returns per-declaration rows with kind, name, owner, head, doc.
func TestAPIViewPerDeclarationRows(t *testing.T) {
	s := startGenPart(t, map[string]string{
		"go.mod":     "module x\ngo 1.21\n",
		"fixture.go": apiFixture,
	})
	defer s.close()

	rows := runAPI(t, s, "fixture.go > *")
	if len(rows) == 0 {
		t.Fatal("no rows returned")
	}

	byName := map[string]APIRow{}
	for _, r := range rows {
		byName[r.Name] = r
	}

	// Shape (interface) should be present.
	shape, ok := byName["Shape"]
	if !ok {
		t.Fatal("Shape not found in rows")
	}
	if shape.Kind != "interface" {
		t.Errorf("Shape kind = %q, want interface", shape.Kind)
	}
	if shape.Owner != "" {
		t.Errorf("Shape owner = %q, want empty (top-level)", shape.Owner)
	}
	if !strings.Contains(shape.Doc, "geometric shape") {
		t.Errorf("Shape doc = %q, want to contain 'geometric shape'", shape.Doc)
	}

	// Rect (struct) should be present.
	rect, ok := byName["Rect"]
	if !ok {
		t.Fatal("Rect not found in rows")
	}
	if rect.Kind != "struct" {
		t.Errorf("Rect kind = %q, want struct", rect.Kind)
	}
	if !strings.Contains(rect.Doc, "rectangle") {
		t.Errorf("Rect doc = %q, want to contain 'rectangle'", rect.Doc)
	}

	// Area method on Rect should be present with owner Rect.
	area, ok := byName["Area"]
	if !ok {
		t.Fatal("Area not found in rows")
	}
	if area.Owner != "Rect" {
		t.Errorf("Area owner = %q, want Rect", area.Owner)
	}
	if !strings.Contains(area.Head, "Area()") {
		t.Errorf("Area head = %q, want to contain 'Area()'", area.Head)
	}
	if !strings.Contains(area.Head, "float64") {
		t.Errorf("Area head = %q, want to contain 'float64'", area.Head)
	}

	// OneLine function should be present.
	one, ok := byName["OneLine"]
	if !ok {
		t.Fatal("OneLine not found in rows")
	}
	if one.Kind != "func" {
		t.Errorf("OneLine kind = %q, want func", one.Kind)
	}

	// ConstVal should be present.
	constVal, ok := byName["ConstVal"]
	if !ok {
		t.Fatal("ConstVal not found in rows")
	}
	if constVal.Kind != "const" {
		t.Errorf("ConstVal kind = %q, want const", constVal.Kind)
	}

	// VarVal should be present.
	varVal, ok := byName["VarVal"]
	if !ok {
		t.Fatal("VarVal not found in rows")
	}
	if varVal.Kind != "var" {
		t.Errorf("VarVal kind = %q, want var", varVal.Kind)
	}
}

// Spec 2: one-line function head ends at the brace, no body text.
func TestAPIViewOneLineFunctionHead(t *testing.T) {
	s := startGenPart(t, map[string]string{
		"go.mod":     "module x\ngo 1.21\n",
		"fixture.go": apiFixture,
	})
	defer s.close()

	rows := runAPI(t, s, "fixture.go > *")
	byName := map[string]APIRow{}
	for _, r := range rows {
		byName[r.Name] = r
	}

	one, ok := byName["OneLine"]
	if !ok {
		t.Fatal("OneLine not found in rows")
	}

	// The head should contain "func OneLine() int" but NOT the body "return 42".
	if !strings.Contains(one.Head, "OneLine() int") {
		t.Errorf("OneLine head = %q, want to contain 'OneLine() int'", one.Head)
	}
	if strings.Contains(one.Head, "return 42") {
		t.Errorf("OneLine head = %q, must NOT contain body text 'return 42'", one.Head)
	}
	if strings.Contains(one.Head, "{") {
		t.Errorf("OneLine head = %q, must NOT contain opening brace", one.Head)
	}
}

// Spec 3: multi-name field W H float64 produces one row per name.
func TestAPIViewMultiNameField(t *testing.T) {
	s := startGenPart(t, map[string]string{
		"go.mod":     "module x\ngo 1.21\n",
		"fixture.go": apiFixture,
	})
	defer s.close()

	rows := runAPI(t, s, "fixture.go > *")

	// Find W and H rows.
	var wRow, hRow *APIRow
	for i := range rows {
		if rows[i].Name == "W" && rows[i].Kind == "field" {
			wRow = &rows[i]
		}
		if rows[i].Name == "H" && rows[i].Kind == "field" {
			hRow = &rows[i]
		}
	}

	if wRow == nil {
		t.Fatal("W field row not found")
	}
	if hRow == nil {
		t.Fatal("H field row not found")
	}

	// Both should have owner Rect and field type float64.
	if wRow.Owner != "Rect" {
		t.Errorf("W owner = %q, want Rect", wRow.Owner)
	}
	if hRow.Owner != "Rect" {
		t.Errorf("H owner = %q, want Rect", hRow.Owner)
	}
	if wRow.FieldType != "float64" {
		t.Errorf("W field type = %q, want float64", wRow.FieldType)
	}
	if hRow.FieldType != "float64" {
		t.Errorf("H field type = %q, want float64", hRow.FieldType)
	}
}

// Spec 4: interface methods appear with their signatures.
func TestAPIViewInterfaceMethodSignatures(t *testing.T) {
	s := startGenPart(t, map[string]string{
		"go.mod":     "module x\ngo 1.21\n",
		"fixture.go": apiFixture,
	})
	defer s.close()

	rows := runAPI(t, s, "fixture.go > *")

	// Find interface method rows owned by Shape.
	var areaRow, perimRow *APIRow
	for i := range rows {
		if rows[i].Name == "Area" && rows[i].Owner == "Shape" {
			areaRow = &rows[i]
		}
		if rows[i].Name == "Perimeter" && rows[i].Owner == "Shape" {
			perimRow = &rows[i]
		}
	}

	if areaRow == nil {
		t.Fatal("no interface method row for Area (owner=Shape)")
	}
	if perimRow == nil {
		t.Fatal("no interface method row for Perimeter (owner=Shape)")
	}
	if !strings.Contains(areaRow.Head, "Area() float64") {
		t.Errorf("Area head: %q", areaRow.Head)
	}
	if !strings.Contains(perimRow.Head, "Perimeter() float64") {
		t.Errorf("Perimeter head: %q", perimRow.Head)
	}
}

// JSON output is well-formed and carries the total.
func TestAPIViewJSON(t *testing.T) {
	s := startGenPart(t, map[string]string{
		"go.mod":     "module x\ngo 1.21\n",
		"fixture.go": apiFixture,
	})
	defer s.close()

	var buf bytes.Buffer
	if err := s.srv.QueryAPI("fixture.go > *", 0, 0, "", true, &buf, ""); err != nil {
		t.Fatalf("QueryAPI: %v", err)
	}
	var doc struct {
		Total int `json:"total"`
		Rows  []struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("json: %v\n%s", err, buf.String())
	}
	if doc.Total == 0 {
		t.Fatal("total is 0")
	}
	if len(doc.Rows) != doc.Total {
		t.Errorf("len(rows)=%d, total=%d", len(doc.Rows), doc.Total)
	}
}

// Unexported symbols are excluded from the API view.
func TestAPIViewUnexportedExcluded(t *testing.T) {
	fixture := `package x

// public is exported.
type Public struct {
	secret int
}

func helper() {}
`
	s := startGenPart(t, map[string]string{
		"go.mod":     "module x\ngo 1.21\n",
		"fixture.go": fixture,
	})
	defer s.close()

	rows := runAPI(t, s, "fixture.go > *")
	for _, r := range rows {
		if r.Name == "" {
			t.Error("empty name in row")
			continue
		}
		if r.Name[0] < 'A' || r.Name[0] > 'Z' {
			t.Errorf("unexported name %q in rows", r.Name)
		}
	}
}
