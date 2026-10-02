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

package stagedsecrets

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
)

func TestWriteFileSecrets(t *testing.T) {
	homeDir := t.TempDir()
	targetDir := t.TempDir()
	staged := &Staged{
		FileSecrets: []FileSecret{
			{
				Name:   "TLS_CERT",
				Target: filepath.Join(targetDir, "ssl", "cert.pem"),
				Value:  base64.StdEncoding.EncodeToString([]byte("cert-content")),
			},
			{
				Name:   "SSH_KEY",
				Target: filepath.Join(targetDir, "ssh", "id_rsa"),
				Value:  base64.StdEncoding.EncodeToString([]byte("ssh-key")),
			},
		},
	}

	if err := Write(homeDir, staged); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	for path, want := range map[string]string{
		filepath.Join(targetDir, "ssl", "cert.pem"): "cert-content",
		filepath.Join(targetDir, "ssh", "id_rsa"):   "ssh-key",
	} {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if string(content) != want {
			t.Errorf("%s content = %q, want %q", path, content, want)
		}
	}

	info, err := os.Stat(filepath.Join(targetDir, "ssl", "cert.pem"))
	if err != nil {
		t.Fatalf("stat cert.pem: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("file mode = %o, want 600", info.Mode().Perm())
	}
}

func TestWriteVariableSecrets(t *testing.T) {
	homeDir := t.TempDir()
	staged := &Staged{VariableSecrets: map[string]string{
		"config": `{"a":"b"}`,
		"token":  "abc123",
	}}

	if err := Write(homeDir, staged); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	secretsPath := filepath.Join(homeDir, ".scion", "secrets.json")
	data, err := os.ReadFile(secretsPath)
	if err != nil {
		t.Fatalf("read secrets.json: %v", err)
	}
	var vars map[string]string
	if err := json.Unmarshal(data, &vars); err != nil {
		t.Fatalf("unmarshal secrets.json: %v", err)
	}
	if len(vars) != 2 || vars["config"] != `{"a":"b"}` || vars["token"] != "abc123" {
		t.Errorf("variable secrets = %#v", vars)
	}

	info, err := os.Stat(secretsPath)
	if err != nil {
		t.Fatalf("stat secrets.json: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("file mode = %o, want 600", info.Mode().Perm())
	}
}

func TestWriteNoVariables(t *testing.T) {
	homeDir := t.TempDir()
	if err := Write(homeDir, &Staged{}); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(homeDir, ".scion", "secrets.json")); !os.IsNotExist(err) {
		t.Error("secrets.json was created without variable secrets")
	}
}

// TestWrite_RefusesSymlinkAtVariableSecretsPath proves a symlink planted at
// <homeDir>/.scion/secrets.json (e.g. left over from a previous run on a
// persisted home, or planted ahead of a restart) is refused rather than
// written or chowned through: Write must return an error, and the symlink's
// target file must be untouched.
func TestWrite_RefusesSymlinkAtVariableSecretsPath(t *testing.T) {
	homeDir := t.TempDir()
	scionDir := filepath.Join(homeDir, ".scion")
	if err := os.MkdirAll(scionDir, 0700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(homeDir, "victim")
	if err := os.WriteFile(victim, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	secretsPath := filepath.Join(scionDir, "secrets.json")
	if err := os.Symlink(victim, secretsPath); err != nil {
		t.Fatal(err)
	}

	staged := &Staged{VariableSecrets: map[string]string{"token": "abc123"}}
	if err := Write(homeDir, staged); err == nil {
		t.Fatal("Write() = nil error, want a refusal for a symlinked secrets.json")
	}

	link, err := os.Readlink(secretsPath)
	if err != nil {
		t.Fatalf("secrets.json is no longer a symlink after the refused write: %v", err)
	}
	if link != victim {
		t.Errorf("secrets.json symlink target = %q, want %q (unchanged)", link, victim)
	}
	content, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("read victim: %v", err)
	}
	if string(content) != "untouched" {
		t.Errorf("victim content = %q, want %q (unchanged)", content, "untouched")
	}
}

// TestWrite_RefusesSymlinkAtFileSecretTarget proves the same for a file
// secret's own Target: a planted symlink there is refused rather than
// written or chowned through, and the symlink's target file is untouched.
func TestWrite_RefusesSymlinkAtFileSecretTarget(t *testing.T) {
	homeDir := t.TempDir()
	targetDir := t.TempDir()
	victim := filepath.Join(targetDir, "victim")
	if err := os.WriteFile(victim, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(targetDir, "ssl", "cert.pem")
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, target); err != nil {
		t.Fatal(err)
	}

	staged := &Staged{FileSecrets: []FileSecret{
		{Name: "TLS_CERT", Target: target, Value: base64.StdEncoding.EncodeToString([]byte("cert-content"))},
	}}
	if err := Write(homeDir, staged); err == nil {
		t.Fatal("Write() = nil error, want a refusal for a symlinked file-secret target")
	}

	link, err := os.Readlink(target)
	if err != nil {
		t.Fatalf("target is no longer a symlink after the refused write: %v", err)
	}
	if link != victim {
		t.Errorf("target symlink = %q, want %q (unchanged)", link, victim)
	}
	content, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("read victim: %v", err)
	}
	if string(content) != "untouched" {
		t.Errorf("victim content = %q, want %q (unchanged)", content, "untouched")
	}
}

// TestWriteAs_SymlinkedScionDirRefusedBeforeChown proves that when
// <homeDir>/.scion itself (not just secrets.json under it) is a symlink —
// e.g. left over from a previous run on a persisted home, or planted ahead
// of a restart — Write refuses before ever creating, writing, or chowning
// anything through it, and the symlink's target directory (including its
// own ownership and mode) is left exactly as it was.
func TestWriteAs_SymlinkedScionDirRefusedBeforeChown(t *testing.T) {
	homeDir := t.TempDir()
	victim := t.TempDir()
	if err := os.Chmod(victim, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(homeDir, ".scion")); err != nil {
		t.Fatal(err)
	}
	wantInfo, err := os.Lstat(victim)
	if err != nil {
		t.Fatal(err)
	}

	staged := &Staged{VariableSecrets: map[string]string{"token": "abc123"}}
	if err := writeAs(homeDir, staged, os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("writeAs() = nil error, want a refusal for a symlinked .scion directory")
	}

	gotInfo, err := os.Lstat(victim)
	if err != nil {
		t.Fatal(err)
	}
	if gotInfo.Mode() != wantInfo.Mode() {
		t.Errorf("victim mode changed: got %v, want %v", gotInfo.Mode(), wantInfo.Mode())
	}
	if _, err := os.Stat(filepath.Join(victim, "secrets.json")); !os.IsNotExist(err) {
		t.Errorf("secrets.json was written through the symlinked .scion directory")
	}
}

// TestWriteAs_SymlinkedFileSecretParentRefusedBeforeChown proves the same for
// a file secret's own parent directory: a symlink planted at the parent
// itself (not just the leaf Target) is refused before any create/chown, and
// the symlink's target directory is untouched.
func TestWriteAs_SymlinkedFileSecretParentRefusedBeforeChown(t *testing.T) {
	homeDir := t.TempDir()
	victim := t.TempDir()
	if err := os.Chmod(victim, 0750); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(homeDir, "secrets")
	if err := os.Symlink(victim, parent); err != nil {
		t.Fatal(err)
	}
	wantInfo, err := os.Lstat(victim)
	if err != nil {
		t.Fatal(err)
	}

	staged := &Staged{FileSecrets: []FileSecret{
		{Name: "TLS_CERT", Target: filepath.Join(parent, "cert.pem"), Value: base64.StdEncoding.EncodeToString([]byte("cert-content"))},
	}}
	if err := writeAs(homeDir, staged, os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("writeAs() = nil error, want a refusal for a symlinked file-secret parent directory")
	}

	gotInfo, err := os.Lstat(victim)
	if err != nil {
		t.Fatal(err)
	}
	if gotInfo.Mode() != wantInfo.Mode() {
		t.Errorf("victim mode changed: got %v, want %v", gotInfo.Mode(), wantInfo.Mode())
	}
	if _, err := os.Stat(filepath.Join(victim, "cert.pem")); !os.IsNotExist(err) {
		t.Errorf("cert.pem was written through the symlinked parent directory")
	}
}

// TestWriteAs_SymlinkedIntermediateComponentRefusedBeforeChown proves the
// same one level further up the chain: a symlink at an INTERMEDIATE
// component (not the immediate parent, and not the leaf) is refused before
// any create/chown happens to anything beneath it.
func TestWriteAs_SymlinkedIntermediateComponentRefusedBeforeChown(t *testing.T) {
	homeDir := t.TempDir()
	victim := t.TempDir()
	if err := os.Chmod(victim, 0750); err != nil {
		t.Fatal(err)
	}
	intermediate := filepath.Join(homeDir, "a")
	if err := os.Symlink(victim, intermediate); err != nil {
		t.Fatal(err)
	}
	wantInfo, err := os.Lstat(victim)
	if err != nil {
		t.Fatal(err)
	}

	staged := &Staged{FileSecrets: []FileSecret{
		{Name: "TLS_CERT", Target: filepath.Join(intermediate, "b", "cert.pem"), Value: base64.StdEncoding.EncodeToString([]byte("cert-content"))},
	}}
	if err := writeAs(homeDir, staged, os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("writeAs() = nil error, want a refusal for a symlinked intermediate component")
	}

	gotInfo, err := os.Lstat(victim)
	if err != nil {
		t.Fatal(err)
	}
	if gotInfo.Mode() != wantInfo.Mode() {
		t.Errorf("victim mode changed: got %v, want %v", gotInfo.Mode(), wantInfo.Mode())
	}
	if _, err := os.Stat(filepath.Join(victim, "b")); !os.IsNotExist(err) {
		t.Errorf("a new component was created through the symlinked intermediate directory")
	}
}

// TestWriteAs_ChownsOnlyUnderHome proves that a file-secret
// target UNDER homeDir is eligible to be chowned (its newly created parent
// directory and its leaf both go through the chown path), while a target
// OUTSIDE homeDir is written exactly as at base — created, but never chowned
// to the workload uid/gid — with containment decided by the dirfd walk, not
// a string-prefix check.
//
// Both subtests pass uid=1/gid=1, a uid the test process (unprivileged) can
// never legitimately chown to. Under home, that makes the chown attempt
// fail, proving the path really is reached. Outside home, the same uid/gid
// causes no failure at all, proving the opposite: the path is skipped
// entirely, not merely attempted-and-ignored.
func TestWriteAs_ChownsOnlyUnderHome(t *testing.T) {
	t.Run("under home directory is a chown target", func(t *testing.T) {
		homeDir := t.TempDir()
		target := filepath.Join(homeDir, "nested", "cert.pem")
		staged := &Staged{FileSecrets: []FileSecret{
			{Name: "TLS_CERT", Target: target, Value: base64.StdEncoding.EncodeToString([]byte("cert-content"))},
		}}
		if err := writeAs(homeDir, staged, 1, 1); err == nil {
			t.Fatal("writeAs() = nil error, want a chown failure proving the under-home directory is a chown target")
		}
	})

	t.Run("outside home directory is created but never chowned", func(t *testing.T) {
		homeDir := t.TempDir()
		outsideDir := t.TempDir()
		target := filepath.Join(outsideDir, "nested", "cert.pem")
		staged := &Staged{FileSecrets: []FileSecret{
			{Name: "TLS_CERT", Target: target, Value: base64.StdEncoding.EncodeToString([]byte("cert-content"))},
		}}
		// uid 1 is never the current test uid; if writeAs tried to chown the
		// outside-home directory or leaf to it, this would fail with EPERM
		// for an unprivileged test process — proving the outside-home path
		// really does skip the chown rather than merely succeeding to chown
		// to a value that happens to match.
		if err := writeAs(homeDir, staged, 1, 1); err != nil {
			t.Fatalf("writeAs failed (outside-home target must not be chowned): %v", err)
		}
		content, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("read %s: %v", target, err)
		}
		if string(content) != "cert-content" {
			t.Errorf("content = %q, want %q", content, "cert-content")
		}
	})
}

