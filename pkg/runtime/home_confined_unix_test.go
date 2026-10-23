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

package runtime

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agenthome"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

// The run-args build writes into the agent home (volume mount points, auth
// file copies) confined to the home: a link at a leaf is replaced, never
// written through, and a linked directory component is refused (the
// best-effort mount-point mkdirs skip it). Each test plants links to an
// "outside" directory and checks that directory is left exactly as it was.

func outsideTree(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		line := rel + " " + d.Type().String()
		if d.Type().IsRegular() {
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			line += " " + string(b)
		}
		lines = append(lines, line)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func newRuntimeConfinedHome(t *testing.T) (home, outside, before string) {
	t.Helper()
	base := t.TempDir()
	home, outside = filepath.Join(base, "home"), filepath.Join(base, "outside")
	for _, d := range []string{home, filepath.Join(outside, "sub")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "auth.json"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	return home, outside, outsideTree(t, outside)
}

func plantLink(t *testing.T, target, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

func volumeRunConfig(home string) RunConfig {
	return RunConfig{
		Harness:      &harness.Generic{},
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		HomeDir:      home,
		BrokerMode:   true,
		Volumes: []api.VolumeMount{
			{Source: "/host/data", Target: "/home/scion/data/cache"},
		},
	}
}

func TestBuildCommonRunArgs_MountPointUnderHome(t *testing.T) {
	home, outside, before := newRuntimeConfinedHome(t)
	if _, err := buildCommonRunArgs(volumeRunConfig(home)); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(filepath.Join(home, "data/cache")); err != nil || !fi.IsDir() {
		t.Fatalf("mount point = %v, %v; want a directory", fi, err)
	}

	// A link planted (or re-planted after the first start) at a directory
	// component is not traversed; the mkdir stays best effort.
	plantLink(t, outside, filepath.Join(home, "data"))
	if _, err := buildCommonRunArgs(volumeRunConfig(home)); err != nil {
		t.Fatalf("best-effort mount-point mkdir failed the build: %v", err)
	}
	if fi, err := os.Lstat(filepath.Join(home, "data")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("data = %v, %v; want the planted link left as is", fi, err)
	}
	if got := outsideTree(t, outside); got != before {
		t.Fatalf("outside directory changed:\n%s\nwant:\n%s", got, before)
	}
}

func authRunConfig(t *testing.T, home string) RunConfig {
	t.Helper()
	src := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(src, []byte(`{"token":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return RunConfig{
		UnixUsername: "scion",
		HomeDir:      home,
		ResolvedAuth: &api.ResolvedAuth{
			Files: []api.FileMapping{{SourcePath: src, ContainerPath: "~/.codex/auth.json"}},
		},
	}
}

func applyAuthOnly(config RunConfig) error {
	return applyResolvedAuth(config, func(string, string) {}, func(api.VolumeMount) {}, func(string, string, bool, bool) {})
}

func TestApplyResolvedAuth_LinkLeafReplaced(t *testing.T) {
	home, outside, before := newRuntimeConfinedHome(t)
	plantLink(t, filepath.Join(outside, "auth.json"), filepath.Join(home, ".codex/auth.json"))
	if err := applyAuthOnly(authRunConfig(t, home)); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(home, ".codex/auth.json")
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		t.Fatalf("auth file = %v, %v; want a regular 0600 file", fi, err)
	}
	if b, _ := os.ReadFile(p); string(b) != `{"token":"x"}` {
		t.Fatalf("auth file = %q", b)
	}
	if got := outsideTree(t, outside); got != before {
		t.Fatalf("outside directory changed:\n%s\nwant:\n%s", got, before)
	}
}

func TestApplyResolvedAuth_RefusesLinkedComponent(t *testing.T) {
	home, outside, before := newRuntimeConfinedHome(t)
	plantLink(t, outside, filepath.Join(home, ".codex"))
	if err := applyAuthOnly(authRunConfig(t, home)); !errors.Is(err, agenthome.ErrLinkComponent) {
		t.Fatalf("err = %v, want a linked directory component refusal", err)
	}
	if got := outsideTree(t, outside); got != before {
		t.Fatalf("outside directory changed:\n%s\nwant:\n%s", got, before)
	}
}

func TestApplyResolvedAuth_LinkReplantedBetweenStartsIsRefused(t *testing.T) {
	home, outside, before := newRuntimeConfinedHome(t)
	if err := applyAuthOnly(authRunConfig(t, home)); err != nil {
		t.Fatal(err)
	}
	plantLink(t, outside, filepath.Join(home, ".codex"))
	if err := applyAuthOnly(authRunConfig(t, home)); !errors.Is(err, agenthome.ErrLinkComponent) {
		t.Fatalf("err = %v, want the re-planted link refused", err)
	}
	if got := outsideTree(t, outside); got != before {
		t.Fatalf("outside directory changed:\n%s\nwant:\n%s", got, before)
	}
}

// --- cloudrun sandbox auth file copy (copySandboxAuthFiles) ---

func sandboxAuthFiles(t *testing.T) []api.FileMapping {
	t.Helper()
	return authRunConfig(t, "").ResolvedAuth.Files
}

func TestCopySandboxAuthFiles_LinkLeafReplaced(t *testing.T) {
	home, outside, before := newRuntimeConfinedHome(t)
	plantLink(t, filepath.Join(outside, "auth.json"), filepath.Join(home, ".codex/auth.json"))
	copySandboxAuthFiles(home, sandboxAuthFiles(t))
	p := filepath.Join(home, ".codex/auth.json")
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("auth file = %v, %v; want a regular file", fi, err)
	}
	if b, _ := os.ReadFile(p); string(b) != `{"token":"x"}` {
		t.Fatalf("auth file = %q", b)
	}
	if got := outsideTree(t, outside); got != before {
		t.Fatalf("outside directory changed:\n%s\nwant:\n%s", got, before)
	}
}

func TestCopySandboxAuthFiles_LinkedComponentSkipped(t *testing.T) {
	home, outside, before := newRuntimeConfinedHome(t)
	// First copy succeeds; a link re-planted at the directory afterwards is
	// not traversed by the next copy (best effort: logged and skipped).
	copySandboxAuthFiles(home, sandboxAuthFiles(t))
	plantLink(t, filepath.Join(outside, "sub"), filepath.Join(home, ".codex"))
	copySandboxAuthFiles(home, sandboxAuthFiles(t))
	if fi, err := os.Lstat(filepath.Join(home, ".codex")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf(".codex = %v, %v; want the planted link left as is", fi, err)
	}
	if got := outsideTree(t, outside); got != before {
		t.Fatalf("outside directory changed:\n%s\nwant:\n%s", got, before)
	}
}

// The auth file copy keeps the source's permission bits less group and
// other write, as the old umask-applied copy did.
func TestCopyFileIntoHome_StripsGroupOtherWrite(t *testing.T) {
	home, _, _ := newRuntimeConfinedHome(t)
	src := filepath.Join(t.TempDir(), "cred")
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(src, 0o666); err != nil {
		t.Fatal(err)
	}
	h, err := agenthome.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()
	if err := copyFileIntoHome(src, h, "d/cred"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(filepath.Join(home, "d/cred")); err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("copy = %v, %v; want mode 0644", fi, err)
	}
}
