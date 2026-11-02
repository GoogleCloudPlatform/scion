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
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// contentFixture is a project config dir (.scion) holding templates/tpl,
// and an outside dir holding agents.md with the outside marker.
type contentFixture struct {
	scionDir, tplDir, outside string
}

func newContentFixture(t *testing.T) *contentFixture {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	base := t.TempDir()
	f := &contentFixture{
		scionDir: filepath.Join(base, "project", DotScion),
		outside:  filepath.Join(base, "outside"),
	}
	f.tplDir = filepath.Join(f.scionDir, "templates", "tpl")
	for _, d := range []string{f.tplDir, f.outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(f.outside, "agents.md"), []byte(contentOutsideMarker), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

// projectTemplate finds the fixture's template the way provisioning does.
func (f *contentFixture) projectTemplate(t *testing.T) *Template {
	t.Helper()
	tpl, err := FindTemplateInProjectPath("tpl", f.scionDir)
	if err != nil {
		t.Fatalf("FindTemplateInProjectPath: %v", err)
	}
	return tpl
}

// assertRefused fails unless err wraps ErrTemplateContentRefused, data is
// empty, and neither carries the outside marker or a host path.
func assertRefused(t *testing.T, data []byte, err error, f *contentFixture) {
	t.Helper()
	if err == nil {
		t.Fatalf("read was not refused; got %q", data)
	}
	if !errors.Is(err, ErrTemplateContentRefused) {
		t.Fatalf("error = %v, want ErrTemplateContentRefused", err)
	}
	if bytes.Contains(data, []byte(contentOutsideMarker)) {
		t.Fatal("content from outside the template was returned")
	}
	for _, p := range []string{f.outside, f.tplDir, f.scionDir} {
		if strings.Contains(err.Error(), p) {
			t.Errorf("error %q names a host path", err)
		}
	}
}

// A symbolic link at the content file, dangling or pointing out of the
// template, is refused with an error and nothing is read.
func TestTemplateContentRefusesLinkOutOfTemplate(t *testing.T) {
	for name, target := range map[string]func(f *contentFixture) string{
		"link out":     func(f *contentFixture) string { return filepath.Join(f.outside, "agents.md") },
		"dangling":     func(f *contentFixture) string { return filepath.Join(f.outside, "missing.md") },
		"link in tree": func(f *contentFixture) string { return "other.md" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newContentFixture(t)
			if err := os.WriteFile(filepath.Join(f.tplDir, "other.md"), []byte("inside"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target(f), filepath.Join(f.tplDir, "agents.md")); err != nil {
				t.Fatal(err)
			}
			tpl := f.projectTemplate(t)
			data, err := ResolveContentInChain([]*Template{tpl}, "agents.md")
			assertRefused(t, data, err, f)
			if !strings.Contains(err.Error(), "replace it with the file itself") {
				t.Errorf("error %q does not say how to fix it", err)
			}
		})
	}
}

// A linked directory on the way to the content file, the template
// directory itself, or the templates directory above it, is refused.
func TestTemplateContentRefusesLinkedDirectories(t *testing.T) {
	t.Run("component below the template", func(t *testing.T) {
		f := newContentFixture(t)
		if err := os.Symlink(f.outside, filepath.Join(f.tplDir, "docs")); err != nil {
			t.Fatal(err)
		}
		data, err := ResolveContentInChain([]*Template{f.projectTemplate(t)}, "docs/agents.md")
		assertRefused(t, data, err, f)
	})
	t.Run("template directory found from the project", func(t *testing.T) {
		f := newContentFixture(t)
		tplLink := filepath.Join(f.scionDir, "templates", "linked")
		if err := os.Symlink(f.outside, tplLink); err != nil {
			t.Fatal(err)
		}
		tpl, err := FindTemplateInProjectPath("linked", f.scionDir)
		if err != nil {
			t.Fatalf("FindTemplateInProjectPath: %v", err)
		}
		data, err := ResolveContentInChain([]*Template{tpl}, "agents.md")
		assertRefused(t, data, err, f)
		if TemplateHasContentFile(tpl, "agents.md") {
			t.Error("agents.md behind a linked template directory was auto-detected")
		}
	})
	t.Run("templates directory found from the project", func(t *testing.T) {
		f := newContentFixture(t)
		templates := filepath.Join(f.scionDir, "templates")
		if err := os.RemoveAll(templates); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(f.outside, "tpl"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.outside, "tpl", "agents.md"), []byte(contentOutsideMarker), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(f.outside, templates); err != nil {
			t.Fatal(err)
		}
		data, err := ResolveContentInChain([]*Template{f.projectTemplate(t)}, "agents.md")
		assertRefused(t, data, err, f)
	})
	t.Run("template directory as a Template literal", func(t *testing.T) {
		f := newContentFixture(t)
		tplLink := filepath.Join(filepath.Dir(f.tplDir), "literal")
		if err := os.Symlink(f.outside, tplLink); err != nil {
			t.Fatal(err)
		}
		tpl := &Template{Name: "literal", Path: tplLink}
		data, err := tpl.ResolveContent("agents.md")
		assertRefused(t, data, err, f)
	})
}

// A hard-linked file, a FIFO, a path through a regular file and a file over
// the size bound are refused; the FIFO read does not block.
func TestTemplateContentRefusesHardLinkAndNonRegular(t *testing.T) {
	t.Run("hard link", func(t *testing.T) {
		f := newContentFixture(t)
		if err := os.Link(filepath.Join(f.outside, "agents.md"), filepath.Join(f.tplDir, "agents.md")); err != nil {
			t.Skipf("hard link not supported here: %v", err)
		}
		data, err := ResolveContentInChain([]*Template{f.projectTemplate(t)}, "agents.md")
		assertRefused(t, data, err, f)
	})
	t.Run("fifo", func(t *testing.T) {
		f := newContentFixture(t)
		fifo := filepath.Join(f.tplDir, "agents.md")
		if err := syscall.Mkfifo(fifo, 0o644); err != nil {
			t.Skipf("mkfifo not supported here: %v", err)
		}
		// A writer that opens and closes the FIFO as soon as a reader has
		// it open, so a read that wrongly opens it ends (at EOF) instead
		// of blocking the test.
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
					_ = w.Close()
				}
				time.Sleep(5 * time.Millisecond)
			}
		}()
		data, err := ResolveContentInChain([]*Template{f.projectTemplate(t)}, "agents.md")
		assertRefused(t, data, err, f)
	})
	t.Run("path through a regular file", func(t *testing.T) {
		f := newContentFixture(t)
		if err := os.WriteFile(filepath.Join(f.tplDir, "agents.md"), []byte("file"), 0o644); err != nil {
			t.Fatal(err)
		}
		data, err := ResolveContentInChain([]*Template{f.projectTemplate(t)}, "agents.md/x.md")
		assertRefused(t, data, err, f)
	})
	t.Run("over the size bound", func(t *testing.T) {
		f := newContentFixture(t)
		big := bytes.Repeat([]byte("a"), MaxTemplateContentBytes+1)
		if err := os.WriteFile(filepath.Join(f.tplDir, "agents.md"), big, 0o644); err != nil {
			t.Fatal(err)
		}
		data, err := ResolveContentInChain([]*Template{f.projectTemplate(t)}, "agents.md")
		assertRefused(t, data, err, f)
	})
}

// The convention lookup detects a plain agents.md and not a linked one.
func TestTemplateHasContentFileOnlyForPlainFile(t *testing.T) {
	f := newContentFixture(t)
	tpl := f.projectTemplate(t)
	if TemplateHasContentFile(tpl, "agents.md") {
		t.Fatal("absent agents.md detected")
	}
	if err := os.Symlink(filepath.Join(f.outside, "agents.md"), filepath.Join(f.tplDir, "agents.md")); err != nil {
		t.Fatal(err)
	}
	if TemplateHasContentFile(tpl, "agents.md") {
		t.Error("linked agents.md detected")
	}
	if err := os.WriteFile(filepath.Join(f.tplDir, "system-prompt.md"), []byte("prompt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !TemplateHasContentFile(tpl, "system-prompt.md") {
		t.Error("plain system-prompt.md not detected")
	}
}
