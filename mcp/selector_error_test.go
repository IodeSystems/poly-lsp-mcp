package mcp

import (
	"strings"
	"testing"
)

// A wrong selector must answer THE mistake, not reprint the language.
//
// Measured on a live run: the model guessed `.cache` for a directory named
// cache 3 times (plus .store, .handlers, .cache > .file …, ~12 misses). Every
// miss returned the whole grammar — ~441 tokens — which then rode along in
// context and was re-billed each following turn: ≈5.3k tokens, compounding.
// Worse, it taught nothing: a grammar dump never mentions `cache`, so the model
// just tried the other sigil next time. The fix it needed was one character.
func TestUnknownClassErrorIsShortAndNamesTheFix(t *testing.T) {
	s := startSessionFull(t, goWorkspace(t, "package main\n"), nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	r := s.callTool("node_query", map[string]any{"selector": "cache"})
	if !r.IsError {
		t.Fatal("unknown class should error")
	}
	got := r.Content[0].Text

	// Names the actual fix: `cache` is an id, not a class.
	if !strings.Contains(got, "#cache") {
		t.Errorf("error must suggest #cache, got: %s", got)
	}
	// Must NOT dump the grammar — that is the whole regression.
	if strings.Contains(got, "Selector grammar") || strings.Contains(got, "COMB") {
		t.Errorf("error dumped the full grammar (~441 tokens); it should answer the mistake:\n%s", got)
	}
	// Budget: a wrong guess is cheap. The old dump was 1764 chars.
	if len(got) > 500 {
		t.Errorf("error is %d chars (~%d tokens); a bad selector must stay cheap:\n%s",
			len(got), len(got)/4, got)
	}
}

// The grammar has to stay REACHABLE, or a caller who genuinely is lost about
// the shape (not just one sigil) has nowhere to go now that errors are terse.
func TestSelectorQuestionMarkReturnsGrammar(t *testing.T) {
	s := startSessionFull(t, goWorkspace(t, "package main\n"), nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	r := s.callTool("node_query", map[string]any{"selector": "?"})
	if r.IsError {
		t.Fatalf(`selector "?" should return the grammar, got error: %s`, r.Content[0].Text)
	}
	for _, want := range []string{"Selector grammar", "TYPES", "PSEUDO", ":has_parent"} {
		if !strings.Contains(r.Content[0].Text, want) {
			t.Errorf("grammar missing %q", want)
		}
	}
}

// Genuinely malformed syntax is a different mistake: the caller is lost about
// the SHAPE, not one sigil, so the grammar is still the right answer there.
func TestMalformedSelectorStillGetsGrammar(t *testing.T) {
	s := startSessionFull(t, goWorkspace(t, "package main\n"), nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	r := s.callTool("node_query", map[string]any{"selector": ":root > *.md"})
	if !r.IsError {
		t.Fatal("malformed selector should error")
	}
	if !strings.Contains(r.Content[0].Text, "Selector grammar") {
		t.Errorf("malformed syntax should still get the grammar, got: %s", r.Content[0].Text)
	}
}

// A selector always arrives inside a JSON string, so a double-quoted id costs a
// SECOND escaping layer: "file#\"store.go\" #Save". Single quotes cost none.
// CSS accepts both — we documented the one that collides with the transport.
//
// This is not a nicety: quoting is MANDATORY for the common case, because to a
// CSS parser every filename is tag.class (store.go = tag `store`, class `go`).
// So the required construct was also the awkward one.
func TestSelectorIdsAcceptSingleQuotes(t *testing.T) {
	s := startSessionFull(t, goWorkspace(t, "package main\n\nfunc Save() {}\n"), nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	// ' and " must be equivalent.
	for _, sel := range []string{`file#'main.go' #Save`, `file#"main.go" #Save`} {
		q := s.callTool("node_query", map[string]any{"selector": sel})
		if q.IsError {
			t.Errorf("selector %s errored: %s", sel, q.Content[0].Text)
			continue
		}
		if !strings.Contains(q.Content[0].Text, "main.go#Save") {
			t.Errorf("selector %s didn't find Save: %s", sel, q.Content[0].Text)
		}
	}
	// :contains too — same transport, same reasoning.
	if q := s.callTool("node_query", map[string]any{"selector": `func:contains('Save')`}); q.IsError {
		t.Errorf(":contains('...') should work: %s", q.Content[0].Text)
	}
}

// An unterminated quote must name the quote the caller actually opened.
func TestUnterminatedQuoteNamesTheRightQuote(t *testing.T) {
	s := startSessionFull(t, goWorkspace(t, "package main\n"), nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	r := s.callTool("node_query", map[string]any{"selector": `file#'unterminated`})
	if !r.IsError {
		t.Fatal("unterminated quote should error")
	}
	if !strings.Contains(r.Content[0].Text, "'") {
		t.Errorf("error should name the ' the caller opened, got: %s", r.Content[0].Text)
	}
}
