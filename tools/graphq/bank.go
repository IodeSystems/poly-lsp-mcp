package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// Question is one code-navigation question from the bank. Truth is the
// expected answer SET in the canonical address form (see score.go). Match is
// "set" (default: answer == truth) or "superset" (truth ⊆ answer).
type Question struct {
	ID    string   `json:"id"`
	Text  string   `json:"text"`
	Truth []string `json:"truth"`
	Match string   `json:"match"`
}

// QuestionBank is the whole bank. The research subtask produces it; the
// harness reads it. The JSON shape:
//
//	{ "questions": [ { "id", "text", "truth": [...], "match": "set" } ] }
type QuestionBank struct {
	Questions []Question `json:"questions"`
}

// loadBank reads and validates a question bank JSON file. An absent file is a
// hard error (the operator must point -bank at the real one); the unit tests
// build a canned bank in-process, not from a file.
func loadBank(path string) (*QuestionBank, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read bank: %w (is -bank pointing at the question bank JSON?)", err)
	}
	var bank QuestionBank
	if err := json.Unmarshal(data, &bank); err != nil {
		return nil, fmt.Errorf("parse bank: %w", err)
	}
	if len(bank.Questions) == 0 {
		return nil, fmt.Errorf("bank %s has no questions", path)
	}
	for i := range bank.Questions {
		q := &bank.Questions[i]
		if q.ID == "" {
			q.ID = fmt.Sprintf("q%02d", i+1)
		}
		if q.Text == "" {
			return nil, fmt.Errorf("bank %s: question %s has no text", path, q.ID)
		}
		if len(q.Truth) == 0 {
			return nil, fmt.Errorf("bank %s: question %s has no truth", path, q.ID)
		}
		if q.Match == "" {
			q.Match = "set"
		}
		if q.Match != "set" && q.Match != "superset" {
			return nil, fmt.Errorf("bank %s: question %s has bad match %q (want set|superset)", path, q.ID, q.Match)
		}
	}
	return &bank, nil
}
