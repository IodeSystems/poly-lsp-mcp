package multiplex

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestDiagnosticStorePutGet(t *testing.T) {
	s := NewDiagnosticStore()
	if got := s.Get("file:///x.go"); got != nil {
		t.Errorf("unpublished URI = %+v, want nil", got)
	}
	s.put("file:///x.go", []Diagnostic{{Message: "boom", Severity: 1}})
	got := s.Get("file:///x.go")
	if len(got) != 1 || got[0].Message != "boom" {
		t.Errorf("Get = %+v, want one boom", got)
	}
}

func TestDiagnosticStoreWaitAfterImmediateReturn(t *testing.T) {
	s := NewDiagnosticStore()
	s.put("file:///x.go", []Diagnostic{{Message: "old"}})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	// since=0 < current gen=1 → WaitAfter must NOT block.
	got := s.WaitAfter(ctx, "file:///x.go", 0)
	if len(got) != 1 || got[0].Message != "old" {
		t.Errorf("WaitAfter = %+v", got)
	}
	if ctx.Err() != nil {
		t.Errorf("ctx expired; WaitAfter blocked when it shouldn't have")
	}
}

func TestDiagnosticStoreWaitAfterBlocksThenWakes(t *testing.T) {
	s := NewDiagnosticStore()
	// Capture gen=0 before any publish, then a delayed put should wake.
	since := s.Gen("file:///x.go")

	go func() {
		time.Sleep(20 * time.Millisecond)
		s.put("file:///x.go", []Diagnostic{{Message: "fresh"}})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	got := s.WaitAfter(ctx, "file:///x.go", since)
	if len(got) != 1 || got[0].Message != "fresh" {
		t.Errorf("WaitAfter = %+v, want one 'fresh'", got)
	}
	if ctx.Err() != nil {
		t.Errorf("timed out unexpectedly")
	}
}

func TestDiagnosticStoreWaitAfterTimeoutReturnsLatest(t *testing.T) {
	s := NewDiagnosticStore()
	s.put("file:///x.go", []Diagnostic{{Message: "stale"}})
	since := s.Gen("file:///x.go") // since==current; nothing newer will come

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	got := s.WaitAfter(ctx, "file:///x.go", since)
	if ctx.Err() == nil {
		t.Errorf("expected ctx to time out")
	}
	// Returns the latest known snapshot — agent will flag as
	// `diagnosticsAvailable: false` or label as stale.
	if len(got) != 1 || got[0].Message != "stale" {
		t.Errorf("got = %+v, want stale snapshot", got)
	}
}

func TestDiagnosticStoreAttachParsesPublish(t *testing.T) {
	// Stand up a notification-handler hook directly (no real child).
	s := NewDiagnosticStore()
	var handler func(method string, params json.RawMessage)
	fakeChild := struct {
		setHandler func(fn func(method string, params json.RawMessage))
	}{
		setHandler: func(fn func(method string, params json.RawMessage)) { handler = fn },
	}
	// Manually replicate what Attach does so we can fire the
	// notification without spawning a real child. (Attach is exercised
	// through the live LSP integration test elsewhere.)
	fakeChild.setHandler(func(method string, params json.RawMessage) {
		if method != "textDocument/publishDiagnostics" {
			return
		}
		var p struct {
			URI         string       `json:"uri"`
			Diagnostics []Diagnostic `json:"diagnostics"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return
		}
		if p.Diagnostics == nil {
			p.Diagnostics = []Diagnostic{}
		}
		s.put(p.URI, p.Diagnostics)
	})

	if handler == nil {
		t.Fatal("handler not installed")
	}
	handler("textDocument/publishDiagnostics", json.RawMessage(`{
		"uri": "file:///x.go",
		"diagnostics": [
			{"range":{"start":{"line":3,"character":0},"end":{"line":3,"character":5}},
			 "severity":1,"message":"oops","source":"gopls"}
		]
	}`))

	got := s.Get("file:///x.go")
	if len(got) != 1 {
		t.Fatalf("got %d diags, want 1", len(got))
	}
	if got[0].Message != "oops" || got[0].Severity != 1 || got[0].Source != "gopls" {
		t.Errorf("diag = %+v", got[0])
	}
	if got[0].Range.Start.Line != 3 || got[0].Range.End.Character != 5 {
		t.Errorf("range = %+v", got[0].Range)
	}
}

func TestDiagnosticStoreEmptyListClearsErrors(t *testing.T) {
	// gopls publishes []diagnostics to mean "this file is now clean".
	// Last-write-wins must honor that.
	s := NewDiagnosticStore()
	s.put("file:///x.go", []Diagnostic{{Message: "broken"}})
	s.put("file:///x.go", []Diagnostic{}) // cleared
	got := s.Get("file:///x.go")
	if got == nil || len(got) != 0 {
		t.Errorf("got = %+v, want empty (non-nil)", got)
	}
}

// TestWaitAnyAfterSettlesAcrossURIs pins the primitive the sibling
// rollup depends on: a publish for a DIFFERENT URI than the one just
// waited on must still wake the waiter. Without this, the rollup samples
// the store while the sibling's message is still in flight — which is
// how TestSiblingDiagnosticsRollup failed roughly 1 run in 3.
func TestWaitAnyAfterSettlesAcrossURIs(t *testing.T) {
	s := NewDiagnosticStore()

	// A publish that already happened is reported without blocking.
	s.put("file:///a.go", []Diagnostic{{Message: "boom"}})
	if got, more := s.WaitAnyAfter(context.Background(), 0, time.Second); !more || got != 1 {
		t.Fatalf("WaitAnyAfter(since=0) = (%d, %v), want (1, true)", got, more)
	}

	// A quiet store reports no activity once the window elapses, and
	// does NOT block for longer than the window.
	start := time.Now()
	if _, more := s.WaitAnyAfter(context.Background(), 1, 60*time.Millisecond); more {
		t.Error("WaitAnyAfter reported activity on a quiet store")
	}
	if waited := time.Since(start); waited > 700*time.Millisecond {
		t.Errorf("quiet wait took %v, want ~60ms", waited)
	}

	// The sibling case: the waiter is watching total generations, and a
	// publish for a URI it never asked about wakes it.
	go func() {
		time.Sleep(30 * time.Millisecond)
		s.put("file:///sibling.go", []Diagnostic{{Message: "fallout"}})
	}()
	got, more := s.WaitAnyAfter(context.Background(), 1, 2*time.Second)
	if !more || got != 2 {
		t.Fatalf("WaitAnyAfter woke as (%d, %v), want (2, true) from the sibling publish", got, more)
	}
	if diags := s.Get("file:///sibling.go"); len(diags) != 1 {
		t.Errorf("sibling diagnostics = %+v, want 1", diags)
	}
}

// TestWaitAnyAfterHonoursContext ensures a cancelled caller doesn't hang
// for the whole quiet window.
func TestWaitAnyAfterHonoursContext(t *testing.T) {
	s := NewDiagnosticStore()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if _, more := s.WaitAnyAfter(ctx, 0, 10*time.Second); more {
		t.Error("reported activity after cancellation")
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Errorf("cancelled wait took %v, want ~20ms", waited)
	}
}
