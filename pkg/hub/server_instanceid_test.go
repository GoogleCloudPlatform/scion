package hub

import (
	"testing"
)

func TestNewInstanceID_NonEmpty(t *testing.T) {
	id := newInstanceID()
	if id == "" {
		t.Fatal("newInstanceID() returned empty string")
	}
}

func TestNewInstanceID_Unique(t *testing.T) {
	ids := make(map[string]struct{}, 100)
	for i := 0; i < 100; i++ {
		id := newInstanceID()
		if _, exists := ids[id]; exists {
			t.Fatalf("duplicate instanceID on call %d: %s", i, id)
		}
		ids[id] = struct{}{}
	}
}

func TestInstanceID_AccessorMatchesField(t *testing.T) {
	s := &Server{instanceID: newInstanceID()}
	if s.InstanceID() == "" {
		t.Fatal("InstanceID() returned empty string")
	}
	if s.InstanceID() != s.instanceID {
		t.Fatal("InstanceID() does not match instanceID field")
	}
}

func TestInstanceID_TwoServersDistinct(t *testing.T) {
	s1 := &Server{instanceID: newInstanceID()}
	s2 := &Server{instanceID: newInstanceID()}
	if s1.InstanceID() == s2.InstanceID() {
		t.Fatalf("two Servers share the same InstanceID: %s", s1.InstanceID())
	}
}
