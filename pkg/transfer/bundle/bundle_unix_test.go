// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build unix

package bundle

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestT1_UnixModesAndSymlinks verifies owner-only modes (0600/0644/0755),
// setuid/setgid/sticky stripping, read-only directories, and internal relative
// symlinks survive round-trip intact.
func TestT1_UnixModesAndSymlinks(t *testing.T) {
	ctx := context.Background()
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	scratch := t.TempDir()

	files := map[string]os.FileMode{
		".ssh/id_ed25519": 0o600,
		"notes.txt":       0o644,
		"bin/run.sh":      0o755,
	}
	for rel, perm := range files {
		full := filepath.Join(srcDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("content:"+rel), perm); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(full, perm); err != nil {
			t.Fatal(err)
		}
	}

	// File with sticky bit set: sticky bit must be stripped on round-trip.
	stickyPath := filepath.Join(srcDir, "sticky.sh")
	if err := os.WriteFile(stickyPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stickyPath, 0o755|os.ModeSticky); err != nil {
		t.Fatal(err)
	}

	// Read-only directory containing a file (exercises deferred dir chmod).
	roDir := filepath.Join(srcDir, "rodir")
	if err := os.Mkdir(roDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roDir, "inside.txt"), []byte("ok"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(roDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o755) })

	// Internal relative symlinks: sibling and parent-staying-inside-root.
	if err := os.Symlink("notes.txt", filepath.Join(srcDir, "notes-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../notes.txt", filepath.Join(srcDir, "bin", "up-link")); err != nil {
		t.Fatal(err)
	}

	srcRoot, err := os.OpenRoot(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srcRoot.Close() }()
	dstRoot, err := os.OpenRoot(dstDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dstRoot.Close() }()

	store := newMemStore()
	lim := DefaultWorkspaceLimits()
	rm, err := Write(ctx, srcRoot, scratch, nil, lim, store)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := Extract(ctx, dstRoot, scratch, rm, lim, store); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dstDir, "rodir"), 0o755) })

	for rel, wantPerm := range files {
		fi, err := os.Lstat(filepath.Join(dstDir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("lstat %s: %v", rel, err)
		}
		if got := fi.Mode().Perm(); got != wantPerm {
			t.Errorf("%s mode = %04o, want %04o", rel, got, wantPerm)
		}
	}

	fiSticky, err := os.Lstat(filepath.Join(dstDir, "sticky.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if fiSticky.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		t.Errorf("special mode bits not stripped: %v", fiSticky.Mode())
	}
	if fiSticky.Mode().Perm() != 0o755 {
		t.Errorf("sticky.sh perm = %04o, want 0755", fiSticky.Mode().Perm())
	}

	fiRO, err := os.Lstat(filepath.Join(dstDir, "rodir"))
	if err != nil {
		t.Fatal(err)
	}
	if fiRO.Mode().Perm() != 0o555 {
		t.Errorf("rodir perm = %04o, want 0555", fiRO.Mode().Perm())
	}

	for link, wantTarget := range map[string]string{
		"notes-link":  "notes.txt",
		"bin/up-link": "../notes.txt",
	} {
		got, err := os.Readlink(filepath.Join(dstDir, filepath.FromSlash(link)))
		if err != nil || got != wantTarget {
			t.Errorf("readlink %s = %q (err=%v), want %q", link, got, err, wantTarget)
		}
	}
}

// TestT2_UnixSkipCounts verifies writer skips and counts sockets, FIFOs (without
// blocking), and absolute/escaping symlinks.
func TestT2_UnixSkipCounts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	srcDir := t.TempDir()
	scratch := t.TempDir()

	if err := os.WriteFile(filepath.Join(srcDir, "regular.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	// FIFO (must not block!)
	if err := syscall.Mkfifo(filepath.Join(srcDir, "my.fifo"), 0o644); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	// Unix domain socket (use short path under /tmp if needed; t.TempDir is short on Linux).
	sockPath := filepath.Join(srcDir, "my.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix socket: %v", err)
	}
	defer func() { _ = ln.Close() }()

	// Escaping symlinks (absolute + relative escaping root + multi-hop chain)
	if err := os.Symlink("/scion-staging/secret", filepath.Join(srcDir, "abs-secret")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(srcDir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../outside", filepath.Join(srcDir, "sub", "rel-escape")); err != nil {
		t.Fatal(err)
	}
	// Symlink chain: a/b -> .. (stays at root, kept), c -> a/b/../x (physically ../x, skipped!)
	if err := os.MkdirAll(filepath.Join(srcDir, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..", filepath.Join(srcDir, "a", "b")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a/b/../x", filepath.Join(srcDir, "c")); err != nil {
		t.Fatal(err)
	}

	srcRoot, err := os.OpenRoot(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srcRoot.Close() }()

	lim := DefaultWorkspaceLimits()
	est, err := Estimate(ctx, srcRoot, nil, lim)
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}

	store := newMemStore()
	rm, err := Write(ctx, srcRoot, scratch, nil, lim, store)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if est.Entries != rm.Entries || est.Bytes != rm.Bytes || est.Skipped != rm.Skipped {
		t.Fatalf("Estimate %+v != Write %+v", est, rm)
	}
	if rm.Skipped.FIFOs != 1 {
		t.Errorf("Skipped.FIFOs = %d, want 1", rm.Skipped.FIFOs)
	}
	if rm.Skipped.Sockets != 1 {
		t.Errorf("Skipped.Sockets = %d, want 1", rm.Skipped.Sockets)
	}
	if rm.Skipped.EscapingSymlinks != 3 {
		t.Errorf("Skipped.EscapingSymlinks = %d, want 3", rm.Skipped.EscapingSymlinks)
	}
}

// TestT3_UnixSymlinkedParentRefused verifies that an archive containing an
// internal symlink followed by a child write through that symlink is refused.
func TestT3_UnixSymlinkedParentRefused(t *testing.T) {
	ctx := context.Background()
	rm, store := buildRawTarGz(t, func(tw *tar.Writer) {
		_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "realdir/", Mode: 0o755})
		_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: "linkdir", Linkname: "realdir", Mode: 0o777})
		_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "linkdir/evil.txt", Size: 4, Mode: 0o644})
		_, _ = tw.Write([]byte("evil"))
	})

	dstDir := t.TempDir()
	dstRoot, err := os.OpenRoot(dstDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dstRoot.Close() }()

	scratch := t.TempDir()
	err = Extract(ctx, dstRoot, scratch, rm, DefaultWorkspaceLimits(), store)
	if !errors.Is(err, ErrSymlinkParent) {
		t.Fatalf("got err = %v, want ErrSymlinkParent", err)
	}
	if _, statErr := os.Stat(filepath.Join(dstDir, "realdir", "evil.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("evil.txt was written through symlink parent!")
	}
	assertDirEmpty(t, scratch)
}

