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

package hub

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// collectedDir is a resource directory as the ResourceStore collected it: the
// collected entries and an os.Root opened at the directory. Every read of a
// collected file during the bootstrap or import that collected it goes
// through it, with transfer.ReadCollectedFile: only a collected entry is
// read, it must still be a regular file inside the root, and its size and
// hash must equal the collected ones. A path collection did not record (a
// link, a non-regular file, an excluded or missing file) reads as absent and
// is never opened.
type collectedDir struct {
	// dir is the directory path, used only in messages.
	dir    string
	root   *os.Root
	files  []transfer.FileInfo
	byPath map[string]transfer.FileInfo
}

func newCollectedDir(dir string, root *os.Root, files []transfer.FileInfo) *collectedDir {
	byPath := make(map[string]transfer.FileInfo, len(files))
	for _, f := range files {
		byPath[f.Path] = f
	}
	return &collectedDir{dir: dir, root: root, files: files, byPath: byPath}
}

// base is the directory's base name, for messages.
func (c *collectedDir) base() string {
	return filepath.Base(c.dir)
}

// entry returns the collected entry at the slash path p.
func (c *collectedDir) entry(p string) (transfer.FileInfo, bool) {
	fi, ok := c.byPath[p]
	return fi, ok
}

// read returns the verified content of the collected entry at the slash path
// p. A path that was not collected returns an error wrapping fs.ErrNotExist.
func (c *collectedDir) read(p string) ([]byte, error) {
	fi, ok := c.byPath[p]
	if !ok {
		return nil, fmt.Errorf("%s was not collected: %w", p, fs.ErrNotExist)
	}
	return transfer.ReadCollectedFile(c.root, fi)
}

// fileReader reads collected template files for derivation and the bundled
// harness-config check. A file larger than maxTemplateFileSize (by its
// collected size) returns errTemplateFileTooLarge without being read.
func (c *collectedDir) fileReader() templateFileReader {
	return func(_ context.Context, p string) ([]byte, error) {
		fi, ok := c.byPath[p]
		if !ok {
			return nil, fmt.Errorf("%s was not collected: %w", p, fs.ErrNotExist)
		}
		if fi.Size > maxTemplateFileSize {
			return nil, errTemplateFileTooLarge
		}
		return transfer.ReadCollectedFile(c.root, fi)
	}
}

// agentConfigManifest lists the collected agent config candidates, in
// scionAgentConfigNames order, as a minimal manifest.
func (c *collectedDir) agentConfigManifest() []store.TemplateFile {
	var files []store.TemplateFile
	for _, name := range scionAgentConfigNames {
		if fi, ok := c.byPath[name]; ok {
			files = append(files, store.TemplateFile{Path: fi.Path, Size: fi.Size, Hash: fi.Hash, Mode: fi.Mode})
		}
	}
	return files
}

// harnessConfigYAML returns the collected config.yaml. present is false when
// collection recorded no config.yaml. A file too large to parse is returned
// as present with no content (derived as unparsable), as before.
func (c *collectedDir) harnessConfigYAML() ([]byte, bool, error) {
	fi, ok := c.byPath[harnessConfigYAMLPath]
	if !ok {
		return nil, false, nil
	}
	if fi.Size > maxHarnessConfigFileSize {
		return nil, true, nil
	}
	data, err := transfer.ReadCollectedFile(c.root, fi)
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// subdirs returns the names of the immediate subdirectories of the slash
// directory prefix that hold at least one collected entry, sorted. A
// subdirectory collection skipped (a link) has no entries and is not listed.
func (c *collectedDir) subdirs(prefix string) []string {
	prefix = strings.TrimSuffix(prefix, "/") + "/"
	seen := map[string]bool{}
	var names []string
	for _, f := range c.files {
		rest, ok := strings.CutPrefix(f.Path, prefix)
		if !ok {
			continue
		}
		name, _, isNested := strings.Cut(rest, "/")
		if !isNested || name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// realSubdir reports whether the slash path p is a directory inside the root
// and no component of it is a link: each component is checked with Lstat
// through the root, which also refuses a path that leaves it.
func (c *collectedDir) realSubdir(p string) error {
	cur := ""
	for _, part := range strings.Split(p, "/") {
		cur = path.Join(cur, part)
		info, err := c.root.Lstat(filepath.FromSlash(cur))
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", cur)
		}
	}
	return nil
}

// errCollectedDirChanged reports a resource directory replaced while it was
// being collected.
var errCollectedDirChanged = errors.New("changed while it was being collected")
