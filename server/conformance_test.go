package server

// Conformance pack. These tests pin the LSP / JSON-RPC base-protocol
// behaviors the spec requires us to enforce — lifecycle gating, exit
// code semantics, jsonrpc field validation — independently of any
// language-specific handler. Add tests here whenever spec-mandated
// behavior is observable from the wire.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/internal/jsonrpc"
)

// sendRaw writes a framed message with the supplied JSON body straight
// to the server, bypassing the typed helpers. Use this when the test
// needs to send something jsonrpc.Write would refuse to produce (missing
// jsonrpc field, wrong version, etc.).
func (s *lspSession) sendRaw(body []byte) {
	s.t.Helper()
	if _, err := fmt.Fprintf(s.clientW, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		s.t.Fatal(err)
	}
	if _, err := s.clientW.Write(body); err != nil {
		s.t.Fatal(err)
	}
}

// recvMessage reads one response from the server. Use when the response
// shape isn't suitable for the existing typed helpers.
func (s *lspSession) recvMessage() *jsonrpc.Message {
	s.t.Helper()
	msg, err := jsonrpc.Read(s.clientR)
	if err != nil {
		s.t.Fatalf("recvMessage: %v", err)
	}
	return msg
}

// waitForExit consumes the Serve goroutine's return value and asserts
// it matches want (use errors.Is semantics). Tests that drive the
// lifecycle by hand use this instead of close().
func (s *lspSession) waitForExit(want error) {
	s.t.Helper()
	select {
	case got := <-s.done:
		if want == nil && got != nil {
			s.t.Errorf("Serve returned %v, want nil", got)
		}
		if want != nil && !errors.Is(got, want) {
			s.t.Errorf("Serve returned %v, want %v", got, want)
		}
	case <-time.After(2 * time.Second):
		s.srvIn.Close()
		s.t.Fatal("Serve did not return within 2s")
	}
}

// nextID assigns a fresh JSON-RPC id. Same numbering as request() so
// raw and typed sends share the id space.
func (s *lspSession) nextRawID() json.RawMessage {
	id := atomic.AddInt64(&s.nextID, 1)
	b, _ := json.Marshal(id)
	return b
}

// ---------------------------------------------------------------- lifecycle

func TestConformancePreInitRequestRejectedWith32002(t *testing.T) {
	s := startSession(t)

	rawID := s.nextRawID()
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(rawID),
		"method":  "workspace/symbol",
		"params":  map[string]any{"query": "x"},
	})
	s.sendRaw(body)

	resp := s.recvMessage()
	if resp.Error == nil {
		t.Fatalf("expected error, got result=%s", resp.Result)
	}
	if resp.Error.Code != -32002 {
		t.Errorf("error code = %d, want -32002 ServerNotInitialized", resp.Error.Code)
	}

	// Cleanup: pre-init exit terminates with ErrExitWithoutShutdown.
	s.notify("exit", nil)
	s.waitForExit(ErrExitWithoutShutdown)
}

func TestConformancePreInitNotificationDropped(t *testing.T) {
	s := startSession(t)

	// Notifications before initialize must be silently dropped per spec.
	// Send a custom one and then issue initialize — the initialize
	// response proves the server is still alive and responsive.
	s.notify("$/customNotificationBeforeInit", map[string]any{"hello": "world"})

	resp := s.request("initialize", map[string]any{})
	if resp.Error != nil {
		t.Fatalf("initialize after dropped notification errored: %+v", resp.Error)
	}
	s.notify("initialized", map[string]any{})

	s.request("shutdown", nil)
	s.notify("exit", nil)
	s.waitForExit(nil)
}

func TestConformancePreInitExitTerminatesWithError(t *testing.T) {
	// Spec: exit notification before initialize is allowed but must
	// terminate the process with a non-zero exit code (we surface this
	// as ErrExitWithoutShutdown so main.go log.Fatals).
	s := startSession(t)
	s.notify("exit", nil)
	s.waitForExit(ErrExitWithoutShutdown)
}

func TestConformanceDoubleInitializeRejected(t *testing.T) {
	s := startSession(t)

	first := s.request("initialize", map[string]any{})
	if first.Error != nil {
		t.Fatalf("first initialize errored: %+v", first.Error)
	}
	s.notify("initialized", map[string]any{})

	// Second initialize must return an error response. Use sendRaw to
	// avoid request()'s t.Fatal on Error.
	rawID := s.nextRawID()
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(rawID),
		"method":  "initialize",
		"params":  map[string]any{},
	})
	s.sendRaw(body)
	second := s.recvMessage()
	if second.Error == nil {
		t.Fatalf("second initialize did not error: %s", second.Result)
	}
	if second.Error.Code != -32600 {
		t.Errorf("error code = %d, want -32600 InvalidRequest", second.Error.Code)
	}

	s.request("shutdown", nil)
	s.notify("exit", nil)
	s.waitForExit(nil)
}

