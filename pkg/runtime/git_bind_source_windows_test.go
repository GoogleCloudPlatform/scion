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
//go:build windows

package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestWindowsGitBindSource_Unsupported checks that the git-metadata bind
// primitives fail closed on Windows with errors.ErrUnsupported, on paths
// that exist (so the refusal is not a missing-path result), and that
// bindDirCreate creates nothing.
func TestWindowsGitBindSource_Unsupported(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "objects"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(root, "config")
	if err := os.WriteFile(cfg, []byte("[core]\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, mode := range []bindSourceMode{bindDirRequire, bindDirCreate, bindFileRequire, bindFileCreate} {
		for _, comp := range []string{"objects", "config", "hooks"} {
			src, present, err := checkedBindSource(root, mode, comp)
			if !errors.Is(err, errors.ErrUnsupported) || present || src != "" {
				t.Fatalf("checkedBindSource(mode %d, %q) = %q, %v, %v; want ErrUnsupported, not present", mode, comp, src, present, err)
			}
		}
	}
	if _, err := os.Lstat(filepath.Join(root, "hooks")); !os.IsNotExist(err) {
		t.Fatalf("checkedBindSource created hooks: %v", err)
	}
	if err := checkPrivateGitConfigSource(cfg); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("checkPrivateGitConfigSource = %v, want ErrUnsupported", err)
	}
	if data, err := readSmallConfigFile(cfg); !errors.Is(err, errors.ErrUnsupported) || data != nil {
		t.Fatalf("readSmallConfigFile = %q, %v; want nil, ErrUnsupported", data, err)
	}
}

// TestWindowsGitBindCallers_RegisterNoMount checks that the callers of the
// bind primitives return the refusal and register no mount.
func TestWindowsGitBindCallers_RegisterNoMount(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "config")
	if err := os.WriteFile(cfg, []byte("[core]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var mounts []string
	register := func(src, dst string, _, _ bool) { mounts = append(mounts, src+" -> "+dst) }

	if err := registerPrivateGitConfig(register, RunConfig{Name: "a", PrivateGitConfigSource: cfg}, "/repo-root/.git"); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("registerPrivateGitConfig = %v, want ErrUnsupported", err)
	}
	if err := bindChecked(register, root, "/repo-root/.git", true, bindFileRequire, "config"); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("bindChecked = %v, want ErrUnsupported", err)
	}
	if err := bindReadWriteOverlayDir(register, filepath.Join(root, "objects"), "/repo-root/.git/objects", true); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("bindReadWriteOverlayDir = %v, want ErrUnsupported", err)
	}
	if len(mounts) != 0 {
		t.Fatalf("mounts registered after a refusal: %v", mounts)
	}
	if _, err := os.Lstat(filepath.Join(root, "objects")); !os.IsNotExist(err) {
		t.Fatalf("bindReadWriteOverlayDir created objects: %v", err)
	}
}
