/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	state "github.com/GoogleCloudPlatform/scion/pkg/agent/state"
)

// stickyActivity is the activity every refusal test plants where following
// the symlink or reading the oversized file would find it, so a regression
// to a plain read shows up as a sticky result instead of "".
var stickyActivity = string(state.ActivityWaitingForInput)

// agentInfoJSONOfSize returns a valid agent-info.json body of exactly size
// bytes whose activity is the given value, padded with a filler field.
func agentInfoJSONOfSize(t *testing.T, activity string, size int) []byte {
	t.Helper()
	prefix := `{"activity":"` + activity + `","pad":"`
	suffix := `"}`
	n := size - len(prefix) - len(suffix)
	if n < 0 {
		t.Fatalf("size %d too small for agent-info.json body", size)
	}
	body := prefix + strings.Repeat("x", n) + suffix
	if len(body) != size {
		t.Fatalf("body length = %d, want %d", len(body), size)
	}
	return []byte(body)
}

func writeAgentInfo(t *testing.T, dir string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, "agent-info.json")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatalf("write agent-info.json: %v", err)
	}
	return p
}

func TestReadLocalActivity_NormalFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeAgentInfo(t, home, []byte(`{"activity":"`+stickyActivity+`"}`))

	if got := readLocalActivity(); got != stickyActivity {
		t.Fatalf("readLocalActivity() = %q, want %q", got, stickyActivity)
	}
}

func TestReadLocalActivity_MissingFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if got := readLocalActivity(); got != "" {
		t.Fatalf("readLocalActivity() with no file = %q, want empty", got)
	}
}

// A symlink at agent-info.json is refused, not followed, even though its
// target holds a sticky activity.
func TestReadLocalActivity_SymlinkLeafNotFollowed(t *testing.T) {
	home := t.TempDir()
	elsewhere := t.TempDir()
	target := writeAgentInfo(t, elsewhere, []byte(`{"activity":"`+stickyActivity+`"}`))
	if err := os.Symlink(target, filepath.Join(home, "agent-info.json")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	t.Setenv("HOME", home)

	if got := readLocalActivity(); got != "" {
		t.Fatalf("readLocalActivity() through symlinked file = %q, want empty (symlink must not be followed)", got)
	}
}

// A symlinked directory component (HOME itself) is refused too.
func TestReadLocalActivity_SymlinkedHomeDirNotFollowed(t *testing.T) {
	realHome := t.TempDir()
	writeAgentInfo(t, realHome, []byte(`{"activity":"`+stickyActivity+`"}`))
	link := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(realHome, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	t.Setenv("HOME", link)

	if got := readLocalActivity(); got != "" {
		t.Fatalf("readLocalActivity() through symlinked HOME = %q, want empty (symlink must not be followed)", got)
	}
}

// A file one byte over agentInfoMaxBytes is refused, even though it is valid
// JSON carrying a sticky activity.
func TestReadLocalActivity_OversizedRefused(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeAgentInfo(t, home, agentInfoJSONOfSize(t, stickyActivity, agentInfoMaxBytes+1))

	if got := readLocalActivity(); got != "" {
		t.Fatalf("readLocalActivity() on oversized file = %q, want empty (read must be capped)", got)
	}
}

// A file of exactly agentInfoMaxBytes is still read.
func TestReadLocalActivity_AtCapReads(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeAgentInfo(t, home, agentInfoJSONOfSize(t, stickyActivity, agentInfoMaxBytes))

	if got := readLocalActivity(); got != stickyActivity {
		t.Fatalf("readLocalActivity() at the size cap = %q, want %q", got, stickyActivity)
	}
}
