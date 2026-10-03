/*
Copyright 2026 The Scion Authors.
*/

package dirfd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ---------------- ReadFileNoFollow / ReadAtNoFollow ----------------

func TestReadFileNoFollow_NormalRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.json")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	data, err := ReadFileNoFollow(path, 1024)
	if err != nil {
		t.Fatalf("ReadFileNoFollow: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("content = %q, want %q", data, "hello")
	}
}

func TestReadFileNoFollow_MissingFileReportsNotExist(t *testing.T) {
	dir := t.TempDir()
	_, err := ReadFileNoFollow(filepath.Join(dir, "missing"), 1024)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected os.ErrNotExist, got %v", err)
	}
}

// TestReadFileNoFollow_RefusesSymlinkAtLeaf fails if the leaf open ever
// drops O_NOFOLLOW: a symlink swapped in at the target path must be refused
// rather than transparently read through.
func TestReadFileNoFollow_RefusesSymlinkAtLeaf(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte("top-secret"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := ReadFileNoFollow(link, 1024)
	if err == nil {
		t.Fatal("expected an error reading through a symlink, got nil")
	}
}

// TestReadFileNoFollow_RefusesIntermediateSymlink fails if the walk to the
// leaf's parent ever stops checking anything but the final component.
func TestReadFileNoFollow_RefusesIntermediateSymlink(t *testing.T) {
	dir := t.TempDir()
	attacker := filepath.Join(dir, "attacker")
	if err := os.Mkdir(attacker, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	victim := filepath.Join(attacker, "victim")
	if err := os.WriteFile(victim, []byte("do-not-touch"), 0o600); err != nil {
		t.Fatalf("write victim: %v", err)
	}
	scionDir := filepath.Join(dir, ".scion")
	if err := os.Symlink(attacker, scionDir); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := ReadFileNoFollow(filepath.Join(scionDir, "victim"), 1024)
	if err == nil {
		t.Fatal("expected an error walking through a symlinked intermediate directory, got nil")
	}
}

// TestReadFileNoFollow_RefusesHardlinkedRegularFile fails if the Nlink==1
// check is removed: a hardlink to an unrelated (possibly root-owned) file
// still looks like an ordinary regular file to a bare S_IFREG check.
func TestReadFileNoFollow_RefusesHardlinkedRegularFile(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "original")
	if err := os.WriteFile(original, []byte("victim-content"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	linked := filepath.Join(dir, "linked")
	if err := os.Link(original, linked); err != nil {
		t.Fatalf("hardlink: %v", err)
	}

	_, err := ReadFileNoFollow(linked, 1024)
	if !errors.Is(err, ErrNotSingleLinkRegular) {
		t.Fatalf("expected ErrNotSingleLinkRegular, got %v", err)
	}
}

// TestReadFileNoFollow_BoundsOversizedContent fails if the LimitReader cap
// is removed or widened: a file one byte over the limit must be refused,
// never buffered in full.
func TestReadFileNoFollow_BoundsOversizedContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big")
	if err := os.WriteFile(path, make([]byte, 101), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := ReadFileNoFollow(path, 100)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("expected ErrTooLarge, got %v", err)
	}

	// Exactly at the limit must still succeed.
	if err := os.WriteFile(path, make([]byte, 100), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if _, err := ReadFileNoFollow(path, 100); err != nil {
		t.Fatalf("expected content exactly at the limit to succeed, got %v", err)
	}
}

// TestReadAtNoFollow_FifoDoesNotHang proves a FIFO with no writer is
// refused immediately rather than hanging the caller. This fails if
// O_NONBLOCK is dropped from the open, or if the non-regular-file check is
// removed so the code proceeds to a blocking read.
func TestReadAtNoFollow_FifoDoesNotHang(t *testing.T) {
	dir := t.TempDir()
	fifoPath := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := ReadFileNoFollow(fifoPath, 1024)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error reading a FIFO, got nil")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ReadFileNoFollow blocked on a FIFO with no writer")
	}
}

// ---------------- WriteFileNoFollow ----------------

func TestWriteFileNoFollow_NormalWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	if err := WriteFileNoFollow(path, []byte(`{"a":1}`), 0o644, 0, 0, ReplaceLeaf); err != nil {
		t.Fatalf("WriteFileNoFollow: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(data) != `{"a":1}` {
		t.Errorf("content = %q", data)
	}

	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Mode().Perm() != 0o644 {
		t.Errorf("mode = %o, want 0644", st.Mode().Perm())
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly the final file to remain, got %v", entries)
	}
}

// TestWriteFileNoFollow_ChownsTempFdBeforeRename mirrors
// writeLimitsState's own equivalent test: a non-root test can't chown to an
// arbitrary uid, but chowning to its own current uid/gid is always
// permitted, which is enough to prove the fd-based call succeeds.
func TestWriteFileNoFollow_ChownsTempFdBeforeRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	if err := WriteFileNoFollow(path, []byte("x"), 0o644, os.Getuid(), os.Getgid(), ReplaceLeaf); err != nil {
		t.Fatalf("WriteFileNoFollow: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "state.json" {
		t.Fatalf("expected only the final file to remain, got %v", entries)
	}
}

// TestWriteFileNoFollow_ChmodTargetsTempFdNotSwappedPath proves the mode is
// set via fchmod on the already-open temp fd, not by a path-based chmod
// that a symlink swapped into the temp file's directory entry could
// redirect. It replaces the temp file's directory entry with a symlink to
// an unrelated file in the window between write and chmod (via the
// package's own test hook, the same technique walk_test.go uses for the
// chown walk's races) and then asserts that unrelated file's mode was never
// touched. This fails if WriteFileNoFollow is reverted to a path-based
// os.Chmod(tmpPath, mode) call.
func TestWriteFileNoFollow_ChmodTargetsTempFdNotSwappedPath(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "root-owned-secret")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}

	finalPath := filepath.Join(dir, "agent-info.json")

	writeNoFollowPreChmodTestHook = func(tmpName string) {
		tmpPath := filepath.Join(dir, tmpName)
		if err := os.Remove(tmpPath); err != nil {
			t.Fatalf("remove temp: %v", err)
		}
		if err := os.Symlink(target, tmpPath); err != nil {
			t.Fatalf("symlink: %v", err)
		}
	}
	defer func() { writeNoFollowPreChmodTestHook = nil }()

	// The call may succeed or fail depending on what the final rename does
	// with the swapped-in symlink; either way, the target's permissions
	// must never change.
	_ = WriteFileNoFollow(finalPath, []byte(`{"a":1}`), 0o644, 0, 0, ReplaceLeaf)

	st, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat target: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("chmod followed the swapped-in symlink: target mode = %o, want unchanged 0600", st.Mode().Perm())
	}
}

