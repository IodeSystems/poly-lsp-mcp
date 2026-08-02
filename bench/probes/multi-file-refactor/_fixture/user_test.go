package userstore

import "testing"

func TestStoreRoundTrip(t *testing.T) {
	s := NewStore()
	u := NewUser(UserID("u1"), "Ada")
	if !ValidID(u.ID) {
		t.Fatalf("id %q should be valid", u.ID)
	}
	s.Put(u)
	got, ok := s.Get(UserID("u1"))
	if !ok || got.Name != "Ada" {
		t.Fatalf("round trip failed: got=%v ok=%v", got, ok)
	}
}