// TestWriteAs_DirectInHomeTargetChowned proves a file secret whose Target
// lives directly inside homeDir (dir == homeDir itself, e.g. ~/.netrc,
// ~/.npmrc, ~/.git-credentials) is chown-eligible exactly like any other
// under-home target — this is the shape
// dirfd.EnsureDirNoFollowUnderRoot's path==root handling exists for.
func TestWriteAs_DirectInHomeTargetChowned(t *testing.T) {
	homeDir := t.TempDir()
	target := filepath.Join(homeDir, ".netrc")

	// uid 1 is never the current test uid; a chown ATTEMPT to it fails
	// EPERM for an unprivileged test process — proving the leaf really is
	// treated as under-home (chown-eligible), not silently skipped.
	staged := &Staged{FileSecrets: []FileSecret{
		{Name: "NETRC", Target: target, Value: base64.StdEncoding.EncodeToString([]byte("machine example.com"))},
	}}
	if err := writeAs(homeDir, staged, 1, 1); err == nil {
		t.Fatal("writeAs() = nil error, want a chown failure proving a direct-in-home target is chown-eligible")
	}

	// With the test's own uid/gid (an unprivileged self-chown always
	// succeeds), the write must succeed end to end and land the content —
	// proving the chown-eligible path doesn't just fail differently, it
	// actually works.
	staged2 := &Staged{FileSecrets: []FileSecret{
		{Name: "NETRC", Target: target, Value: base64.StdEncoding.EncodeToString([]byte("machine example.com"))},
	}}
	if err := writeAs(homeDir, staged2, os.Getuid(), os.Getgid()); err != nil {
		t.Fatalf("writeAs with the caller's own uid/gid failed: %v", err)
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read %s: %v", target, err)
	}
	if string(content) != "machine example.com" {
		t.Errorf("content = %q, want %q", content, "machine example.com")
	}
}

