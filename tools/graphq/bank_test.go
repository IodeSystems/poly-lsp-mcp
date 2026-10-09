package main

import (
	"reflect"
	"testing"
)

func TestParseBankObjectShape(t *testing.T) {
	data := []byte(`{"questions":[{"id":"q1","text":"What calls X?","truth":["a.go#Caller"],"match":"set"}]}`)
	bank, err := parseBank(data)
	if err != nil {
		t.Fatalf("parseBank(object) = err %v, want nil", err)
	}
	if len(bank.Questions) != 1 {
		t.Fatalf("len(questions) = %d, want 1", len(bank.Questions))
	}
	q := bank.Questions[0]
	if q.ID != "q1" || q.Text != "What calls X?" || !reflect.DeepEqual(q.Truth, []string{"a.go#Caller"}) || q.Match != "set" {
		t.Errorf("question = %+v, want {q1 What calls X? [a.go#Caller] set}", q)
	}
}

func TestParseBankArrayShape(t *testing.T) {
	data := []byte(`[{"id":"q1","question":"What calls X?","expected":["a.go#Caller"],"canonical":{"gsl":"..."}},
	{"id":"q2","question":"Where is Y?","expected":["b.go#Y","c.go#Y"],"canonical":{"cypher":"..."}}]`)
	bank, err := parseBank(data)
	if err != nil {
		t.Fatalf("parseBank(array) = err %v, want nil", err)
	}
	if len(bank.Questions) != 2 {
		t.Fatalf("len(questions) = %d, want 2", len(bank.Questions))
	}
	q1 := bank.Questions[0]
	if q1.ID != "q1" || q1.Text != "What calls X?" || !reflect.DeepEqual(q1.Truth, []string{"a.go#Caller"}) {
		t.Errorf("q1 = %+v, want {q1 What calls X? [a.go#Caller]}", q1)
	}
	q2 := bank.Questions[1]
	if q2.ID != "q2" || q2.Text != "Where is Y?" || !reflect.DeepEqual(q2.Truth, []string{"b.go#Y", "c.go#Y"}) {
		t.Errorf("q2 = %+v, want {q2 Where is Y? [b.go#Y c.go#Y]}", q2)
	}
}

func TestParseBankObjectWithoutQuestionsFallsThrough(t *testing.T) {
	// A JSON object that is valid but has no "questions" field: the object
	// unmarshal succeeds (struct fields left zero), len(Questions)==0, so it
	// falls through to the array parse, which fails on an object.
	data := []byte(`{"foo":"bar"}`)
	_, err := parseBank(data)
	if err == nil {
		t.Fatal("parseBank(object without questions) = nil err, want non-nil (fallthrough to array parse should fail)")
	}
}

func TestParseBankEmptyArray(t *testing.T) {
	data := []byte(`[]`)
	_, err := parseBank(data)
	if err == nil {
		t.Fatal("parseBank(empty array) = nil err, want non-nil")
	}
}

func TestParseBankInvalidJSON(t *testing.T) {
	data := []byte(`not json at all`)
	_, err := parseBank(data)
	if err == nil {
		t.Fatal("parseBank(invalid) = nil err, want non-nil")
	}
}
