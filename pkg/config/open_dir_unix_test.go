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

package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agenthome"
)

// HarnessConfigDir.OpenDir reopens the directory FindHarnessConfigDir
// loaded from the trusted root it was found from: a directory swapped for
// a link after the lookup is refused instead of reopened by Path.
func TestHarnessConfigDirOpenDirReopensFromTrustedRoot(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	projectPath := filepath.Join(tmp, "project", DotScion)
	dir := filepath.Join(projectPath, harnessConfigsDirName, "hc")
	writeNamedHarnessConfigDir(t, dir, "inside-image:1")
	outside := filepath.Join(tmp, "outside")
	writeNamedHarnessConfigDir(t, outside, "outside-image:1")

	hc, err := FindHarnessConfigDir("hc", projectPath)
	if err != nil {
		t.Fatal(err)
	}
	h, err := hc.OpenDir()
	if err != nil {
		t.Fatalf("OpenDir: %v", err)
	}
	if _, err := h.ReadFile("config.yaml", 1<<20); err != nil {
		t.Fatalf("read through OpenDir: %v", err)
	}
	_ = h.Close()

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, outside, dir)
	if h, err := hc.OpenDir(); err == nil {
		_ = h.Close()
		t.Fatal("OpenDir followed a link swapped in after the lookup")
	} else if !errors.Is(err, agenthome.ErrLinkComponent) {
		t.Fatalf("OpenDir = %v, want ErrLinkComponent", err)
	}
}

// A HarnessConfigDir loaded without a trusted root opens the directory
// itself without following a link.
func TestHarnessConfigDirOpenDirWithoutRoot(t *testing.T) {
	tmp := t.TempDir()
	realDir := filepath.Join(tmp, "real")
	writeNamedHarnessConfigDir(t, realDir, "img:1")
	linked := filepath.Join(tmp, "linked")
	mustSymlink(t, realDir, linked)
	if _, err := (&HarnessConfigDir{Path: linked}).OpenDir(); !errors.Is(err, agenthome.ErrLinkComponent) {
		t.Fatalf("OpenDir of a linked Path = %v, want ErrLinkComponent", err)
	}
	if _, err := (&HarnessConfigDir{}).OpenDir(); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("OpenDir with no Path = %v, want fs.ErrNotExist", err)
	}
	h, err := (&HarnessConfigDir{Path: realDir}).OpenDir()
	if err != nil {
		t.Fatal(err)
	}
	_ = h.Close()
}

// Template.OpenDir refuses a template directory that is a link.
func TestTemplateOpenDirRefusesLink(t *testing.T) {
	tmp := t.TempDir()
	realDir := filepath.Join(tmp, "real")
	if err := os.MkdirAll(filepath.Join(realDir, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(tmp, "linked")
	mustSymlink(t, realDir, linked)
	if _, err := (&Template{Path: linked}).OpenDir(); !errors.Is(err, agenthome.ErrLinkComponent) {
		t.Fatalf("OpenDir of a linked template = %v, want ErrLinkComponent", err)
	}
	h, err := (&Template{Path: realDir}).OpenDir()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()
	if infos, err := h.ReadDir("."); err != nil || len(infos) != 1 || !infos[0].IsDir() {
		t.Fatalf("ReadDir(.) = %v, %v; want the skills dir", infos, err)
	}
}

// A HarnessConfigDir whose Path is changed after the lookup no longer uses
// the recorded trusted root: (a) a Path naming another real directory opens
// that directory; (b) a Path naming a linked directory is refused, never
// followed.
func TestHarnessConfigDirOpenDirChangedPath(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	projectPath := filepath.Join(tmp, "project", DotScion)
	writeNamedHarnessConfigDir(t, filepath.Join(projectPath, harnessConfigsDirName, "hc"), "found-image:1")
	other := filepath.Join(tmp, "other")
	writeNamedHarnessConfigDir(t, other, "other-image:1")
	linked := filepath.Join(tmp, "linked")
	mustSymlink(t, other, linked)

	hc, err := FindHarnessConfigDir("hc", projectPath)
	if err != nil {
		t.Fatal(err)
	}
	moved := *hc
	moved.Path = other
	h, err := moved.OpenDir()
	if err != nil {
		t.Fatalf("OpenDir of a changed real Path: %v", err)
	}
	data, err := h.ReadFile("config.yaml", 1<<20)
	_ = h.Close()
	if err != nil || !strings.Contains(string(data), "other-image:1") {
		t.Fatalf("OpenDir of a changed Path read %q, %v; want the directory Path names", data, err)
	}

	moved.Path = linked
	if h, err := moved.OpenDir(); err == nil {
		_ = h.Close()
		t.Fatal("OpenDir followed a changed Path that is a link")
	} else if !errors.Is(err, agenthome.ErrLinkComponent) {
		t.Fatalf("OpenDir of a linked changed Path = %v, want ErrLinkComponent", err)
	}
}
