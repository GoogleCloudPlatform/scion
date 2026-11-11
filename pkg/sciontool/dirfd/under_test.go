//go:build unix

/*
Copyright 2026 The Scion Authors.
*/

package dirfd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// readUnder resolves base/rel with OpenParentNoFollowUnder and reads the
// leaf with ReadAtNoFollowStat, closing the directory descriptor.
func readUnder(t *testing.T, base, rel string, maxBytes int64) (string, error) {
	t.Helper()
	fd, leaf, err := OpenParentNoFollowUnder(base, rel)
	if err != nil {
		return "", err
	}
	defer func() { _ = syscall.Close(fd) }()
	data, _, err := ReadAtNoFollowStat(fd, leaf, maxBytes)
	return string(data), err
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestOpenParentNoFollowUnder_ReadsRegularFile(t *testing.T) {
	base := t.TempDir()
	mustWrite(t, filepath.Join(base, "a", "b", "file"), "hello")

	got, err := readUnder(t, base, "a/b/file", 64)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

// TestOpenParentNoFollowUnder_FollowsLinkedBase covers the host layout
// where the base directory is reached through a symbolic link (for
// example /home -> /var/home): the base is trusted and opened normally.
func TestOpenParentNoFollowUnder_FollowsLinkedBase(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	mustWrite(t, filepath.Join(realDir, "home", "file"), "via-link")
	link := filepath.Join(root, "linked")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	got, err := readUnder(t, filepath.Join(link, "home"), "file", 64)
	if err != nil {
		t.Fatalf("read through linked base: %v", err)
	}
	if got != "via-link" {
		t.Errorf("got %q, want %q", got, "via-link")
	}
}

func TestOpenParentNoFollowUnder_RefusesLinkBelowBase(t *testing.T) {
	base := t.TempDir()
	other := t.TempDir()
	mustWrite(t, filepath.Join(other, "file"), "elsewhere")
	if err := os.Symlink(other, filepath.Join(base, "sub")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if got, err := readUnder(t, base, "sub/file", 64); err == nil {
		t.Fatalf("linked directory below base was followed, got %q", got)
	}
}

func TestOpenParentNoFollowUnder_RejectsBadRel(t *testing.T) {
	base := t.TempDir()
	for _, rel := range []string{"", ".", "/abs/file", "..", "../file", "a/../../file", "a/../b"} {
		if fd, _, err := OpenParentNoFollowUnder(base, rel); err == nil {
			_ = syscall.Close(fd)
			t.Errorf("rel %q was accepted", rel)
		}
	}
	if fd, _, err := OpenParentNoFollowUnder("", "file"); err == nil {
		_ = syscall.Close(fd)
		t.Error("empty base was accepted")
	}
}

func TestReadAtNoFollowStat_RefusesLeafLink(t *testing.T) {
	base := t.TempDir()
	mustWrite(t, filepath.Join(base, "target"), "target-content")
	if err := os.Symlink(filepath.Join(base, "target"), filepath.Join(base, "file")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if got, err := readUnder(t, base, "file", 64); err == nil {
		t.Fatalf("leaf link was followed, got %q", got)
	}
}

func TestReadAtNoFollowStat_RefusesHardLink(t *testing.T) {
	base := t.TempDir()
	mustWrite(t, filepath.Join(base, "target"), "target-content")
	if err := os.Link(filepath.Join(base, "target"), filepath.Join(base, "file")); err != nil {
		t.Fatalf("link: %v", err)
	}
	_, err := readUnder(t, base, "file", 64)
	if !errors.Is(err, ErrNotSingleLinkRegular) {
		t.Fatalf("err = %v, want ErrNotSingleLinkRegular", err)
	}
}

func TestReadAtNoFollowStat_RefusesFIFOWithoutBlocking(t *testing.T) {
	base := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(base, "file"), 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	type result struct {
		data string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		fd, leaf, err := OpenParentNoFollowUnder(base, "file")
		if err != nil {
			done <- result{err: err}
			return
		}
		defer func() { _ = syscall.Close(fd) }()
		data, _, err := ReadAtNoFollowStat(fd, leaf, 64)
		done <- result{string(data), err}
	}()
	select {
	case r := <-done:
		if !errors.Is(r.err, ErrNotSingleLinkRegular) {
			t.Fatalf("err = %v, want ErrNotSingleLinkRegular", r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("read blocked on a FIFO with no writer")
	}
}

func TestReadAtNoFollowStat_SizeLimit(t *testing.T) {
	base := t.TempDir()
	mustWrite(t, filepath.Join(base, "exact"), strings.Repeat("x", 16))
	mustWrite(t, filepath.Join(base, "over"), strings.Repeat("x", 17))

	got, err := readUnder(t, base, "exact", 16)
	if err != nil || len(got) != 16 {
		t.Fatalf("file at the limit: got %d bytes, err %v", len(got), err)
	}
	got, err = readUnder(t, base, "over", 16)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if got != "" {
		t.Errorf("oversized file returned %q, want nothing", got)
	}
}