// TestWriteFileNoFollow_RemovesTempOnChownFailure proves the temp file is
// cleaned up rather than left behind when a later step (here, an fchown to
// a uid this test process isn't permitted to use) fails after the file was
// already created.
func TestWriteFileNoFollow_RemovesTempOnChownFailure(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can chown to any uid; this test needs the call to fail")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	// uid 1 ("daemon" on most systems) is never the current test uid and a
	// non-root process cannot chown to it — that's the point.
	err := WriteFileNoFollow(path, []byte("x"), 0o644, 1, 1, ReplaceLeaf)
	if err == nil {
		t.Fatal("expected chown to an unpermitted uid to fail")
	}

	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		t.Fatalf("readdir: %v", rerr)
	}
	if len(entries) != 0 {
		t.Fatalf("expected the temp file to be cleaned up after a failed write, got %v", entries)
	}
}

// TestWriteFileNoFollow_InvalidLeafPolicyRejected fails if LeafPolicyUnset
// (the zero value) is ever treated as usable: every caller must explicitly
// choose ReplaceLeaf or RefuseSymlink, so the zero value must be refused,
// not silently behave as either one.
func TestWriteFileNoFollow_InvalidLeafPolicyRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	if err := WriteFileNoFollow(path, []byte("x"), 0o644, 0, 0, LeafPolicyUnset); err == nil {
		t.Fatal("expected LeafPolicyUnset to be refused, got nil")
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("expected no file to be written when the policy is invalid")
	}
}