// TestWriteAs_ContainmentTable is the containment table: a target whose
// parent directory IS homeDir itself (dir == root) is chown-eligible
// exactly like any other under-home target, while a target whose parent
// merely shares homeDir's own string prefix (a sibling directory, not a
// descendant) is never chowned. Treating the direct-in-home case as
// chown-eligible must not loosen containment for a path that only looks
// similar as a string.
func TestWriteAs_ContainmentTable(t *testing.T) {
	parent := t.TempDir()
	homeDir := filepath.Join(parent, "home")
	if err := os.Mkdir(homeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	siblingDir := filepath.Join(parent, "home-other")
	if err := os.Mkdir(siblingDir, 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name          string
		target        string
		wantChownable bool // a uid=1 chown attempt should fail iff the target is chown-eligible
	}{
		{"direct-in-home target is chown-eligible", filepath.Join(homeDir, ".netrc"), true},
		{"sibling-of-home target is never chowned", filepath.Join(siblingDir, "leaf"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			staged := &Staged{FileSecrets: []FileSecret{
				{Name: "X", Target: tt.target, Value: base64.StdEncoding.EncodeToString([]byte("v"))},
			}}
			err := writeAs(homeDir, staged, 1, 1)
			if tt.wantChownable && err == nil {
				t.Fatal("writeAs() = nil error, want a chown failure (target should be chown-eligible)")
			}
			if !tt.wantChownable && err != nil {
				t.Fatalf("writeAs() = %v, want nil (target must never be chowned)", err)
			}
		})
	}
}

func TestDecodeErrors(t *testing.T) {
	for name, encoded := range map[string]string{
		"invalid base64": "not-valid-base64!!!",
		"invalid JSON":   base64.StdEncoding.EncodeToString([]byte("not json")),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(encoded); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

// TestWriteAs_OutsideHomeTrustedSymlinkedAncestorSucceeds is the regression
// test for a file-secret Target outside homeDir whose path crosses a
// TRUSTED (root-owned, non-group/other-writable) symlinked ancestor — the
// shape of a real "/var/run" -> "/run" alias, the Kubernetes-conventional
// secrets location. Before the trusted-ancestor-follow fix, the outside-home
// branch's os.MkdirAll(dir) would have created "varrun/secrets" as a
// regular directory tree (never resolving the symlink itself, since
// MkdirAll only fails on a symlink it walks THROUGH, not one it walks
// INTO) while the following WriteFileNoFollow(fs.Target, ...) call's own
// strict no-follow walk refused the very same "varrun" symlinked ancestor
// outright — so the whole write failed closed even for this trusted,
// operator/system path. This test fails on that old code path and passes
// once the outside-home branch resolves dir via
// dirfd.EnsureDirTrustedAncestorFollow instead.
func TestWriteAs_OutsideHomeTrustedSymlinkedAncestorSucceeds(t *testing.T) {
	restore := dirfd.SetTrustedAncestorOwnerUIDForTest(os.Getuid())
	defer restore()

	homeDir := t.TempDir()
	parent := t.TempDir()
	run := filepath.Join(parent, "run")
	if err := os.Mkdir(run, 0755); err != nil {
		t.Fatal(err)
	}
	varrun := filepath.Join(parent, "varrun")
	if err := os.Symlink(run, varrun); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(varrun, "secrets", "sa.json")

	staged := &Staged{FileSecrets: []FileSecret{
		{Name: "SA", Target: target, Value: base64.StdEncoding.EncodeToString([]byte("sa-token"))},
	}}
	if err := writeAs(homeDir, staged, os.Getuid(), os.Getgid()); err != nil {
		t.Fatalf("writeAs() = %v, want nil (a trusted symlinked ancestor like /var/run -> /run must still work)", err)
	}

	content, err := os.ReadFile(filepath.Join(run, "secrets", "sa.json"))
	if err != nil {
		t.Fatalf("read %s/secrets/sa.json: %v", run, err)
	}
	if string(content) != "sa-token" {
		t.Errorf("content = %q, want %q", content, "sa-token")
	}
}

// TestWriteAs_OutsideHomeUntrustedSymlinkedAncestorFails proves the negative
// twin of the test above: an ancestor symlink that is NOT owned by the
// trusted uid is refused (fatal), and nothing is created at the link's
// destination — writing a secret to an unintended, workload-influenceable
// location is worse than not starting.
func TestWriteAs_OutsideHomeUntrustedSymlinkedAncestorFails(t *testing.T) {
	// The seam is set to a uid that can never equal the fixtures' real
	// owner (os.Getuid()), rather than left at its default (0): relying on
	// the default would make this test pass for the wrong reason — and
	// silently stop testing anything — if the suite ever ran as root,
	// where os.Getuid() == 0 == the default trusted uid.
	restore := dirfd.SetTrustedAncestorOwnerUIDForTest(os.Getuid() + 1)
	defer restore()

	homeDir := t.TempDir()
	parent := t.TempDir()
	run := filepath.Join(parent, "run")
	if err := os.Mkdir(run, 0755); err != nil {
		t.Fatal(err)
	}
	varrun := filepath.Join(parent, "varrun")
	if err := os.Symlink(run, varrun); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(varrun, "secrets", "sa.json")

	staged := &Staged{FileSecrets: []FileSecret{
		{Name: "SA", Target: target, Value: base64.StdEncoding.EncodeToString([]byte("sa-token"))},
	}}
	if err := writeAs(homeDir, staged, os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("writeAs() = nil error, want a refusal for an untrusted symlinked ancestor")
	}

	if _, err := os.Stat(filepath.Join(run, "secrets")); !os.IsNotExist(err) {
		t.Errorf("run/secrets exists after a refused untrusted ancestor (stat err=%v); nothing must be created at the link's destination", err)
	}
}

// TestWriteAs_OutsideHomeGroupWritableAncestorParentFails proves the other
// negative shape: a symlink owned by the trusted uid but sitting in a
// group/other-writable directory is still refused, and nothing is created
// at the link's destination — ownership of the link alone is not enough,
// since a workload with write access to its containing directory could
// have replanted it.
func TestWriteAs_OutsideHomeGroupWritableAncestorParentFails(t *testing.T) {
	restore := dirfd.SetTrustedAncestorOwnerUIDForTest(os.Getuid())
	defer restore()

	homeDir := t.TempDir()
	parent := t.TempDir()
	if err := os.Chmod(parent, 0777); err != nil {
		t.Fatal(err)
	}
	run := filepath.Join(parent, "run")
	if err := os.Mkdir(run, 0755); err != nil {
		t.Fatal(err)
	}
	varrun := filepath.Join(parent, "varrun")
	if err := os.Symlink(run, varrun); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(varrun, "secrets", "sa.json")

	staged := &Staged{FileSecrets: []FileSecret{
		{Name: "SA", Target: target, Value: base64.StdEncoding.EncodeToString([]byte("sa-token"))},
	}}
	if err := writeAs(homeDir, staged, os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("writeAs() = nil error, want a refusal for a symlink in a group/other-writable directory")
	}

	if _, err := os.Stat(filepath.Join(run, "secrets")); !os.IsNotExist(err) {
		t.Errorf("run/secrets exists after a refused world-writable-parent ancestor (stat err=%v); nothing must be created at the link's destination", err)
	}
}

// TestWriteAs_HomeDirReachedViaTrustedSymlinkedAncestor proves the
// home-directory case: homeDir itself reached through a trusted symlinked
// ancestor — an image where, say, "/home" -> "/var/home" — still works
// end to end, including the under-home chown path, instead of aborting
// every staged-secret write the way a strict no-follow open of homeDir's
// own chain would.
func TestWriteAs_HomeDirReachedViaTrustedSymlinkedAncestor(t *testing.T) {
	restore := dirfd.SetTrustedAncestorOwnerUIDForTest(os.Getuid())
	defer restore()

	parent := t.TempDir()
	varHome := filepath.Join(parent, "varhome")
	if err := os.Mkdir(varHome, 0755); err != nil {
		t.Fatal(err)
	}
	homeLink := filepath.Join(parent, "home")
	if err := os.Symlink(varHome, homeLink); err != nil {
		t.Fatal(err)
	}
	// homeDir itself ("home/scion") does not exist yet under the real
	// target (varhome/scion): EnsureDirTrustedAncestorFollow must create it
	// while resolving homeDir's own chain, the same as it would for any
	// other missing component.
	homeDir := filepath.Join(homeLink, "scion")
	target := filepath.Join(homeDir, ".netrc")

	staged := &Staged{FileSecrets: []FileSecret{
		{Name: "NETRC", Target: target, Value: base64.StdEncoding.EncodeToString([]byte("machine example.com"))},
	}}
	if err := writeAs(homeDir, staged, os.Getuid(), os.Getgid()); err != nil {
		t.Fatalf("writeAs() = %v, want nil (homeDir reached via a trusted symlinked ancestor must still work)", err)
	}

	content, err := os.ReadFile(filepath.Join(varHome, "scion", ".netrc"))
	if err != nil {
		t.Fatalf("read %s/scion/.netrc: %v", varHome, err)
	}
	if string(content) != "machine example.com" {
		t.Errorf("content = %q, want %q", content, "machine example.com")
	}
}

// TestWriteAs_RejectsTrailingSlashTarget proves a Target ending in a path
// separator is rejected outright, with nothing created anywhere near the
// destination — not silently treated as a directory (which filepath.Base
// alone would do, since it strips a trailing separator) and not silently
// normalized into a different, also-surprising destination.
func TestWriteAs_RejectsTrailingSlashTarget(t *testing.T) {
	homeDir := t.TempDir()
	targetDir := t.TempDir()
	target := filepath.Join(targetDir, "secrets", "sa.json") + "/"

	staged := &Staged{FileSecrets: []FileSecret{
		{Name: "SA", Target: target, Value: base64.StdEncoding.EncodeToString([]byte("sa-token"))},
	}}
	if err := writeAs(homeDir, staged, os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("writeAs() = nil error, want a refusal for a trailing-slash target")
	}

	if _, err := os.Stat(filepath.Join(targetDir, "secrets")); !os.IsNotExist(err) {
		t.Errorf("secrets dir exists after a refused trailing-slash target (stat err=%v); nothing must be created", err)
	}
}

// TestWriteAs_RejectsDotAndDotDotTarget proves a Target whose leaf is "."
// or ".." is rejected, rather than being resolved by filepath.Clean into
// some other, surprising location — and that nothing is created at the
// would-be parent either.
//
// The targets here are built with plain string concatenation, not
// filepath.Join or filepath.Clean: both of those collapse a trailing
// "/x/.." or "/." segment before writeAs ever sees it, which would leave
// this test exercising nothing but an ordinary, already-covered path.
func TestWriteAs_RejectsDotAndDotDotTarget(t *testing.T) {
	tests := []struct {
		name   string
		target func(targetDir string) string
	}{
		{
			name:   "dot-dot leaf",
			target: func(targetDir string) string { return targetDir + "/secrets/x/.." },
		},
		{
			name:   "dot leaf",
			target: func(targetDir string) string { return targetDir + "/secrets/." },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			homeDir := t.TempDir()
			targetDir := t.TempDir()
			target := tt.target(targetDir)

			staged := &Staged{FileSecrets: []FileSecret{
				{Name: "SA", Target: target, Value: base64.StdEncoding.EncodeToString([]byte("sa-token"))},
			}}
			if err := writeAs(homeDir, staged, os.Getuid(), os.Getgid()); err == nil {
				t.Fatalf("writeAs() = nil error, want a refusal for target %q", target)
			}

			if _, err := os.Stat(targetDir + "/secrets"); !os.IsNotExist(err) {
				t.Errorf("secrets dir exists after a refused target %q (stat err=%v); nothing must be created", target, err)
			}
		})
	}
}