// TestT3_UnixSymlinkChainRefused verifies that multi-hop symlink chains
// escaping the root are refused regardless of entry order in the archive.
func TestT3_UnixSymlinkChainRefused(t *testing.T) {
	ctx := context.Background()
	lim := DefaultWorkspaceLimits()

	cases := []struct {
		name  string
		write func(tw *tar.Writer)
	}{
		{
			name: "target symlink before escaping chain link",
			write: func(tw *tar.Writer) {
				_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "a/", Mode: 0o755})
				_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: "a/b", Linkname: "..", Mode: 0o777})
				_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: "c", Linkname: "a/b/../x", Mode: 0o777})
			},
		},
		{
			name: "escaping chain link before target symlink",
			write: func(tw *tar.Writer) {
				_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: "c", Linkname: "a/b/../x", Mode: 0o777})
				_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "a/", Mode: 0o755})
				_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: "a/b", Linkname: "..", Mode: 0o777})
			},
		},
		{
			name: "symlink loop refused",
			write: func(tw *tar.Writer) {
				_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: "x", Linkname: "y", Mode: 0o777})
				_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: "y", Linkname: "x", Mode: 0o777})
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rm, store := buildRawTarGz(t, tc.write)
			dstRoot, err := os.OpenRoot(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = dstRoot.Close() }()
			scratch := t.TempDir()
			err = Extract(ctx, dstRoot, scratch, rm, lim, store)
			if !errors.Is(err, ErrEscapingSymlink) {
				t.Fatalf("got err = %v, want ErrEscapingSymlink", err)
			}
			assertDirEmpty(t, scratch)
		})
	}
}