// TestWriteFileNoFollow_LeafPolicyMatrix exercises both LeafPolicy values
// against a symlink leaf, a FIFO leaf, and a pre-existing regular file:
// ReplaceLeaf must overwrite all three; RefuseSymlink must refuse the first
// two (leaving them and whatever they point at completely untouched) and
// still overwrite the third.
func TestWriteFileNoFollow_LeafPolicyMatrix(t *testing.T) {
	for _, policy := range []LeafPolicy{ReplaceLeaf, RefuseSymlink} {
		policy := policy
		t.Run(fmt.Sprintf("policy=%d", policy), func(t *testing.T) {
			t.Run("symlink leaf", func(t *testing.T) {
				dir := t.TempDir()
				victim := filepath.Join(dir, "victim")
				if err := os.WriteFile(victim, []byte("do-not-touch"), 0o600); err != nil {
					t.Fatalf("write victim: %v", err)
				}
				path := filepath.Join(dir, "leaf")
				if err := os.Symlink(victim, path); err != nil {
					t.Fatalf("symlink: %v", err)
				}

				err := WriteFileNoFollow(path, []byte("new"), 0o644, 0, 0, policy)

				victimData, rerr := os.ReadFile(victim)
				if rerr != nil {
					t.Fatalf("read victim: %v", rerr)
				}
				if string(victimData) != "do-not-touch" {
					t.Fatalf("victim was modified through the symlink: %q", victimData)
				}

				fi, lerr := os.Lstat(path)
				if lerr != nil {
					t.Fatalf("lstat: %v", lerr)
				}
				switch policy {
				case ReplaceLeaf:
					if err != nil {
						t.Fatalf("ReplaceLeaf: expected the symlink leaf to be replaced, got %v", err)
					}
					if fi.Mode()&os.ModeSymlink != 0 {
						t.Error("ReplaceLeaf: leaf is still a symlink after the write")
					}
				case RefuseSymlink:
					if err == nil {
						t.Fatal("RefuseSymlink: expected the symlink leaf to be refused, got nil")
					}
					if fi.Mode()&os.ModeSymlink == 0 {
						t.Error("RefuseSymlink: leaf is no longer a symlink after a refused write")
					}
				}
			})

			t.Run("FIFO leaf", func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "leaf")
				if err := syscall.Mkfifo(path, 0o600); err != nil {
					t.Fatalf("mkfifo: %v", err)
				}

				done := make(chan error, 1)
				go func() {
					done <- WriteFileNoFollow(path, []byte("new"), 0o644, 0, 0, policy)
				}()

				var err error
				select {
				case err = <-done:
				case <-time.After(3 * time.Second):
					t.Fatal("WriteFileNoFollow blocked on a FIFO leaf with no reader/writer")
				}

				fi, lerr := os.Lstat(path)
				if lerr != nil {
					t.Fatalf("lstat: %v", lerr)
				}
				switch policy {
				case ReplaceLeaf:
					if err != nil {
						t.Fatalf("ReplaceLeaf: expected the FIFO leaf to be replaced, got %v", err)
					}
					if fi.Mode()&os.ModeNamedPipe != 0 {
						t.Error("ReplaceLeaf: leaf is still a FIFO after the write")
					}
				case RefuseSymlink:
					if err == nil {
						t.Fatal("RefuseSymlink: expected the FIFO leaf to be refused, got nil")
					}
					if fi.Mode()&os.ModeNamedPipe == 0 {
						t.Error("RefuseSymlink: leaf is no longer a FIFO after a refused write")
					}
				}
			})

			t.Run("regular file leaf", func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "leaf")
				if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
					t.Fatalf("write: %v", err)
				}

				if err := WriteFileNoFollow(path, []byte("new"), 0o644, 0, 0, policy); err != nil {
					t.Fatalf("policy %d: expected a regular-file leaf to be replaced, got %v", policy, err)
				}
				data, rerr := os.ReadFile(path)
				if rerr != nil {
					t.Fatalf("read back: %v", rerr)
				}
				if string(data) != "new" {
					t.Errorf("content = %q, want %q", data, "new")
				}
			})
		})
	}
}

