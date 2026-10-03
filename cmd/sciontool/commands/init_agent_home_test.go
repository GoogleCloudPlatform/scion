/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"errors"
	"os/user"
	"testing"
)

// withLookupUserByID temporarily overrides the lookupUserByID package var.
func withLookupUserByID(t *testing.T, f func(string) (*user.User, error)) {
	t.Helper()
	orig := lookupUserByID
	lookupUserByID = f
	t.Cleanup(func() { lookupUserByID = orig })
}

// TestResolveAgentHome covers every branch resolveAgentHome takes: the
// dropped-privilege case (targetUID != 0, looked up by uid), the rootless
// case (targetUID == 0, looked up by name), and the still-root case
// (targetUID == 0, not rootless, no lookup attempted at all) — each with
// both a successful lookup and a failing one, so the $HOME fallback this
// function shares with resolveAgentHome's callers (reportInitFailure's
// agentHome argument, in particular) is pinned for every combination that
// reaches it.
func TestResolveAgentHome(t *testing.T) {
	const fallbackHome = "/root"

	tests := []struct {
		name         string
		targetUID    int
		rootless     bool
		lookupByID   func(string) (*user.User, error)
		lookupByName func(string) (*user.User, error)
		want         string
	}{
		{
			name:      "dropped privilege, lookup by uid succeeds",
			targetUID: 1000,
			rootless:  false,
			lookupByID: func(uid string) (*user.User, error) {
				if uid != "1000" {
					t.Errorf("lookupUserByID called with %q, want %q", uid, "1000")
				}
				return &user.User{Uid: "1000", Gid: "1000", HomeDir: "/home/scion"}, nil
			},
			want: "/home/scion",
		},
		{
			name:      "dropped privilege, lookup by uid fails",
			targetUID: 1000,
			rootless:  false,
			lookupByID: func(string) (*user.User, error) {
				return nil, errors.New("no such user")
			},
			want: fallbackHome,
		},
		{
			name:      "rootless, lookup by name succeeds",
			targetUID: 0,
			rootless:  true,
			lookupByName: func(username string) (*user.User, error) {
				if username != "scion" {
					t.Errorf("scionUserLookup called with %q, want %q", username, "scion")
				}
				return &user.User{Uid: "0", Gid: "0", HomeDir: "/home/scion"}, nil
			},
			want: "/home/scion",
		},
		{
			name:      "rootless, lookup by name fails",
			targetUID: 0,
			rootless:  true,
			lookupByName: func(string) (*user.User, error) {
				return nil, errors.New("no such user")
			},
			want: fallbackHome,
		},
		{
			name:      "still root, not rootless: no lookup attempted",
			targetUID: 0,
			rootless:  false,
			want:      fallbackHome,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", fallbackHome)
			byIDCalled := false
			byNameCalled := false
			withLookupUserByID(t, func(uid string) (*user.User, error) {
				byIDCalled = true
				if tt.lookupByID == nil {
					t.Fatalf("lookupUserByID called but this case does not expect it (uid=%q)", uid)
				}
				return tt.lookupByID(uid)
			})
			withScionUserLookup(t, func(username string) (*user.User, error) {
				byNameCalled = true
				if tt.lookupByName == nil {
					t.Fatalf("scionUserLookup called but this case does not expect it (username=%q)", username)
				}
				return tt.lookupByName(username)
			})

			got := resolveAgentHome(tt.targetUID, tt.rootless)
			if got != tt.want {
				t.Errorf("resolveAgentHome(%d, %v) = %q, want %q", tt.targetUID, tt.rootless, got, tt.want)
			}
			if tt.lookupByID != nil && !byIDCalled {
				t.Error("expected lookupUserByID to be called, it wasn't")
			}
			if tt.lookupByID == nil && byIDCalled {
				t.Error("lookupUserByID was called, expected no lookup by uid for this case")
			}
			if tt.lookupByName != nil && !byNameCalled {
				t.Error("expected scionUserLookup to be called, it wasn't")
			}
			if tt.lookupByName == nil && byNameCalled {
				t.Error("scionUserLookup was called, expected no lookup by name for this case")
			}
		})
	}
}
