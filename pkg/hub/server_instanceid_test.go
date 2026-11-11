package hub

import (
	"strings"
	"testing"

	"github.com/google/uuid"
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

// TestNewInstanceID_PodNameKeepsUniqueSuffix pins the per-replica identity
// that hub metrics and traces export as service.instance.id
// (ptone/scion#3619): with POD_NAME set the ID is prefixed with it, and it
// still ends in a fresh random UUID, so two processes never share an ID,
// even if they share a pod name (a restarted container) or a hub ID.
func TestNewInstanceID_PodNameKeepsUniqueSuffix(t *testing.T) {
	t.Setenv("POD_NAME", "scion-hub-7d9f-abcde")
	a, b := newInstanceID(), newInstanceID()
	if a == b {
		t.Fatalf("two processes with the same POD_NAME share instance ID %q", a)
	}
	for _, id := range []string{a, b} {
		suffix, ok := strings.CutPrefix(id, "scion-hub-7d9f-abcde-")
		if !ok {
			t.Fatalf("instance ID %q lacks the POD_NAME prefix", id)
		}
		if _, err := uuid.Parse(suffix); err != nil {
			t.Fatalf("instance ID %q does not end in a UUID: %v", id, err)
		}
	}
}
