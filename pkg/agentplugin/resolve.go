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

package agentplugin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// ErrInvalidPath reports an agent_plugins path that is not a relative path
// inside the template directory.
var ErrInvalidPath = errors.New("must be relative to the template directory")

// ErrNotFound reports a plugin directory that is missing, is not a
// directory, or is (or sits under) a symlink inside the template.
var ErrNotFound = errors.New("not found or is a symlink")

// ResolveTemplateRef validates ref.Path (non-empty, not absolute,
// filepath.IsLocal, no ".." component; a leading "./" is allowed but not
// required) and opens it as a sub-root of the template directory. Every
// component is Lstat'ed and must be a real directory: a symlinked plugin
// dir, or a symlinked parent inside the template, is rejected.
func ResolveTemplateRef(templateDir string, ref api.AgentPluginReference) (*os.Root, error) {
	rel, err := cleanRefPath(ref.Path)
	if err != nil {
		return nil, err
	}
	tplRoot, err := os.OpenRoot(templateDir)
	if err != nil {
		return nil, fmt.Errorf("open template directory: %w", err)
	}
	defer func() { _ = tplRoot.Close() }()
	ok, err := realDirs(tplRoot, rel)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotFound
	}
	root, err := tplRoot.OpenRoot(rel)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	return root, nil
}

func cleanRefPath(p string) (string, error) {
	if p == "" || filepath.IsAbs(p) || strings.HasPrefix(p, "/") || strings.Contains(p, `\`) {
		return "", ErrInvalidPath
	}
	if slices.Contains(strings.Split(p, "/"), "..") {
		return "", ErrInvalidPath
	}
	rel := filepath.Clean(strings.TrimPrefix(p, "./"))
	if rel == "." || !filepath.IsLocal(rel) {
		return "", ErrInvalidPath
	}
	return rel, nil
}
