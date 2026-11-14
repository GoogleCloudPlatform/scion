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

package agent

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"log/slog"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
)

// Experiment-gated platform skill content (ptone/scion#7830, which also
// delivers ptone/scion#7399). A Markdown file in a platform skill may
// declare `experiment: <id>` in its YAML frontmatter: on SKILL.md it gates
// the whole skill, on any other *.md file it gates that file. Inside any
// *.md file, lines between
//
//	<!-- scion:experiment <id> -->
//	<!-- scion:end-experiment -->
//
// (each marker on its own line) are kept only while <id> is on; the marker
// lines themselves are always removed. Every gate fails closed: an
// unknown or retired ID, missing Hub state, or an unclosed or nested block
// means off.

// experimentOn returns the experiment state source for this provisioning
// or start. In broker mode only the Hub decides, through the dispatch
// experiments in HubAgentDefaults (nil defaults: every experiment is off);
// the broker host's settings are never consulted. Outside broker mode
// (solo/local CLI) the effective settings' local_experiments decide.
func experimentOn(ctx context.Context, settings *config.VersionedSettings) func(id string) bool {
	if api.IsBrokerModeFromContext(ctx) {
		return api.HubAgentDefaultsFromContext(ctx).ExperimentEnabled
	}
	return func(id string) bool {
		return settings != nil && slices.Contains(settings.LocalExperiments, id)
	}
}

// skillExperimentGate wraps isOn for skill gates: an ID that is not an
// active registered experiment (unknown or retired) is off, with a warning,
// and a nil isOn means every gate is off.
func skillExperimentGate(isOn func(string) bool) func(string) bool {
	return func(id string) bool {
		if _, ok := experiments.Default().Lookup(id); !ok {
			slog.Warn("platform skill gate names an unknown or retired experiment; treating it as off", "experiment", id)
			return false
		}
		return isOn != nil && isOn(id)
	}
}

var (
	experimentOpenRe = regexp.MustCompile(`^<!--\s*scion:experiment\s+(\S+)\s*-->$`)
	experimentEndRe  = regexp.MustCompile(`^<!--\s*scion:end-experiment\s*-->$`)
)

// gateMarkdown applies the experiment gates of one Markdown file. It
// reports keep=false when the file's frontmatter names an experiment that
// is off. Otherwise it returns the content with every gated block
// resolved; content without block markers is returned unchanged, byte for
// byte. Line endings (LF or CRLF) are preserved.
func gateMarkdown(name string, data []byte, isOn func(string) bool) (out []byte, keep bool) {
	if id := parseSkillFrontmatter(data).Experiment; id != "" && !isOn(id) {
		return nil, false
	}
	if !bytes.Contains(data, []byte("scion:")) {
		return data, true
	}
	var buf bytes.Buffer
	buf.Grow(len(data))
	inBlock, blockOn, changed := false, false, false
	rest := data
	for len(rest) > 0 {
		line := rest
		if i := bytes.IndexByte(rest, '\n'); i >= 0 {
			line = rest[:i+1]
		}
		rest = rest[len(line):]
		trimmed := bytes.TrimSpace(line)
		if m := experimentOpenRe.FindSubmatch(trimmed); m != nil {
			changed = true
			id := string(m[1])
			if inBlock {
				slog.Warn("platform skill has a nested experiment block; dropping the rest of the file", "file", name, "experiment", id)
				return buf.Bytes(), true
			}
			inBlock, blockOn = true, isOn(id)
			continue
		}
		if experimentEndRe.Match(trimmed) {
			changed = true
			if !inBlock {
				slog.Warn("platform skill has an end-experiment marker without an opener; removing it", "file", name)
			}
			inBlock = false
			continue
		}
		if !inBlock || blockOn {
			buf.Write(line)
		}
	}
	if inBlock {
		slog.Warn("platform skill has an unclosed experiment block; dropped it to the end of the file", "file", name)
	}
	if !changed {
		return data, true
	}
	return buf.Bytes(), true
}

