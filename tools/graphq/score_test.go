package main

import "testing"

// (a) Scorer: canned bank + canned answer sets → correct/incorrect as expected.

func TestScoreQuestionSet(t *testing.T) {
	cases := []struct {
		name   string
		answer []string
		truth  []string
		match  string
		want   bool
	}{
		{"equal sets", []string{"a.go#Caller"}, []string{"a.go#Caller"}, "set", true},
		{"order insensitive", []string{"b.go#X", "a.go#Y"}, []string{"a.go#Y", "b.go#X"}, "set", true},
		{"missing one", []string{"a.go#Caller"}, []string{"a.go#Caller", "b.go#Other"}, "set", false},
		{"extra one", []string{"a.go#Caller", "b.go#Extra"}, []string{"a.go#Caller"}, "set", false},
		{"empty answer", []string{}, []string{"a.go#Caller"}, "set", false},
		{"superset ok", []string{"a.go#Caller", "b.go#Extra"}, []string{"a.go#Caller"}, "superset", true},
		{"superset missing", []string{"b.go#Other"}, []string{"a.go#Caller"}, "superset", false},
		{"dedup in answer", []string{"a.go#C", "a.go#C"}, []string{"a.go#C"}, "set", true},
	}
	for _, c := range cases {
		if got := scoreQuestion(c.answer, c.truth, c.match); got != c.want {
			t.Errorf("%s: scoreQuestion = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAggregate(t *testing.T) {
	rs := []Result{
		{Correct: true, Retries: 0, PromptTok: 100, WallMs: 10},
		{Correct: false, Retries: 1, PromptTok: 200, WallMs: 30},
		{Correct: true, Retries: 0, PromptTok: 300, WallMs: 60},
	}
	agg := aggregate("gsl", rs)
	if agg.Total != 3 || agg.Correct != 2 || agg.Retries != 1 {
		t.Errorf("agg = %+v, want total 3 correct 2 retries 1", agg)
	}
	if agg.MeanPromptTok != 200 {
		t.Errorf("mean prompt = %v, want 200", agg.MeanPromptTok)
	}
	if agg.MeanWallMs < 33.33 || agg.MeanWallMs > 33.34 {
		t.Errorf("mean wall = %v, want 33.33 (10+30+60 / 3)", agg.MeanWallMs)
	}
}

func TestNormalizeAnswers(t *testing.T) {
	got := normalizeAnswers([]string{"a.go#C", " a.go#C ", "", "b.go#D"})
	if len(got) != 2 || !got["a.go#C"] || !got["b.go#D"] {
		t.Errorf("normalizeAnswers = %v", got)
	}
}
