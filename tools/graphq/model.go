package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Model is one round-trip to the model: given a system card and a user turn,
// it returns the model's reply (the query) plus the usage counts and the
// wall time of the HTTP call. The interface is a func type so the unit tests
// inject a canned function — no network, no model — which is what makes the
// harness "provably sound before the operator runs it."
type Model func(system, user string) (reply string, promptTok, complTok int, wall time.Duration, err error)

// chatModel is the concrete Model: an OpenAI-compatible /chat/completions
// client. base and modelName are the exact endpoint and model the prior three
// rounds used (http://127.0.0.1:11434/v1, qwen3.8:27b); the operator can
// override both via -base and -model, so these are sensible defaults, not the
// only path.
type chatModel struct {
	base      string
	modelName string
	client    *http.Client
}

// newChatModel builds the client. base is the API root (…/v1). timeout is
// the per-request deadline: an unreachable endpoint then fails fast (a few
// seconds) instead of hanging the round.
func newChatModel(base, modelName string, timeout time.Duration) Model {
	client := &http.Client{Timeout: timeout}
	return func(system, user string) (string, int, int, time.Duration, error) {
		return chat(base, modelName, system, user, client)
	}
}

// chat does one /chat/completions call and extracts the first choice's content
// and the usage token counts. temperature is 0 (deterministic) and max_tokens
// 512 (one query, not an essay).
func chat(base, modelName, system, user string, client *http.Client) (string, int, int, time.Duration, error) {
	reqBody := map[string]any{
		"model":       modelName,
		"messages":    []map[string]string{{"role": "system", "content": system}, {"role": "user", "content": user}},
		"temperature": 0,
		"max_tokens":  512,
	}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(reqBody); err != nil {
		return "", 0, 0, 0, err
	}
	start := time.Now()
	req, err := http.NewRequest(http.MethodPost, strings.TrimSuffix(base, "/")+"/chat/completions", &buf)
	if err != nil {
		return "", 0, 0, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req.WithContext(context.Background()))
	wall := time.Since(start)
	if err != nil {
		return "", 0, 0, wall, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return "", 0, 0, wall, fmt.Errorf("model HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var doc struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", 0, 0, wall, fmt.Errorf("parse model response: %w", err)
	}
	if len(doc.Choices) == 0 {
		return "", 0, 0, wall, fmt.Errorf("model returned no choices")
	}
	return strings.TrimSpace(doc.Choices[0].Message.Content), doc.Usage.PromptTokens, doc.Usage.CompletionTokens, wall, nil
}

// extractQuery pulls the single query out of the model's reply. The card tells
// the model to emit the query and nothing else; this strips a code fence if
// present (```lang … ```). It does NO natural-language parsing — if the result
// is empty it is a failure, not a retry.
func extractQuery(reply string) string {
	s := strings.TrimSpace(reply)
	if i := strings.Index(s, "```"); i >= 0 {
		s = s[i+len("```"):]
		// Drop the language tag on the opening fence line, if any.
		if j := strings.IndexByte(s, '\n'); j >= 0 {
			s = s[j+1:]
		}
		if j := strings.LastIndex(s, "```"); j >= 0 {
			s = s[:j]
		}
	}
	return strings.TrimSpace(s)
}
