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
	"path/filepath"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/agenthome"
)

// ErrTemplateRefNotAllowed is wrapped by the error for a template reference
// that a project-scoped lookup does not accept: an absolute directory outside
// the registered template hydration roots, or a remote source. The message
// never contains the reference.
var ErrTemplateRefNotAllowed = errors.New("template reference not allowed")

var (
	hydrationRootsMu sync.RWMutex
	hydrationRoots   []string
)

// SetTemplateHydrationRoots replaces the directories under which a
// project-scoped template lookup (FindTemplateInProjectPath with a project
// path) accepts an absolute template directory. A Runtime Broker registers
// the directories it writes hydrated templates to itself: its template cache
// and, when co-located with the Hub, the Hub's local storage directory. They
// are broker-owned and never inside a project or agent directory. Empty and
// relative entries are ignored. It returns the previous roots, so a caller
// (or a test) can restore them.
func SetTemplateHydrationRoots(roots ...string) []string {
	var clean []string
	for _, r := range roots {
		if r == "" || !filepath.IsAbs(r) {
			continue
		}
		clean = append(clean, filepath.Clean(r))
	}
	hydrationRootsMu.Lock()
	defer hydrationRootsMu.Unlock()
	prev := hydrationRoots
	hydrationRoots = clean
	return prev
}

// AddTemplateHydrationRoot registers one more hydration root (see
// SetTemplateHydrationRoots). An empty, relative or already registered root
// is ignored.
func AddTemplateHydrationRoot(root string) {
	if root == "" || !filepath.IsAbs(root) {
		return
	}
	root = filepath.Clean(root)
	hydrationRootsMu.Lock()
	defer hydrationRootsMu.Unlock()
	for _, r := range hydrationRoots {
		if r == root {
			return
		}
	}
	hydrationRoots = append(hydrationRoots, root)
}

// TemplateHydrationRoots returns the registered hydration roots.
func TemplateHydrationRoots() []string {
	hydrationRootsMu.RLock()
	defer hydrationRootsMu.RUnlock()
	return append([]string(nil), hydrationRoots...)
}

// templateRefNotAllowed returns the error for a refused template reference.
func templateRefNotAllowed(reason string) error {
	return fmt.Errorf("%w: %s", ErrTemplateRefNotAllowed, reason)
}

// hydratedTemplateDir returns the Template for dir, an absolute directory,
// when it lies below a registered hydration root and is reached from that
// root without following a symbolic link (agenthome.OpenBelow). Otherwise it
// returns an error wrapping ErrTemplateRefNotAllowed, or one satisfying
// fs.ErrNotExist when dir is under a root but missing.
func hydratedTemplateDir(dir string) (*Template, error) {
	dir = filepath.Clean(dir)
	for _, root := range TemplateHydrationRoots() {
		rel, err := filepath.Rel(root, dir)
		if err != nil || rel == "." || !filepath.IsLocal(rel) {
			continue
		}
		h, err := agenthome.OpenBelow(root, rel)
		if err != nil {
			if errors.Is(err, agenthome.ErrLinkComponent) || errors.Is(err, agenthome.ErrNotDirectory) {
				return nil, templateRefNotAllowed("a directory on the path to the hydrated template is a symbolic link or not a directory")
			}
			// Typed not-found with a display-safe Name (FriendlyTemplateName:
			// the base name, or "" for a content hash) and fixed text: the
			// broker's cache path is never put in the message.
			return nil, NewTemplateNotFoundError(dir, "hydrated template directory not found")
		}
		_ = h.Close()
		return templateBelow(templateNameFromDir(dir), root, rel, ""), nil
	}
	return nil, templateRefNotAllowed("an absolute template directory is accepted only from the broker's own template cache")
}