// TestWriteFileNoFollow_TruncateInPlaceOrCreate_OverwritesSameInode proves
// that when the leaf already exists as a regular file, TruncateInPlaceOrCreate
// rewrites it IN PLACE — same inode, no create-then-rename — which is what
// avoids the EBUSY a rename over a bind-mounted regular file's directory
// entry would otherwise produce.
func TestWriteFileNoFollow_TruncateInPlaceOrCreate_OverwritesSameInode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "leaf")
	if err := os.WriteFile(path, []byte("old-content"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	var before syscall.Stat_t
	if err := syscall.Stat(path, &before); err != nil {
		t.Fatalf("stat before: %v", err)
	}

	if err := WriteFileNoFollow(path, []byte("new"), 0o644, 0, 0, TruncateInPlaceOrCreate); err != nil {
		t.Fatalf("WriteFileNoFollow: %v", err)
	}

	var after syscall.Stat_t
	if err := syscall.Stat(path, &after); err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if before.Ino != after.Ino {
		t.Errorf("inode changed: before %d, after %d (TruncateInPlaceOrCreate must not create+rename)", before.Ino, after.Ino)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "new" {
		t.Errorf("content = %q, want %q", data, "new")
	}
	if after.Mode&0o7777 != 0o644 {
		t.Errorf("mode = %#o, want %#o", after.Mode&0o7777, 0o644)
	}
}

// TestWriteFileNoFollow_TruncateInPlaceOrCreate_CreatesWhenAbsent proves an
// absent leaf is still created (via the ordinary create-then-rename path)
// when there is nothing to write in place yet.
func TestWriteFileNoFollow_TruncateInPlaceOrCreate_CreatesWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "leaf")

	if err := WriteFileNoFollow(path, []byte("new"), 0o644, 0, 0, TruncateInPlaceOrCreate); err != nil {
		t.Fatalf("WriteFileNoFollow: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "new" {
		t.Errorf("content = %q, want %q", data, "new")
	}
}

// TestWriteFileNoFollow_TruncateInPlaceOrCreate_RefusesSymlink proves a
// symlinked leaf is refused exactly like RefuseSymlink refuses one — never
// written or truncated through — leaving the symlink and its target intact.
func TestWriteFileNoFollow_TruncateInPlaceOrCreate_RefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("do-not-touch"), 0o600); err != nil {
		t.Fatalf("write victim: %v", err)
	}
	path := filepath.Join(dir, "leaf")
	if err := os.Symlink(victim, path); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if err := WriteFileNoFollow(path, []byte("new"), 0o644, 0, 0, TruncateInPlaceOrCreate); err == nil {
		t.Fatal("expected a symlinked leaf to be refused")
	}

	victimData, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("read victim: %v", err)
	}
	if string(victimData) != "do-not-touch" {
		t.Fatalf("victim was modified through the symlink: %q", victimData)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("leaf is no longer a symlink after a refused write")
	}
}

// TestWriteFileNoFollow_TruncateInPlaceOrCreate_RefusesHardlinkedLeaf proves
// a leaf that is a regular file but has more than one hard link — a
// workload can plant this by hard-linking to an unrelated (possibly
// root-owned) file it does not itself own — is refused before anything is
// truncated or written, exactly like a symlinked leaf: the victim inode's
// content, owner, and mode must all be unchanged, and the error must name
// the link count.
func TestWriteFileNoFollow_TruncateInPlaceOrCreate_RefusesHardlinkedLeaf(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("do-not-touch"), 0o600); err != nil {
		t.Fatalf("write victim: %v", err)
	}
	wantInfo, err := os.Stat(victim)
	if err != nil {
		t.Fatalf("stat victim: %v", err)
	}
	path := filepath.Join(dir, "leaf")
	if err := os.Link(victim, path); err != nil {
		t.Fatalf("link: %v", err)
	}

	err = WriteFileNoFollow(path, []byte("new"), 0o644, 0, 0, TruncateInPlaceOrCreate)
	if err == nil {
		t.Fatal("expected a hardlinked leaf to be refused")
	}
	if !strings.Contains(err.Error(), "link count") {
		t.Errorf("error = %v, want it to name the link count", err)
	}

	victimData, rerr := os.ReadFile(victim)
	if rerr != nil {
		t.Fatalf("read victim: %v", rerr)
	}
	if string(victimData) != "do-not-touch" {
		t.Fatalf("victim was modified through the hardlink: %q", victimData)
	}
	gotInfo, serr := os.Stat(victim)
	if serr != nil {
		t.Fatalf("stat victim: %v", serr)
	}
	if gotInfo.Mode() != wantInfo.Mode() {
		t.Errorf("victim mode changed: got %v, want %v", gotInfo.Mode(), wantInfo.Mode())
	}
	if gotInfo.Sys().(*syscall.Stat_t).Uid != wantInfo.Sys().(*syscall.Stat_t).Uid {
		t.Error("victim owner changed")
	}
}