// gatedView returns a read-only view of fsys with every experiment gate of
// its Markdown files applied (gateMarkdown): a gated-off file is absent,
// and a file with gated blocks reads as resolved, with matching sizes in
// Stat and ReadDir. Everything else reads through unchanged. isOn is
// called with each experiment ID a gate names; wrap it with
// skillExperimentGate for the unknown-ID rule. A SKILL.md whose own gate
// is off is absent too; the caller decides whether that skips the skill.
func gatedView(fsys fs.FS, isOn func(string) bool) fs.FS {
	return &gatedFS{base: fsys, isOn: isOn}
}

type gatedFS struct {
	base fs.FS
	isOn func(string) bool
}

var (
	_ fs.ReadDirFS  = (*gatedFS)(nil)
	_ fs.StatFS     = (*gatedFS)(nil)
	_ fs.ReadFileFS = (*gatedFS)(nil)
)

func isMarkdownName(name string) bool { return strings.HasSuffix(name, ".md") }

// resolve reads and gates the Markdown file name. ok is false when the
// file is gated off.
func (g *gatedFS) resolve(name string) (data []byte, ok bool, err error) {
	raw, err := fs.ReadFile(g.base, name)
	if err != nil {
		return nil, false, err
	}
	data, ok = gateMarkdown(name, raw, g.isOn)
	return data, ok, nil
}

func (g *gatedFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	f, err := g.base.Open(name)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	switch {
	case fi.IsDir():
		return &gatedDir{File: f, fsys: g, name: name}, nil
	case fi.Mode().IsRegular() && isMarkdownName(name):
		_ = f.Close()
		data, ok, err := g.resolve(name)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
		}
		return &gatedFile{Reader: bytes.NewReader(data), info: sizedInfo{FileInfo: fi, size: int64(len(data))}}, nil
	default:
		return f, nil
	}
}

func (g *gatedFS) ReadFile(name string) ([]byte, error) {
	f, err := g.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(f)
}

func (g *gatedFS) Stat(name string) (fs.FileInfo, error) {
	f, err := g.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return f.Stat()
}

func (g *gatedFS) ReadDir(name string) ([]fs.DirEntry, error) {
	des, err := fs.ReadDir(g.base, name)
	if err != nil {
		return nil, err
	}
	out := make([]fs.DirEntry, 0, len(des))
	for _, d := range des {
		if !d.Type().IsRegular() || !isMarkdownName(d.Name()) {
			out = append(out, d)
			continue
		}
		full := path.Join(name, d.Name())
		data, ok, err := g.resolve(full)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		out = append(out, sizedEntry{DirEntry: d, size: int64(len(data))})
	}
	return out, nil
}

// gatedDir is a directory of a gatedFS; ReadDir lists the gated entries.
type gatedDir struct {
	fs.File
	fsys    *gatedFS
	name    string
	entries []fs.DirEntry
	read    bool
}

func (d *gatedDir) ReadDir(n int) ([]fs.DirEntry, error) {
	if !d.read {
		es, err := d.fsys.ReadDir(d.name)
		if err != nil {
			return nil, err
		}
		d.entries, d.read = es, true
	}
	if n <= 0 {
		out := d.entries
		d.entries = nil
		return out, nil
	}
	if len(d.entries) == 0 {
		return nil, io.EOF
	}
	n = min(n, len(d.entries))
	out := d.entries[:n]
	d.entries = d.entries[n:]
	return out, nil
}

// gatedFile is a resolved Markdown file of a gatedFS.
type gatedFile struct {
	*bytes.Reader
	info fs.FileInfo
}

func (f *gatedFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *gatedFile) Close() error               { return nil }

// sizedInfo reports a resolved file's size in place of the source's.
type sizedInfo struct {
	fs.FileInfo
	size int64
}

func (i sizedInfo) Size() int64 { return i.size }

type sizedEntry struct {
	fs.DirEntry
	size int64
}

func (e sizedEntry) Info() (fs.FileInfo, error) {
	fi, err := e.DirEntry.Info()
	if err != nil {
		return nil, err
	}
	return sizedInfo{FileInfo: fi, size: e.size}, nil
}
