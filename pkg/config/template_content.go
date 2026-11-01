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

package config

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/GoogleCloudPlatform/scion/pkg/agenthome"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
)

// ErrTemplateContentRefused is wrapped by the errors of template content
// reads (agent_instructions, system_prompt) that are refused: a file value
// that is not a relative path inside the template directory, or a file
// that is a symbolic link, sits below one, is not a plain regular file or
// is too large. The messages never contain the value or a host path.
var ErrTemplateContentRefused = errors.New("template content file refused")

// MaxTemplateContentBytes bounds a content file read from a template.
const MaxTemplateContentBytes = 4 << 20

// templateBelow returns a Template for the directory rel below root, which
// is the trusted directory the template was found from: the template's
// content files are read through a walk from root that follows no link
// (Template.openDir).
func templateBelow(name, root, rel, scope string) *Template {
	return &Template{
		Name:  name,
		Path:  filepath.Join(root, rel),
		Scope: scope,
		root:  root,
		rel:   rel,
	}
}

// openDir opens the template directory for content reads. When the
// Template records the trusted directory it was found from (templateBelow),
// every component from there down is opened without following a link;
// otherwise the template directory itself is opened without following a
// link, below its parent. A missing directory satisfies
// errors.Is(err, fs.ErrNotExist).
func (t *Template) openDir() (*agenthome.Home, error) {
	clean := filepath.Clean(t.Path)
	if t.Path == "" || clean == "." || clean == string(filepath.Separator) {
		return nil, fs.ErrNotExist
	}
	if t.root != "" && filepath.Join(t.root, t.rel) == clean {
		return agenthome.OpenBelow(t.root, t.rel)
	}
	return agenthome.OpenBelow(filepath.Dir(clean), filepath.Base(clean))
}

// isTemplateContentFileName reports whether value names a file inside the
// template directory: filepath.IsLocal, no ".." element before cleaning,
// and not the directory itself.
func isTemplateContentFileName(value string) bool {
	if !filepath.IsLocal(value) || filepath.Clean(value) == "." {
		return false
	}
	for _, p := range strings.Split(filepath.ToSlash(value), "/") {
		if p == ".." {
			return false
		}
	}
	return true
}

// refusedContent returns the error for a refused content file: a fixed
// reason with a fix, wrapping ErrTemplateContentRefused, never the value or
// a host path.
func refusedContent(reason string) error {
	return fmt.Errorf("%w: %s", ErrTemplateContentRefused, reason)
}

// classifyContentReadError maps an error (other than fs.ErrNotExist) from
// opening the template directory or reading the content file to the
// reported error.
func classifyContentReadError(err error) error {
	switch {
	case errors.Is(err, agenthome.ErrLinkComponent):
		return refusedContent("a directory on the path to the content file is a symbolic link; replace the link with the directory itself")
	case errors.Is(err, agenthome.ErrNotDirectory):
		return refusedContent("a component of the content file path is not a directory")
	case errors.Is(err, syscall.ELOOP):
		return refusedContent("the content file is a symbolic link; replace it with the file itself")
	case errors.Is(err, dirfd.ErrTooLarge):
		return refusedContent("the content file is larger than 4 MiB")
	case errors.Is(err, dirfd.ErrNotSingleLinkRegular):
		return refusedContent("the content file is not a plain regular file (a symbolic link, hard link, directory or device); replace it with a regular file")
	case errors.Is(err, fs.ErrPermission):
		return refusedContent("the content file or its directory cannot be read (permission denied)")
	}
	return refusedContent("the content file cannot be read as a plain regular file inside the template directory")
}

// readTemplateContent reads the content file value in template t. found is
// false, with a nil error, when the template directory or the file does not
// exist (only fs.ErrNotExist counts as absent). value must already have
// passed isTemplateContentFileName.
func readTemplateContent(t *Template, value string) (data []byte, found bool, err error) {
	home, err := t.openDir()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, classifyContentReadError(err)
	}
	defer func() { _ = home.Close() }()
	data, err = home.ReadFile(filepath.ToSlash(value), MaxTemplateContentBytes)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, classifyContentReadError(err)
	}
	return data, true, nil
}

// resolveContentInTemplates resolves a content field value (agent_instructions
// or system_prompt) against templates, most specific first:
//
//   - an empty value has no content;
//   - a value with a line break is inline content and is never looked up;
//   - a single-line value that is not a relative path inside the template
//     directory (absolute, a ".." element, or the directory itself) is
//     refused;
//   - otherwise the first template holding the file supplies its content,
//     read without following a link (Template.openDir, agenthome); a file
//     that exists but is refused is an error, never inline content;
//   - when no template holds the file, the value is inline content.
func resolveContentInTemplates(templates []*Template, value string) ([]byte, error) {
	if value == "" {
		return nil, nil
	}
	if strings.ContainsAny(value, "\r\n") {
		return []byte(value), nil
	}
	if !isTemplateContentFileName(value) {
		return nil, refusedContent("a single-line value names a file and must be a relative path inside the template directory; use a file in the template or multi-line inline text")
	}
	for i := len(templates) - 1; i >= 0; i-- {
		if templates[i] == nil {
			continue
		}
		data, found, err := readTemplateContent(templates[i], value)
		if err != nil {
			return nil, err
		}
		if found {
			return data, nil
		}
	}
	return []byte(value), nil
}

// TemplateHasContentFile reports whether template t holds name (a relative
// path) as a content file that a content read would accept: a plain regular
// file inside the template directory, reached without a symbolic link. It
// is the convention lookup for agents.md and system-prompt.md, so a file
// that would be refused is never detected.
func TemplateHasContentFile(t *Template, name string) bool {
	if t == nil || !isTemplateContentFileName(name) {
		return false
	}
	_, found, err := readTemplateContent(t, name)
	return found && err == nil
}
