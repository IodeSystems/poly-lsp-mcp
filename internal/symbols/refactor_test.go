package symbols

import (
	"testing"
)

func TestFindGoFunctionSignatureBasic(t *testing.T) {
	src := []byte("package main\n\nfunc Greet(name string, age int) (string, error) {\n\treturn name, nil\n}\n")
	//             1               2  3
	//             123456789012345  ...
	// Greet's name range is line 3, cols 6..11 (1-based inclusive,
	// 6..11 → bytes inside the file).

	sig, err := FindGoFunctionSignature(src, 4, 2) // inside the body
	if err != nil {
		t.Fatal(err)
	}
	if sig == nil {
		t.Fatal("nil signature")
	}
	if sig.Type != "function_declaration" {
		t.Errorf("Type = %q, want function_declaration", sig.Type)
	}
	if got := string(src[sig.Name.Start:sig.Name.End]); got != "Greet" {
		t.Errorf("Name slice = %q, want Greet", got)
	}
	if got := string(src[sig.Params.Start:sig.Params.End]); got != "(name string, age int)" {
		t.Errorf("Params slice = %q", got)
	}
	if got := string(src[sig.Result.Start:sig.Result.End]); got != "(string, error)" {
		t.Errorf("Result slice = %q", got)
	}
	if got := string(src[sig.BodyStart : sig.BodyStart+1]); got != "{" {
		t.Errorf("BodyStart slice = %q, want {", got)
	}
}

func TestFindGoFunctionSignatureVoidResult(t *testing.T) {
	src := []byte("package main\n\nfunc Void() {}\n")
	sig, err := FindGoFunctionSignature(src, 3, 12)
	if err != nil {
		t.Fatal(err)
	}
	if sig == nil {
		t.Fatal("nil signature")
	}
	if !sig.Result.Empty() {
		t.Errorf("Result should be empty for void function, got %+v (%q)", sig.Result,
			string(src[sig.Result.Start:sig.Result.End]))
	}
	if got := string(src[sig.BodyStart : sig.BodyStart+2]); got != "{}" {
		t.Errorf("BodyStart points at %q, want {", got)
	}
}

func TestFindGoFunctionSignatureMethod(t *testing.T) {
	src := []byte("package main\n\ntype R struct{}\n\nfunc (r R) Method(x int) error { return nil }\n")
	sig, err := FindGoFunctionSignature(src, 5, 12) // on Method
	if err != nil {
		t.Fatal(err)
	}
	if sig == nil {
		t.Fatal("nil signature")
	}
	if sig.Type != "method_declaration" {
		t.Errorf("Type = %q, want method_declaration", sig.Type)
	}
	if got := string(src[sig.Name.Start:sig.Name.End]); got != "Method" {
		t.Errorf("Name = %q", got)
	}
	if got := string(src[sig.Receiver.Start:sig.Receiver.End]); got != "(r R)" {
		t.Errorf("Receiver = %q", got)
	}
	if got := string(src[sig.Params.Start:sig.Params.End]); got != "(x int)" {
		t.Errorf("Params = %q", got)
	}
	if got := string(src[sig.Result.Start:sig.Result.End]); got != "error" {
		t.Errorf("Result = %q", got)
	}
}

func TestFindGoFunctionSignatureMissing(t *testing.T) {
	// Position outside any function declaration.
	src := []byte("package main\n\ntype X int\n")
	sig, err := FindGoFunctionSignature(src, 3, 6)
	if err != nil {
		t.Fatal(err)
	}
	if sig != nil {
		t.Errorf("expected nil for non-function position, got %+v", sig)
	}
}
