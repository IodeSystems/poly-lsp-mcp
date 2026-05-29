package bindings

import "testing"

func TestParseProtoExtractsAllDeclarationKinds(t *testing.T) {
	content := []byte(`syntax = "proto3";

package x;

message UserID {
  int64 value = 1;
}

enum Status {
  ACTIVE = 0;
  REVOKED = 1;
}

service UserService {
  rpc GetUser(UserID) returns (User);
  rpc DeleteUser(UserID) returns (User);
}

message User {
  UserID id = 1;
}
`)
	entities := parseProto(content)
	got := map[string]int{}
	for _, e := range entities {
		got[e.Name]++
	}
	for _, want := range []string{"UserID", "User", "Status", "UserService", "GetUser", "DeleteUser"} {
		if got[want] == 0 {
			t.Errorf("missing %q from parseProto: %+v", want, got)
		}
	}
}

func TestParseProtoSkipsCommentsAndStrings(t *testing.T) {
	content := []byte(`syntax = "proto3";

// message FakeInComment {
//   not a real declaration
// }

/* enum FakeBlock {
   ALSO_FAKE = 0;
} */

message Real {
  string note = 1;  // "message FakeInString"
}
`)
	entities := parseProto(content)
	for _, e := range entities {
		switch e.Name {
		case "FakeInComment", "FakeBlock", "FakeInString":
			t.Errorf("regex matched inside comment/string: %+v", e)
		}
	}
	found := false
	for _, e := range entities {
		if e.Name == "Real" {
			found = true
		}
	}
	if !found {
		t.Error("missed the real message declaration")
	}
}

func TestParseProtoTracksLineColumnOfName(t *testing.T) {
	content := []byte("syntax = \"proto3\";\n\nmessage Foo {\n}\n")
	entities := parseProto(content)
	if len(entities) != 1 {
		t.Fatalf("got %d entities, want 1: %+v", len(entities), entities)
	}
	if entities[0].Line != 3 {
		t.Errorf("line = %d, want 3", entities[0].Line)
	}
	// "message Foo": F starts at column 9.
	if entities[0].Col != 9 {
		t.Errorf("col = %d, want 9", entities[0].Col)
	}
}