func TestConformancePostShutdownRequestRejectedWith32600(t *testing.T) {
	s := startSession(t)

	s.request("initialize", map[string]any{})
	s.notify("initialized", map[string]any{})
	if r := s.request("shutdown", nil); r.Error != nil {
		t.Fatalf("shutdown errored: %+v", r.Error)
	}

	// Send a workspace/symbol after shutdown — must be rejected. Use
	// sendRaw + recvMessage because the request() helper t.Fatals on
	// any error response.
	rawID := s.nextRawID()
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(rawID),
		"method":  "workspace/symbol",
		"params":  map[string]any{"query": "x"},
	})
	s.sendRaw(body)
	resp := s.recvMessage()
	if resp.Error == nil {
		t.Fatalf("post-shutdown request did not error: %s", resp.Result)
	}
	if resp.Error.Code != -32600 {
		t.Errorf("error code = %d, want -32600 InvalidRequest", resp.Error.Code)
	}

	s.notify("exit", nil)
	s.waitForExit(nil)
}

func TestConformancePostShutdownNotificationDropped(t *testing.T) {
	s := startSession(t)

	s.request("initialize", map[string]any{})
	s.notify("initialized", map[string]any{})
	s.request("shutdown", nil)

	// Notifications after shutdown should be silently dropped. Send one,
	// then send exit, and verify the server still exits cleanly.
	s.notify("$/customNotificationAfterShutdown", map[string]any{})
	s.notify("exit", nil)
	s.waitForExit(nil)
}

func TestConformanceExitWithoutShutdownReturnsErrExitWithoutShutdown(t *testing.T) {
	s := startSession(t)

	s.request("initialize", map[string]any{})
	s.notify("initialized", map[string]any{})
	// Skip shutdown, go straight to exit. Spec requires exit-code-1.
	s.notify("exit", nil)
	s.waitForExit(ErrExitWithoutShutdown)
}

func TestConformanceCleanShutdownReturnsNil(t *testing.T) {
	s := startSession(t)
	s.request("initialize", map[string]any{})
	s.notify("initialized", map[string]any{})
	s.request("shutdown", nil)
	s.notify("exit", nil)
	s.waitForExit(nil)
}

// -------------------------------------------------------- jsonrpc validation

func TestConformanceMissingJsonrpcFieldRejected(t *testing.T) {
	s := startSession(t)

	// Initialize through normal channels so the lifecycle gate is open;
	// then send a raw message without jsonrpc field.
	s.request("initialize", map[string]any{})
	s.notify("initialized", map[string]any{})

	rawID := s.nextRawID()
	body, _ := json.Marshal(map[string]any{
		"id":     json.RawMessage(rawID),
		"method": "workspace/symbol",
		"params": map[string]any{},
	})
	s.sendRaw(body)

	resp := s.recvMessage()
	if resp.Error == nil {
		t.Fatalf("expected -32600, got result=%s", resp.Result)
	}
	if resp.Error.Code != -32600 {
		t.Errorf("error code = %d, want -32600 InvalidRequest", resp.Error.Code)
	}

	s.request("shutdown", nil)
	s.notify("exit", nil)
	s.waitForExit(nil)
}

func TestConformanceWrongJsonrpcVersionRejected(t *testing.T) {
	s := startSession(t)
	s.request("initialize", map[string]any{})
	s.notify("initialized", map[string]any{})

	rawID := s.nextRawID()
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "1.0",
		"id":      json.RawMessage(rawID),
		"method":  "workspace/symbol",
		"params":  map[string]any{},
	})
	s.sendRaw(body)

	resp := s.recvMessage()
	if resp.Error == nil {
		t.Fatalf("expected error, got result=%s", resp.Result)
	}
	if resp.Error.Code != -32600 {
		t.Errorf("error code = %d, want -32600 InvalidRequest", resp.Error.Code)
	}

	s.request("shutdown", nil)
	s.notify("exit", nil)
	s.waitForExit(nil)
}

func TestConformanceBadJsonrpcFieldNotificationDropped(t *testing.T) {
	s := startSession(t)
	s.request("initialize", map[string]any{})
	s.notify("initialized", map[string]any{})

	// Notification with wrong jsonrpc version: must be dropped silently.
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "1.0",
		"method":  "$/customNotification",
		"params":  map[string]any{},
	})
	s.sendRaw(body)

	// Issue a real request and assert the server is still responsive.
	resp := s.request("workspace/symbol", map[string]any{"query": ""})
	if resp.Error != nil {
		t.Errorf("server stopped responding after bad-notification: %+v", resp.Error)
	}

	s.request("shutdown", nil)
	s.notify("exit", nil)
	s.waitForExit(nil)
}