// TestT2_UnixHomeExclusionsThroughSymlinkedAncestor verifies that excluded
// secrets are never bundled under their physical path when an ancestor
// directory is an in-root symlink (covering D6 lists, caller literals, and
// .scion/harness).
func TestT2_UnixHomeExclusionsThroughSymlinkedAncestor(t *testing.T) {
	ctx := context.Background()
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	scratch := t.TempDir()

	// 1. D6 credential under symlinked .config/gcloud -> ../dotfiles/gcloud
	if err := os.MkdirAll(filepath.Join(srcDir, "dotfiles", "gcloud", "configurations"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "dotfiles", "gcloud", "credentials.db"), []byte("GCLOUD_SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "dotfiles", "gcloud", "configurations", "config_default"), []byte("keep-gcloud-cfg"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(srcDir, ".config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../dotfiles/gcloud", filepath.Join(srcDir, ".config", "gcloud")); err != nil {
		t.Fatal(err)
	}

	// 2. Caller literal secret under symlinked .config/my-app -> ../real-secrets
	if err := os.MkdirAll(filepath.Join(srcDir, "real-secrets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "real-secrets", "token.json"), []byte("CALLER_SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../real-secrets", filepath.Join(srcDir, ".config", "my-app")); err != nil {
		t.Fatal(err)
	}

	// 3. .scion/harness/secrets under symlinked .scion/harness -> ../real-harness
	if err := os.MkdirAll(filepath.Join(srcDir, "real-harness", "secrets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "real-harness", "secrets", "API_KEY"), []byte("HARNESS_SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(srcDir, ".scion"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../real-harness", filepath.Join(srcDir, ".scion", "harness")); err != nil {
		t.Fatal(err)
	}

	srcRoot, err := os.OpenRoot(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srcRoot.Close() }()
	dstRoot, err := os.OpenRoot(dstDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dstRoot.Close() }()

	ex := HomeExcluder(HomeExcluderOptions{
		ExtraLiterals: []string{
			".config/my-app/token.json",
			".scion/harness/secrets",
		},
	})
	lim := DefaultHomeLimits()

	est, err := Estimate(ctx, srcRoot, ex, lim)
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}

	store := newMemStore()
	rm, err := Write(ctx, srcRoot, scratch, ex, lim, store)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if est.Entries != rm.Entries || est.Bytes != rm.Bytes || est.Skipped != rm.Skipped {
		t.Fatalf("Estimate %+v != Write %+v", est, rm)
	}

	if err := Extract(ctx, dstRoot, scratch, rm, lim, store); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	// None of the excluded secrets may exist at either their logical or physical paths.
	for _, secretPath := range []string{
		".config/gcloud/credentials.db",
		"dotfiles/gcloud/credentials.db",
		".config/my-app/token.json",
		"real-secrets/token.json",
		".scion/harness/secrets/API_KEY",
		"real-harness/secrets/API_KEY",
	} {
		if _, err := os.Stat(filepath.Join(dstDir, filepath.FromSlash(secretPath))); !os.IsNotExist(err) {
			t.Errorf("secret %q leaked to target (stat err=%v)", secretPath, err)
		}
	}

	// Non-excluded sibling file inside the symlinked dir is preserved.
	if got, err := os.ReadFile(filepath.Join(dstDir, ".config", "gcloud", "configurations", "config_default")); err != nil || string(got) != "keep-gcloud-cfg" {
		t.Errorf("non-excluded gcloud config = %q, err=%v", got, err)
	}
}

// TestT4_UnixWriterMaxPartsEnforced verifies Write aborts with
// ErrLimitExceeded(parts) when compressed tar metadata (many symlinks with
// long incompressible targets) exceeds lim.MaxParts(), with no Put beyond the limit.
func TestT4_UnixWriterMaxPartsEnforced(t *testing.T) {
	ctx := context.Background()
	srcDir := t.TempDir()
	scratch := t.TempDir()

	// Create 500 symlinks with 200-char incompressible relative targets.
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	var x uint32 = 0x9e3779b9
	for i := range 500 {
		var sb strings.Builder
		sb.Grow(200)
		for range 200 {
			x = x*1664525 + 1013904223
			sb.WriteByte(alphabet[int(x>>24)%len(alphabet)])
		}
		if err := os.Symlink(sb.String(), filepath.Join(srcDir, fmt.Sprintf("l%04d", i))); err != nil {
			t.Fatal(err)
		}
	}

	srcRoot, err := os.OpenRoot(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srcRoot.Close() }()

	lim := Limits{
		MaxBytes:   1024,
		MaxEntries: 1000,
		MaxDepth:   16,
		MaxPathLen: 256,
		PartSize:   1024,
	}
	store := newMemStore()

	_, err = Write(ctx, srcRoot, scratch, nil, lim, store)
	var le *ErrLimitExceeded
	if !errors.As(err, &le) || le.Field != "parts" {
		t.Fatalf("got err = %v, want ErrLimitExceeded(parts)", err)
	}
	if got := store.putCalls.Load(); got > int64(lim.MaxParts()) {
		t.Fatalf("sink.Put called %d times, exceeds MaxParts()=%d", got, lim.MaxParts())
	}
	assertDirEmpty(t, scratch)
}

// TestT2_UnixHomeExclusions_AbsoluteInHomeAncestor reproduces Round 3 R1:
// .config -> /home/scion/dotfiles/config (an absolute symlink pointing back
// into AgentHomeAbs) must rebase and exclude dotfiles/config/gcloud/credentials.db.
func TestT2_UnixHomeExclusions_AbsoluteInHomeAncestor(t *testing.T) {
	ctx := context.Background()
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	scratch := t.TempDir()

	if err := os.MkdirAll(filepath.Join(srcDir, "dotfiles", "config", "gcloud", "configurations"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "dotfiles", "config", "gcloud", "credentials.db"), []byte("ABS_SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "dotfiles", "config", "gcloud", "configurations", "config_default"), []byte("keep-cfg"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/home/scion/dotfiles/config", filepath.Join(srcDir, ".config")); err != nil {
		t.Fatal(err)
	}

	srcRoot, _ := os.OpenRoot(srcDir)
	defer func() { _ = srcRoot.Close() }()
	dstRoot, _ := os.OpenRoot(dstDir)
	defer func() { _ = dstRoot.Close() }()

	ex := HomeExcluder(HomeExcluderOptions{AgentHomeAbs: "/home/scion"})
	lim := DefaultHomeLimits()

	est, err := Estimate(ctx, srcRoot, ex, lim)
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	store := newMemStore()
	rm, err := Write(ctx, srcRoot, scratch, ex, lim, store)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if est.Entries != rm.Entries || est.Bytes != rm.Bytes || est.Skipped != rm.Skipped {
		t.Fatalf("Estimate %+v != Write %+v", est, rm)
	}
	if err := Extract(ctx, dstRoot, scratch, rm, lim, store); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dstDir, "dotfiles", "config", "gcloud", "credentials.db")); !os.IsNotExist(err) {
		t.Fatalf("secret leaked via absolute in-home ancestor symlink (stat err=%v)", err)
	}
	if got, err := os.ReadFile(filepath.Join(dstDir, "dotfiles", "config", "gcloud", "configurations", "config_default")); err != nil || string(got) != "keep-cfg" {
		t.Fatalf("non-excluded file = %q, err=%v", got, err)
	}
}

// TestT2_UnixHomeExclusions_FailClosedUnresolvableAncestor verifies that
// unresolvable ancestor symlinks on any exclusion entry fail closed with
// *ErrUnportableHome from both Write and Estimate, while an outside-home leaf
// symlink (e.g. K8s /scion-staging/...) stays excluded without failing.
func TestT2_UnixHomeExclusions_FailClosedUnresolvableAncestor(t *testing.T) {
	ctx := context.Background()
	lim := DefaultHomeLimits()

	cases := []struct {
		name         string
		linkRel      string
		linkTarget   string
		agentHomeAbs string
		wantPath     string
	}{
		{
			name:         "absolute symlink outside home",
			linkRel:      ".config",
			linkTarget:   "/etc/dotfiles",
			agentHomeAbs: "/home/scion",
			wantPath:     ".config",
		},
		{
			name:         "absolute symlink with empty AgentHomeAbs",
			linkRel:      ".config",
			linkTarget:   "/home/scion/dotfiles",
			agentHomeAbs: "",
			wantPath:     ".config",
		},
		{
			name:         "relative escaping ancestor symlink",
			linkRel:      ".config",
			linkTarget:   "../../outside",
			agentHomeAbs: "/home/scion",
			wantPath:     ".config",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srcDir := t.TempDir()
			if err := os.Symlink(tc.linkTarget, filepath.Join(srcDir, tc.linkRel)); err != nil {
				t.Fatal(err)
			}
			srcRoot, _ := os.OpenRoot(srcDir)
			defer func() { _ = srcRoot.Close() }()

			ex := HomeExcluder(HomeExcluderOptions{AgentHomeAbs: tc.agentHomeAbs})
			var uh *ErrUnportableHome

			if _, err := Estimate(ctx, srcRoot, ex, lim); !errors.As(err, &uh) || uh.Path != tc.wantPath {
				t.Errorf("Estimate err = %v, want ErrUnportableHome(Path=%q)", err, tc.wantPath)
			}
			store := newMemStore()
			if _, err := Write(ctx, srcRoot, t.TempDir(), ex, lim, store); !errors.As(err, &uh) || uh.Path != tc.wantPath {
				t.Errorf("Write err = %v, want ErrUnportableHome(Path=%q)", err, tc.wantPath)
			}
			if store.putCalls.Load() != 0 {
				t.Errorf("expected 0 Put calls on ErrUnportableHome, got %d", store.putCalls.Load())
			}
		})
	}

	t.Run("outside-home leaf secret symlink does not fail", func(t *testing.T) {
		srcDir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(srcDir, ".kube"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("/scion-staging/kubeconfig", filepath.Join(srcDir, ".kube", "config")); err != nil {
			t.Fatal(err)
		}
		srcRoot, _ := os.OpenRoot(srcDir)
		defer func() { _ = srcRoot.Close() }()

		ex := HomeExcluder(HomeExcluderOptions{AgentHomeAbs: "/home/scion"})
		if _, err := Estimate(ctx, srcRoot, ex, lim); err != nil {
			t.Fatalf("Estimate leaf secret symlink: %v", err)
		}
		if _, err := Write(ctx, srcRoot, t.TempDir(), ex, lim, newMemStore()); err != nil {
			t.Fatalf("Write leaf secret symlink: %v", err)
		}
	})
}

// TestT2_UnixExcludedHardlinkSkipped verifies hard links to excluded secret
// files (and files inside excluded credential directories) are skipped and
// counted as Excluded, while user files hard-linked into excluded caches (e.g.
// uv venvs) or .scion/harness/outputs survive the round trip.
func TestT2_UnixExcludedHardlinkSkipped(t *testing.T) {
	ctx := context.Background()
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	scratch := t.TempDir()

	if err := os.MkdirAll(filepath.Join(srcDir, ".kube"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, ".kube", "config"), []byte("KUBE_SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(srcDir, ".kube", "config"), filepath.Join(srcDir, "leaked-kubeconfig")); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(srcDir, ".scion", "harness", "secrets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, ".scion", "harness", "secrets", "KEY"), []byte("HARNESS_KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(srcDir, ".scion", "harness", "secrets", "KEY"), filepath.Join(srcDir, "leaked-harness-key")); err != nil {
		t.Fatal(err)
	}

	// Non-secret cache hardlink (uv venv pattern): cache is excluded lexically,
	// but the user file hard-linked into .cache MUST survive!
	if err := os.MkdirAll(filepath.Join(srcDir, ".cache", "uv", "archive"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, ".cache", "uv", "archive", "mod.py"), []byte("print('ok')"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(srcDir, ".local", "share", "uv", "tools", "ruff", "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(
		filepath.Join(srcDir, ".cache", "uv", "archive", "mod.py"),
		filepath.Join(srcDir, ".local", "share", "uv", "tools", "ruff", "lib", "mod.py"),
	); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(srcDir, "normal.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}

	srcRoot, _ := os.OpenRoot(srcDir)
	defer func() { _ = srcRoot.Close() }()
	dstRoot, _ := os.OpenRoot(dstDir)
	defer func() { _ = dstRoot.Close() }()

	ex := HomeExcluder(HomeExcluderOptions{AgentHomeAbs: "/home/scion"})
	lim := DefaultHomeLimits()

	est, err := Estimate(ctx, srcRoot, ex, lim)
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	store := newMemStore()
	rm, err := Write(ctx, srcRoot, scratch, ex, lim, store)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if est.Entries != rm.Entries || est.Bytes != rm.Bytes || est.Skipped != rm.Skipped {
		t.Fatalf("Estimate %+v != Write %+v", est, rm)
	}
	if err := Extract(ctx, dstRoot, scratch, rm, lim, store); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	for _, leaked := range []string{"leaked-kubeconfig", "leaked-harness-key", ".cache/uv/archive/mod.py"} {
		if _, err := os.Stat(filepath.Join(dstDir, filepath.FromSlash(leaked))); !os.IsNotExist(err) {
			t.Errorf("excluded path %q was NOT excluded (stat err=%v)", leaked, err)
		}
	}
	if got, err := os.ReadFile(filepath.Join(dstDir, ".local", "share", "uv", "tools", "ruff", "lib", "mod.py")); err != nil || string(got) != "print('ok')" {
		t.Errorf("cache-hardlinked uv tool file = %q, err=%v; want preserved", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(dstDir, "normal.txt")); err != nil || string(got) != "ok" {
		t.Errorf("normal.txt = %q, err=%v", got, err)
	}
}

// TestT2_UnixCredentialHardlinkBudgetFailClosed verifies that exhausting
// maxExcludedHardlinkScanEntries under a credential directory fails closed with
// *ErrUnportableHome from both Write and Estimate.
func TestT2_UnixCredentialHardlinkBudgetFailClosed(t *testing.T) {
	orig := maxExcludedHardlinkScanEntries
	maxExcludedHardlinkScanEntries = 8
	t.Cleanup(func() { maxExcludedHardlinkScanEntries = orig })

	ctx := context.Background()
	srcDir := t.TempDir()
	credDir := filepath.Join(srcDir, ".config", "gcloud", "legacy_credentials")
	if err := os.MkdirAll(credDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := range 10 {
		if err := os.WriteFile(filepath.Join(credDir, fmt.Sprintf("cred-%02d.json", i)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	srcRoot, _ := os.OpenRoot(srcDir)
	defer func() { _ = srcRoot.Close() }()

	ex := HomeExcluder(HomeExcluderOptions{AgentHomeAbs: "/home/scion"})
	lim := DefaultHomeLimits()
	var uh *ErrUnportableHome

	if _, err := Estimate(ctx, srcRoot, ex, lim); !errors.As(err, &uh) {
		t.Errorf("Estimate err = %v, want *ErrUnportableHome", err)
	}
	store := newMemStore()
	if _, err := Write(ctx, srcRoot, t.TempDir(), ex, lim, store); !errors.As(err, &uh) {
		t.Errorf("Write err = %v, want *ErrUnportableHome", err)
	}
	if store.putCalls.Load() != 0 {
		t.Errorf("expected 0 Put calls on hardlink budget exhaustion, got %d", store.putCalls.Load())
	}
}

// TestT2_UnixUnportablePathError verifies that a filename failing
// ValidateRelPath (such as one containing a backslash) returns *ErrUnportablePath
// from both Write and Estimate.
func TestT2_UnixUnportablePathError(t *testing.T) {
	ctx := context.Background()
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, `bad\name.txt`), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	srcRoot, _ := os.OpenRoot(srcDir)
	defer func() { _ = srcRoot.Close() }()

	lim := DefaultWorkspaceLimits()
	var up *ErrUnportablePath
	if _, err := Estimate(ctx, srcRoot, nil, lim); !errors.As(err, &up) || up.Path != `bad\name.txt` {
		t.Errorf("Estimate err = %v, want ErrUnportablePath(bad\\name.txt)", err)
	}
	if _, err := Write(ctx, srcRoot, t.TempDir(), nil, lim, newMemStore()); !errors.As(err, &up) || up.Path != `bad\name.txt` {
		t.Errorf("Write err = %v, want ErrUnportablePath(bad\\name.txt)", err)
	}
}

func TestWorkspaceExcluderWithOptions_SecretsSymlinksAndHardlinks(t *testing.T) {
	ctx := context.Background()
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	scratch := t.TempDir()

	// Normal workspace file + .scion identity dir (always excluded).
	if err := os.WriteFile(filepath.Join(srcDir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(srcDir, ".scion"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, ".scion", "marker"), []byte("id"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Direct file secret at /workspace/.env and hard-linked copy.
	if err := os.WriteFile(filepath.Join(srcDir, ".env"), []byte("SECRET_KEY=topsecret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(srcDir, ".env"), filepath.Join(srcDir, "env-hardlink.txt")); err != nil {
		t.Fatal(err)
	}
	// Secret reached through an absolute in-workspace symlinked ancestor:
	// config -> /workspace/real-cfg, secret at /workspace/config/sa.json.
	if err := os.MkdirAll(filepath.Join(srcDir, "real-cfg"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "real-cfg", "sa.json"), []byte("SA_SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "real-cfg", "app.yaml"), []byte("ok: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/workspace/real-cfg", filepath.Join(srcDir, "config")); err != nil {
		t.Fatal(err)
	}

	srcRoot, err := os.OpenRoot(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srcRoot.Close() }()
	dstRoot, err := os.OpenRoot(dstDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dstRoot.Close() }()

	ex := WorkspaceExcluderWithOptions(WorkspaceExcluderOptions{
		ExtraLiterals: []string{".env", "config/sa.json"},
		WorkspaceAbs:  "/workspace",
	})
	lim := DefaultWorkspaceLimits()

	est, err := Estimate(ctx, srcRoot, ex, lim)
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	store := newMemStore()
	rm, err := Write(ctx, srcRoot, scratch, ex, lim, store)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if est.Entries != rm.Entries || est.Bytes != rm.Bytes {
		t.Fatalf("Estimate %+v != Write %+v", est, rm)
	}
	if err := Extract(ctx, dstRoot, scratch, rm, lim, store); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	for _, leaked := range []string{".scion/marker", ".env", "env-hardlink.txt", "config/sa.json", "real-cfg/sa.json"} {
		if _, err := os.Stat(filepath.Join(dstDir, filepath.FromSlash(leaked))); !os.IsNotExist(err) {
			t.Errorf("excluded workspace path %q leaked to target (stat err=%v)", leaked, err)
		}
	}
	if got, err := os.ReadFile(filepath.Join(dstDir, "main.go")); err != nil || string(got) != "package main\n" {
		t.Errorf("main.go = %q, err=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(dstDir, "real-cfg", "app.yaml")); err != nil || string(got) != "ok: true\n" {
		t.Errorf("real-cfg/app.yaml = %q, err=%v", got, err)
	}

	// Escaping symlinked ancestor on an excluded workspace secret must fail closed.
	escDir := t.TempDir()
	if err := os.Symlink("/etc", filepath.Join(escDir, "config")); err != nil {
		t.Fatal(err)
	}
	escRoot, err := os.OpenRoot(escDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = escRoot.Close() }()
	_, err = Estimate(ctx, escRoot, ex, lim)
	var unportable *ErrUnportableHome
	if !errors.As(err, &unportable) {
		t.Fatalf("expected *ErrUnportableHome on escaping workspace secret ancestor, got %v", err)
	}
}
