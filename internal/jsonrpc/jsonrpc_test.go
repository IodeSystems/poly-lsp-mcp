package jsonrpc

import (
	"bufio"
	"bytes"
	"encoding/json"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	buf := &bytes.Buffer{}
	out := &Message{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  "initialize",
		Params:  json.RawMessage(`{}`),
	}
	if err := Write(buf, out); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := Read(bufio.NewReader(buf))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Method != "initialize" || string(got.ID) != "1" || string(got.Params) != "{}" {
		t.Errorf("got %+v", got)
	}
}

func TestIsNotification(t *testing.T) {
	cases := []struct {
		name string
		m    Message
		want bool
	}{
		{"no id", Message{Method: "x"}, true},
		{"null id", Message{Method: "x", ID: json.RawMessage(`null`)}, true},
		{"with id", Message{Method: "x", ID: json.RawMessage(`1`)}, false},
	}
	for _, c := range cases {
		if got := c.m.IsNotification(); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestReadRejectsMissingContentLength(t *testing.T) {
	buf := bytes.NewBufferString("\r\n{}\r\n")
	if _, err := Read(bufio.NewReader(buf)); err == nil {
		t.Error("expected error for missing Content-Length")
	}
}