// ---------------------------------------------------------- response shapes

func TestConformanceShutdownResultIsNull(t *testing.T) {
	s := startSession(t)
	s.request("initialize", map[string]any{})
	s.notify("initialized", map[string]any{})

	resp := s.request("shutdown", nil)
	if string(resp.Result) != "null" {
		t.Errorf("shutdown result = %s, want null", resp.Result)
	}

	s.notify("exit", nil)
	s.waitForExit(nil)
}

func TestConformanceReferencesEmptyIsArrayNotNull(t *testing.T) {
	// LSP requires Location[] response for textDocument/references with
	// `[]` semantics for "no results". Returning null breaks clients that
	// expect to iterate without a guard.
	s := startSession(t)
	s.request("initialize", map[string]any{})
	s.notify("initialized", map[string]any{})

	resp := s.request("textDocument/references", map[string]any{
		"textDocument": map[string]any{"uri": "file:///nonexistent.go"},
		"position":     map[string]any{"line": 0, "character": 0},
		"context":      map[string]any{"includeDeclaration": true},
	})
	if string(resp.Result) != "[]" {
		t.Errorf("references result = %s, want []", resp.Result)
	}

	s.request("shutdown", nil)
	s.notify("exit", nil)
	s.waitForExit(nil)
}

func TestConformanceWorkspaceSymbolEmptyIsArrayNotNull(t *testing.T) {
	s := startSession(t)
	s.request("initialize", map[string]any{}) // no rootUri → no index
	s.notify("initialized", map[string]any{})

	resp := s.request("workspace/symbol", map[string]any{"query": "anything"})
	if string(resp.Result) != "[]" {
		t.Errorf("workspace/symbol result = %s, want []", resp.Result)
	}

	s.request("shutdown", nil)
	s.notify("exit", nil)
	s.waitForExit(nil)
}

// ---------------------------------------------------------- framing edges

func TestConformanceMalformedJSONTerminatesConnection(t *testing.T) {
	// A body that isn't decodable JSON forces jsonrpc.Read to return a
	// decode error. Serve must terminate; main.go log.Fatals.
	s := startSession(t)

	s.sendRaw([]byte("{not valid json"))
	select {
	case got := <-s.done:
		if got == nil {
			t.Errorf("Serve returned nil for malformed JSON, want error")
		}
		if got != nil && !strings.Contains(got.Error(), "decode body") {
			// Accept any non-EOF error here; just sanity-check the
			// message hints at the cause.
			t.Logf("Serve error: %v", got)
		}
	case <-time.After(2 * time.Second):
		s.srvIn.Close()
		t.Fatal("Serve did not return within 2s after malformed JSON")
	}
}

func TestConformanceEOFOnIdleSessionReturnsNil(t *testing.T) {
	// EOF without an exit notification is treated as a clean stream end
	// (process supervisors will kill stdin when the editor crashes; we
	// shouldn't exit-code 1 over that).
	s := startSession(t)
	s.clientW.Close()
	s.waitForExit(nil)
}

// ---------------------------------------------------------- regression bait

func TestConformanceContentLengthMustPrecedeBody(t *testing.T) {
	// jsonrpc.Read already enforces this; the test pins the contract.
	r := bufio.NewReader(bytes.NewBufferString("\r\n{}"))
	if _, err := jsonrpc.Read(r); err == nil {
		t.Error("expected error reading framed message with no Content-Length")
	}
}

func TestConformanceFramingRoundTrip(t *testing.T) {
	// jsonrpc.Write then Read should round-trip every message field.
	out := &bytes.Buffer{}
	msg := &jsonrpc.Message{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`42`),
		Method:  "textDocument/hover",
		Params:  json.RawMessage(`{"x":1}`),
	}
	if err := jsonrpc.Write(out, msg); err != nil {
		t.Fatal(err)
	}
	// Confirm header shape.
	if !bytes.Contains(out.Bytes(), []byte("Content-Length: ")) {
		t.Errorf("Content-Length header missing from %q", out.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("\r\n\r\n")) {
		t.Errorf("header/body separator missing")
	}
	got, err := jsonrpc.Read(bufio.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if got.Method != msg.Method || string(got.ID) != string(msg.ID) || string(got.Params) != string(msg.Params) {
		t.Errorf("round trip mismatch: got %+v want %+v", got, msg)
	}
}

