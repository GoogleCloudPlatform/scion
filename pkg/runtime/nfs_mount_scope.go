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

package runtime

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
)

// errNFSMountScope marks an agent mount refused by checkNFSMountScope.
var errNFSMountScope = errors.New("mount outside the agent's directories on NFS project storage")

// checkNFSMountScope is the mount-scope check for NFS project storage
// (ptone/scion#6943): every directory an agent container gets from the
// share is one of the project's leaves (a workspace, an agent directory or
// below, a shared dir), never a directory that holds the project
// configuration or the trees of other agents or projects.
//
// rel is the mount's path relative to the export (a Kubernetes subPath,
// a Cloud Run server path below the export, or a bind source below the
// share's host base), subPathRoot the configured subpath root ("" is the
// default) and projectID the project the agent belongs to. It refuses:
//
//   - the export root, the subpath root and every other parent of the
//     project root;
//   - any path that is not in projectID's own tree;
//   - the project root;
//   - the project configuration directory (<project>/.scion) and any path
//     under it;
//   - <project>/agents and <project>/shared-dirs, the parents of every
//     agent directory and every shared dir.
//
// It fails closed: an invalid subpath root, a missing project ID or a path
// that is not clean is refused too.
func checkNFSMountScope(rel, subPathRoot, projectID string) error {
	refuse := func(why string) error {
		return fmt.Errorf("%w: %q is %s; an agent mounts only its own directories of the project tree",
			errNFSMountScope, rel, why)
	}
	root, err := config.ResolveSubPathRoot(subPathRoot)
	if err != nil {
		return refuse(fmt.Sprintf("not checkable (%v)", err))
	}
	if projectID == "" || strings.ContainsAny(projectID, `/\`) || projectID == "." || projectID == ".." {
		return refuse(fmt.Sprintf("not checkable against project ID %q", projectID))
	}
	slashed := filepath.ToSlash(rel)
	if strings.HasPrefix(slashed, "/") || strings.ContainsRune(slashed, 0) {
		return refuse("not a relative path")
	}
	if slashed != "" && path.Clean(slashed) != slashed {
		return refuse("not a clean path")
	}
	projectRoot := path.Join(filepath.ToSlash(root), projectID)
	switch {
	case slashed == "" || slashed == ".":
		return refuse("the export root")
	case slashed == projectRoot:
		return refuse("the project root")
	case strings.HasPrefix(projectRoot, slashed+"/"):
		return refuse("a parent of the project root")
	case !strings.HasPrefix(slashed, projectRoot+"/"):
		return refuse(fmt.Sprintf("outside project %s's tree", projectID))
	}
	first, _, _ := strings.Cut(strings.TrimPrefix(slashed, projectRoot+"/"), "/")
	switch {
	case first == projectfs.ProjectConfigDirName:
		return refuse("the project configuration directory or a path under it")
	case slices.Contains([]string{"agents", "shared-dirs"}, first) && slashed == projectRoot+"/"+first:
		return refuse(fmt.Sprintf("the parent of every entry in %s/", first))
	}
	return nil
}

// nfsBindScope is the mount-scope check of the local-container runtimes'
// binds for an agent whose workspace is on NFS project storage.
type nfsBindScope struct {
	// hostBase is where the share is mounted on this host (resolved).
	hostBase    string
	subPathRoot string
	projectID   string
}

// newNFSBindScope returns the bind check for cfg, or nil when its
// workspace is not on NFS project storage. The share's host base is not
// in the run config: it is the part of the (resolved) workspace before
// <subpath root>/<project ID>, and a workspace that does not sit strictly
// inside that tree is refused.
func newNFSBindScope(cfg RunConfig) (*nfsBindScope, error) {
	if cfg.WorkspaceBackendName != "nfs" {
		return nil, nil
	}
	refuse := func(why string) error {
		return fmt.Errorf("%w: workspace %q %s", errNFSMountScope, cfg.Workspace, why)
	}
	root, err := config.ResolveSubPathRoot(cfg.NFSSubPathRoot)
	if err != nil {
		return nil, refuse(fmt.Sprintf("cannot be checked (%v)", err))
	}
	if cfg.ProjectID == "" || cfg.Workspace == "" {
		return nil, refuse(fmt.Sprintf("cannot be checked against project ID %q", cfg.ProjectID))
	}
	want := append(strings.Split(filepath.ToSlash(root), "/"), cfg.ProjectID)
	comps := strings.Split(filepath.ToSlash(filepath.Clean(cfg.Workspace)), "/")
	// The rightmost <subpath root>/<project ID> with something below it.
	for i := len(comps) - len(want) - 1; i >= 1; i-- {
		if slices.Equal(comps[i:i+len(want)], want) {
			base := strings.Join(comps[:i], "/")
			if base == "" {
				base = "/"
			}
			return &nfsBindScope{hostBase: filepath.FromSlash(base), subPathRoot: root, projectID: cfg.ProjectID}, nil
		}
	}
	return nil, refuse(fmt.Sprintf("is not inside project %s's tree on NFS project storage", cfg.ProjectID))
}

// check refuses a bind source (resolved) on the share that
// checkNFSMountScope refuses. Sources off the share are not its concern.
func (s *nfsBindScope) check(src string) error {
	if s == nil {
		return nil
	}
	rel, err := filepath.Rel(s.hostBase, src)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return nil
	}
	if rel == "." {
		rel = ""
	}
	return checkNFSMountScope(rel, s.subPathRoot, s.projectID)
}
