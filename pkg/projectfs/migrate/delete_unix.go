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

package migrate

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
)

// DeleteLegacyProjectTrees removes, at project delete on a shared backend,
// the slug-keyed legacy trees of the deleted project (ptone/scion#5835):
//
//   - on the share, the retired hub-projects tree <LegacyHubProjectsRoot>/
//     <slug> and its <slug>.migrated-<ts> copies;
//   - when globalDir is set (the host's ~/.scion), <globalDir>/projects/
//     <slug> and its <slug>.migrated-<ts> copies.
//
// A tree is removed only when the identity markers it carries name
// projectID (the migration's identity binding: .scion/project-id and its
// pre-rename name, the project ID keys of .scion/settings.yaml, and, for
// the host-local tree itself but not its copies, the workspace records). A tree that carries no marker
// or names another project is kept (OutcomeKept, with the markers read as
// the reason): a slug alone never deletes anything, since a slug can be reused by
// another project. The removal is shareddirs.DeleteTree's confined walk
// (no symlink followed at any component).
//
// On a Layout that is not shared it does nothing: the local backend's
// slug-keyed trees keep their existing deleters.
func DeleteLegacyProjectTrees(l projectfs.Layout, globalDir, slug, projectID string) ([]projectfs.Deletion, error) {
	if !shareddirs.ValidProjectID(projectID) {
		return nil, fmt.Errorf("invalid project ID %q", projectID)
	}
	if slug == "" || api.Slugify(slug) != slug {
		return nil, fmt.Errorf("invalid project slug %q", slug)
	}
	if !l.Backend.Shared() {
		return nil, nil
	}
	type root struct {
		tree  Tree
		kind  projectfs.TreeKind
		local bool
	}
	var roots []root
	hubProjects, err := LegacyHubProjectsRoot(l)
	if err != nil {
		return nil, err
	}
	roots = append(roots, root{tree: hubProjects, kind: projectfs.TreeLegacyHubProjects})
	if globalDir != "" {
		if !filepath.IsAbs(globalDir) {
			return nil, fmt.Errorf("global dir %q is not absolute", globalDir)
		}
		roots = append(roots, root{tree: Tree{Base: globalDir, Rel: "projects"}, kind: projectfs.TreeLegacyLocalProject, local: true})
	}

	var out []projectfs.Deletion
	for _, r := range roots {
		names, ok, err := listDir(r.tree)
		if err != nil {
			out = append(out, projectfs.Deletion{Tree: r.kind, Path: r.tree.String(), Outcome: projectfs.OutcomeFailed, Err: err})
			continue
		}
		if !ok {
			continue
		}
		for _, name := range legacyTreeNames(names, slug) {
			t := Tree{Base: r.tree.Base, Rel: filepath.Join(r.tree.Rel, name)}
			// The host's workspace records are keyed by the live slug, not
			// by a copy's name: a .migrated-<ts> copy is identified by the
			// markers inside it only.
			local := r.local && name == slug
			out = append(out, deleteLegacyTree(r.kind, t, local, globalDir, slug, projectID))
		}
	}
	return out, nil
}

// legacyTreeNames returns, sorted, the entries of names that are slug's
// legacy tree or one of its .migrated-<ts> copies.
func legacyTreeNames(names []string, slug string) []string {
	var out []string
	for _, n := range names {
		if n == slug {
			out = append(out, n)
			continue
		}
		ts, ok := strings.CutPrefix(n, slug+MigratedSuffix)
		if !ok {
			continue
		}
		if _, err := time.Parse(migratedTimeFormat, ts); err == nil {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// deleteLegacyTree removes t when its identity markers name projectID.
func deleteLegacyTree(kind projectfs.TreeKind, t Tree, local bool, globalDir, slug, projectID string) projectfs.Deletion {
	d := projectfs.Deletion{Tree: kind, Path: t.String()}
	k, err := entryKind(t)
	if err != nil {
		d.Outcome, d.Err = projectfs.OutcomeFailed, err
		return d
	}
	if k != kindDir {
		d.Outcome, d.Reason = projectfs.OutcomeKept, "not a directory (a symlink is never followed)"
		return d
	}
	id, detail, err := treeIdentity(t, local, globalDir, slug, projectID)
	if err != nil {
		d.Outcome, d.Err = projectfs.OutcomeFailed, err
		return d
	}
	if id != identityVerified {
		d.Outcome, d.Reason = projectfs.OutcomeKept, "identity markers do not name the project: "+detail
		return d
	}
	existed, err := shareddirs.DeleteTree(t.Base, t.Rel)
	switch {
	case err != nil:
		d.Outcome, d.Err = projectfs.OutcomeFailed, err
	case !existed:
		d.Outcome = projectfs.OutcomeAbsent
	default:
		d.Outcome, d.Reason = projectfs.OutcomeDeleted, detail
	}
	return d
}