// ---------------- ReadUnderRootNoFollow ----------------

func TestReadUnderRootNoFollow_NormalRead(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(sub, "secret.txt")
	if err := os.WriteFile(path, []byte("sk-test"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	data, err := ReadUnderRootNoFollow(root, path, 1024)
	if err != nil {
		t.Fatalf("ReadUnderRootNoFollow: %v", err)
	}
	if string(data) != "sk-test" {
		t.Errorf("content = %q", data)
	}
}

// TestReadUnderRootNoFollow_RefusesSymlinkInRootPointingOutside fails if
// containment is checked only by comparing path strings: the symlink's own
// name sits inside root, but its target does not, and the fd-anchored walk
// must refuse it rather than follow it out.
func TestReadUnderRootNoFollow_RefusesSymlinkInRootPointingOutside(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("leaked"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	link := filepath.Join(root, "link")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := ReadUnderRootNoFollow(root, link, 1024)
	if err == nil {
		t.Fatal("expected an error reading a symlink whose name is inside root but whose target is not, got nil")
	}
}

// TestReadUnderRootNoFollow_RefusesParentEscape fails if the containment
// check is dropped or bypassed: a path that textually walks back out of
// root via ".." must be refused before any filesystem call, not resolved.
func TestReadUnderRootNoFollow_RefusesParentEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("leaked"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	escaping := filepath.Join(root, "..", filepath.Base(outside), "secret")
	_, err := ReadUnderRootNoFollow(root, escaping, 1024)
	if !errors.Is(err, ErrPathEscapesRoot) {
		t.Fatalf("expected ErrPathEscapesRoot, got %v", err)
	}
}

func TestReadUnderRootNoFollow_BoundsOversizedContent(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "big")
	if err := os.WriteFile(path, make([]byte, 101), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := ReadUnderRootNoFollow(root, path, 100)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("expected ErrTooLarge, got %v", err)
	}
}

// TestReadUnderRootNoFollow_AllowsIntermediateSymlinkInsideRoot proves an
// intermediate directory that is a symlink is now FOLLOWED, not refused,
// when its target is itself inside root: the fd-anchored walk resolves
// "sub" through curFd's own readlink and continues into "attacker" (also
// directly under root) using the same fd-anchored openat chain, never a
// path re-lookup. This is the behaviour a Kubernetes projected-secret
// volume's "..data" symlink needs (see
// TestReadUnderRootNoFollow_AllowsProjectedSecretStyleSymlinkChain below for
// the real shape); a blanket refusal of every symlink, regardless of where
// it points, broke that unconditionally.
func TestReadUnderRootNoFollow_AllowsIntermediateSymlinkInsideRoot(t *testing.T) {
	root := t.TempDir()
	attacker := filepath.Join(root, "attacker")
	if err := os.Mkdir(attacker, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	victim := filepath.Join(attacker, "victim")
	if err := os.WriteFile(victim, []byte("do-not-touch"), 0o600); err != nil {
		t.Fatalf("write victim: %v", err)
	}
	sub := filepath.Join(root, "sub")
	if err := os.Symlink("attacker", sub); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	data, err := ReadUnderRootNoFollow(root, filepath.Join(sub, "victim"), 1024)
	if err != nil {
		t.Fatalf("expected a symlinked intermediate directory whose target is also inside root to be followed, got %v", err)
	}
	if string(data) != "do-not-touch" {
		t.Errorf("content = %q, want %q", data, "do-not-touch")
	}
}

// TestReadUnderRootNoFollow_AllowsProjectedSecretStyleSymlinkChain
// reproduces a real Kubernetes projected-secret (and ConfigMap) volume
// layout: kubelet lays out a timestamped directory holding the actual
// files, a "..data" symlink to it, and each key as a symlink through
// "..data" (e.g. "token" -> "..data/token"). This resolver must allow that
// layout: a blanket symlink refusal would make every from_file read of a
// projected-secret key fail. This is the acceptance-gate test: it must
// pass.
func TestReadUnderRootNoFollow_AllowsProjectedSecretStyleSymlinkChain(t *testing.T) {
	root := t.TempDir()
	timestamped := "..2026_09_28_12_00_00.123456789"
	if err := os.Mkdir(filepath.Join(root, timestamped), 0o700); err != nil {
		t.Fatalf("mkdir timestamped dir: %v", err)
	}
	tokenPath := filepath.Join(root, timestamped, "token")
	if err := os.WriteFile(tokenPath, []byte("sa-token-value\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	if err := os.Symlink(timestamped, filepath.Join(root, "..data")); err != nil {
		t.Fatalf("symlink ..data: %v", err)
	}
	if err := os.Symlink(filepath.Join("..data", "token"), filepath.Join(root, "token")); err != nil {
		t.Fatalf("symlink token: %v", err)
	}

	data, err := ReadUnderRootNoFollow(root, filepath.Join(root, "token"), 1024)
	if err != nil {
		t.Fatalf("expected a projected-secret-style symlink chain to resolve, got %v", err)
	}
	if string(data) != "sa-token-value\n" {
		t.Errorf("content = %q, want %q", data, "sa-token-value\n")
	}
}

// TestReadUnderRootNoFollow_RefusesSymlinkTargetEscapingViaDotDot fails if a
// relative symlink target containing ".." is followed: this walk has no
// fd-anchored way to prove such a target stays under root (unlike a plain
// sibling name), so it must be refused rather than guessed at.
func TestReadUnderRootNoFollow_RefusesSymlinkTargetEscapingViaDotDot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("leaked"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	link := filepath.Join(root, "link")
	target := filepath.Join("..", filepath.Base(outside), "secret")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := ReadUnderRootNoFollow(root, link, 1024)
	if !errors.Is(err, ErrPathEscapesRoot) {
		t.Fatalf("expected ErrPathEscapesRoot for a relative symlink target containing '..', got %v", err)
	}
}

// TestReadUnderRootNoFollow_RefusesAbsoluteSymlinkTarget fails if an
// absolute symlink target is ever followed, even one that would happen to
// resolve back inside root: proving that requires trusting the target's own
// text, which the fd-anchored walk deliberately never does.
func TestReadUnderRootNoFollow_RefusesAbsoluteSymlinkTarget(t *testing.T) {
	root := t.TempDir()
	inRoot := filepath.Join(root, "inside")
	if err := os.WriteFile(inRoot, []byte("data"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(inRoot, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := ReadUnderRootNoFollow(root, link, 1024)
	if !errors.Is(err, ErrPathEscapesRoot) {
		t.Fatalf("expected ErrPathEscapesRoot for an absolute symlink target, even one numerically inside root, got %v", err)
	}
}

// TestReadUnderRootNoFollow_RefusesEscapingSymlinkAtIntermediateComponent
// covers the same escape shapes as
// TestReadUnderRootNoFollow_RefusesSymlinkTargetEscapingViaDotDot and
// TestReadUnderRootNoFollow_RefusesAbsoluteSymlinkTarget, but with the
// symlink at an INTERMEDIATE path component instead of the leaf: this walk
// resolves a symlink through a different code path depending on whether it
// is the last remaining component (ReadAtNoFollow's own O_NOFOLLOW open) or
// an earlier one (the O_DIRECTORY|O_NOFOLLOW open, which fails ENOTDIR for
// a symlink rather than ELOOP — see isSymlinkAt's doc comment), so a test
// that only ever puts the escaping symlink at the leaf never exercises the
// intermediate branch's own escape checks at all.
func TestReadUnderRootNoFollow_RefusesEscapingSymlinkAtIntermediateComponent(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("leaked"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	tests := []struct {
		name    string
		readRel string // path passed to ReadUnderRootNoFollow, relative to root
		setup   func(t *testing.T, root string)
	}{
		{
			name:    "absolute target",
			readRel: filepath.Join("abs", "secret"),
			setup: func(t *testing.T, root string) {
				if err := os.Symlink(outside, filepath.Join(root, "abs")); err != nil {
					t.Fatalf("symlink abs: %v", err)
				}
			},
		},
		{
			name:    "relative target with one ..",
			readRel: filepath.Join("rel", "secret"),
			setup: func(t *testing.T, root string) {
				target := filepath.Join("..", filepath.Base(outside))
				if err := os.Symlink(target, filepath.Join(root, "rel")); err != nil {
					t.Fatalf("symlink rel: %v", err)
				}
			},
		},
		{
			name:    "relative target with .. nested under a real directory",
			readRel: filepath.Join("d", "up", "secret"),
			setup: func(t *testing.T, root string) {
				if err := os.Mkdir(filepath.Join(root, "d"), 0o700); err != nil {
					t.Fatalf("mkdir d: %v", err)
				}
				target := filepath.Join("..", "..", filepath.Base(outside))
				if err := os.Symlink(target, filepath.Join(root, "d", "up")); err != nil {
					t.Fatalf("symlink d/up: %v", err)
				}
			},
		},
		{
			name:    "chained: safe-looking splice resolves into an escaping symlink",
			readRel: filepath.Join("chain", "secret"),
			setup: func(t *testing.T, root string) {
				if err := os.Mkdir(filepath.Join(root, "d"), 0o700); err != nil {
					t.Fatalf("mkdir d: %v", err)
				}
				target := filepath.Join("..", "..", filepath.Base(outside))
				if err := os.Symlink(target, filepath.Join(root, "d", "up")); err != nil {
					t.Fatalf("symlink d/up: %v", err)
				}
				// "chain" -> "d/up" does not itself contain ".." or an
				// absolute component, so it splices cleanly; the escape
				// only surfaces one hop later, when "up" (now itself an
				// intermediate component) is resolved in turn.
				if err := os.Symlink(filepath.Join("d", "up"), filepath.Join(root, "chain")); err != nil {
					t.Fatalf("symlink chain: %v", err)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			tc.setup(t, root)

			_, err := ReadUnderRootNoFollow(root, filepath.Join(root, tc.readRel), 1024)
			if !errors.Is(err, ErrPathEscapesRoot) {
				t.Fatalf("expected ErrPathEscapesRoot, got %v", err)
			}
		})
	}
}

// TestReadUnderRootNoFollow_RefusesSymlinkLoop fails if the walk spins
// forever (or panics) on a symlink loop planted by whatever owns the
// containing directory.
func TestReadUnderRootNoFollow_RefusesSymlinkLoop(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink("b", filepath.Join(root, "a")); err != nil {
		t.Fatalf("symlink a: %v", err)
	}
	if err := os.Symlink("a", filepath.Join(root, "b")); err != nil {
		t.Fatalf("symlink b: %v", err)
	}

	_, err := ReadUnderRootNoFollow(root, filepath.Join(root, "a"), 1024)
	if !errors.Is(err, ErrTooManySymlinksUnderRoot) {
		t.Fatalf("expected ErrTooManySymlinksUnderRoot for a symlink loop, got %v", err)
	}
}

// TestReadUnderRootNoFollow_FailedSecondIntermediateDoesNotCloseItsFdNumberTwice
// proves that when the walk has already opened one intermediate directory
// (root/a) and the NEXT component (root/a/b) fails to open, the fd number
// this package closed for "a" is not closed a SECOND time by the function's
// own deferred cleanup. This matters because this walk can run in root's
// own multi-threaded PID-1 process: between the first (correct) close and a
// hypothetical second one, the kernel is free to have already handed that
// exact number to a completely unrelated fd opened by another goroutine, so
// a second close would silently close somebody else's file instead of
// erroring.
//
// Detecting this requires an OBSERVABLE, not just a clean run under the
// race detector (which does not track OS file descriptor lifetimes at all):
// the moment this package closes "a"'s fd, the test's hook immediately
// dup2's a sentinel pipe onto that exact number, deterministically
// re-claiming it regardless of what the kernel's normal allocator would
// otherwise have done with it. If the walk's cleanup closes that number a
// second time, the dup'd sentinel is what gets closed, and a subsequent
// fcntl(F_GETFD) on that number reports EBADF instead of succeeding. This
// test fails if the "owns the fd" tracking is not reset the instant the fd
// is closed in the loop.
func TestReadUnderRootNoFollow_FailedSecondIntermediateDoesNotCloseItsFdNumberTwice(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "a"), 0o700); err != nil {
		t.Fatalf("mkdir a: %v", err)
	}
	// "b" is deliberately absent under "a": the walk opens "a" successfully
	// (first intermediate, depth 1) and then fails to open "b" (second
	// intermediate, depth 2) — the two-or-more-intermediate shape the bug
	// requires, since the very first component is never "owned" yet when
	// it's opened.

	sentinelRead, sentinelWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer func() { _ = sentinelRead.Close() }()
	defer func() { _ = sentinelWrite.Close() }()

	claimedFd := -1
	readUnderRootIntermediateCloseTestHook = func(closedFd int) {
		claimedFd = closedFd
		if derr := syscall.Dup2(int(sentinelRead.Fd()), closedFd); derr != nil {
			t.Fatalf("dup2 sentinel onto claimed fd %d: %v", closedFd, derr)
		}
	}
	defer func() { readUnderRootIntermediateCloseTestHook = nil }()

	_, err = ReadUnderRootNoFollow(root, filepath.Join(root, "a", "b", "f"), 1024)
	if err == nil {
		t.Fatal("expected an error walking through a missing second intermediate directory, got nil")
	}
	if claimedFd < 0 {
		t.Fatal("test hook never fired — this run never reached a second intermediate component, so it proves nothing")
	}

	// If the walk's own cleanup double-closed claimedFd, the sentinel dup
	// planted at that exact number would already be gone.
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(claimedFd), uintptr(syscall.F_GETFD), 0); errno != 0 {
		t.Fatalf("fd %d was closed a second time after this package already closed it once (want the sentinel dup to remain live): %v", claimedFd, errno)
	}
}

func TestRelUnderRoot_RejectsRootItself(t *testing.T) {
	root := t.TempDir()
	if _, err := relUnderRoot(root, root); !errors.Is(err, ErrPathEscapesRoot) {
		t.Fatalf("expected ErrPathEscapesRoot for root itself, got %v", err)
	}
}

func TestRelUnderRoot_AcceptsNestedPath(t *testing.T) {
	root := t.TempDir()
	rel, err := relUnderRoot(root, filepath.Join(root, "a", "b"))
	if err != nil {
		t.Fatalf("relUnderRoot: %v", err)
	}
	want := filepath.Join("a", "b")
	if rel != want {
		t.Fatalf("rel = %q, want %q", rel, want)
	}
}

func TestRelUnderRoot_RejectsSiblingThatSharesPrefix(t *testing.T) {
	root := t.TempDir()
	sibling := root + "-sibling"
	if err := os.MkdirAll(sibling, 0o700); err != nil {
		t.Fatalf("mkdir sibling: %v", err)
	}
	if !strings.HasPrefix(sibling, root) {
		t.Fatalf("test setup: expected %q to share a string prefix with %q", sibling, root)
	}
	if _, err := relUnderRoot(root, filepath.Join(sibling, "file")); !errors.Is(err, ErrPathEscapesRoot) {
		t.Fatalf("expected ErrPathEscapesRoot for a sibling directory sharing root's string prefix, got %v", err)
	}
}
