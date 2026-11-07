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

// Package provisioner is the layout authority's Provisioner
// (ptone/scion#5834): the hub and the brokers create a project's
// Scion-owned directories through it, so a leaf has the same mode and ACL
// whoever creates it. It wraps pkg/projectfs (the leaf contract,
// projectfs.EnsureSharedLeaf) and pkg/provision (the provisioning state
// directory, its lock and sentinel, and the clone). It is a package of its
// own so that pkg/projectfs, which configuration loaders and the relay
// import, keeps no dependency on pkg/provision or pkg/store.
//
// It builds on every platform: on a non-unix one the leaf primitives it
// calls (projectfs.EnsureSharedLeafExpecting, Resolved.EnsureLeaf) and
// provision.PrepareStateDir fail with errors.ErrUnsupported, so every
// method fails with it too (the hub and the brokers are unix-only).
package provisioner

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Provisioner creates the Scion-owned directories of a Layout.
type Provisioner struct {
	Layout projectfs.Layout
	// ProvisionShared runs the workspace provisioning; nil means
	// provision.ProvisionShared. Tests replace it.
	ProvisionShared func(provision.ProvisionInput) error
}

// ErrStorage marks a failed storage step of a provisioning (a leaf or the
// provisioning state directory could not be created or checked), as
// opposed to a failed clone. Its wrapped error may carry a host path.
var ErrStorage = errors.New("provisioner: project storage step failed")

// EnsureLeaf creates the directory r if it is missing and reports whether
// it already existed (projectfs.Resolved.EnsureLeaf: the leaf contract on a
// shared backend, the directory created as before on the local backend).
func (p Provisioner) EnsureLeaf(r projectfs.Resolved) (existed bool, err error) {
	return r.EnsureLeaf()
}

// OpenStateDir opens a project's provisioning state directory on a shared
// backend, <ProjectRoot>/provision: created with the leaf contract when
// missing (projectfs.EnsureSharedLeafExpecting with the Layout's nfs uid and
// gid as the expected owner: existing upper directories with the share's
// layout are not warned about, and nothing is chowned), checked to be a
// real directory (provision.PrepareStateDir, its ownership left as it is),
// and swept of aged lock litter
// (provision.GarbageCollectStateDir, ptone/scion#5836: every open sweeps,
// never a live lock). It returns the directory's path below the resolved
// host base.
func (p Provisioner) OpenStateDir(projectID string) (string, error) {
	r, err := p.Layout.ProvisionStateDir(projectID)
	if err != nil {
		return "", err
	}
	base, err := r.ResolvedHostBase()
	if err != nil {
		return "", err
	}
	uid, gid := provision.DefaultOwnerID(p.Layout.UID), provision.DefaultOwnerID(p.Layout.GID)
	fd, _, err := projectfs.EnsureSharedLeafExpecting(base, r.RelPath, shareddirs.Owner{UID: uid, GID: gid})
	if err != nil {
		return "", fmt.Errorf("create the provisioning state directory: %w", err)
	}
	if err := shareddirs.CloseFd(fd); err != nil {
		return "", err
	}
	dir := filepath.Join(base, r.RelPath)
	if err := provision.PrepareStateDir(dir, uid, gid, false); err != nil {
		return "", err
	}
	provision.GarbageCollectStateDir(dir)
	return dir, nil
}

// SharedWorkspace is the input of ProvisionSharedWorkspace.
type SharedWorkspace struct {
	Ctx       context.Context
	ProjectID string
	// AgentID is recorded for logs; empty for the hub's provisioning at
	// project create.
	AgentID  string
	GitClone *api.GitCloneConfig
	// CloneToken and CloneTokenRemote are provision.ProvisionInput's: the
	// token reaches only the clone command, and only for that remote.
	CloneToken       string
	CloneTokenRemote string
}

// ProvisionSharedWorkspace provisions a project's shared-plain workspace on
// a shared Layout the way the Kubernetes provisioning init container lays
// it out, for the hub (at project create) and the brokers (the backstop
// before a local container starts) alike: the workspace leaf
// (Resolve(workspace)) and the provisioning state directory (OpenStateDir)
// are created with the leaf contract, then provision.ProvisionShared runs
// with the sentinel and the lock in the state directory only, ownership
// from the Layout, a failed chown not fatal, and the clone hardened for a
// project-supplied URL. An existing sentinel means no clone.
//
// createdRoot reports whether the project root did not exist before the
// call, so a caller can remove a tree it created when provisioning fails.
// A failed storage step wraps ErrStorage; a clone failure is returned as
// provision.ProvisionShared returns it.
func (p Provisioner) ProvisionSharedWorkspace(in SharedWorkspace) (createdRoot bool, err error) {
	l := p.Layout
	if !l.Backend.Shared() {
		return false, fmt.Errorf("provisioner: shared workspace provisioning needs a shared backend, not %q", l.Backend)
	}
	r, err := l.Resolve(projectfs.Project{ID: in.ProjectID}, "", projectfs.WorkspaceMount(projectfs.ModeSharedPlain, ""))
	if err != nil {
		return false, err
	}
	pr, err := l.ProjectRoot(in.ProjectID)
	if err != nil {
		return false, err
	}
	base, err := r.ResolvedHostBase()
	if err != nil {
		return false, fmt.Errorf("%w: host base: %w", ErrStorage, err)
	}
	_, statErr := os.Lstat(filepath.Join(base, pr.RelPath))
	createdRoot = errors.Is(statErr, fs.ErrNotExist)
	if _, err := p.EnsureLeaf(r); err != nil {
		return createdRoot, fmt.Errorf("%w: workspace leaf: %w", ErrStorage, err)
	}
	stateDir, err := p.OpenStateDir(in.ProjectID)
	if err != nil {
		return createdRoot, fmt.Errorf("%w: provisioning state directory: %w", ErrStorage, err)
	}
	run := p.ProvisionShared
	if run == nil {
		run = provision.ProvisionShared
	}
	err = run(provision.ProvisionInput{
		Ctx: in.Ctx,
		Resolved: provision.ResolvedWorkspace{
			HostPath:           filepath.Join(base, r.RelPath),
			ServerRelativePath: r.RelPath,
			HostBase:           base,
			Backend:            string(l.Backend),
		},
		ProjectID:        in.ProjectID,
		AgentID:          in.AgentID,
		Mode:             store.SharingModeSharedPlain,
		GitClone:         in.GitClone,
		CloneToken:       in.CloneToken,
		CloneTokenRemote: in.CloneTokenRemote,
		HostGitHardened:  true,
		NFSUID:           provision.DefaultOwnerID(l.UID),
		NFSGID:           provision.DefaultOwnerID(l.GID),
		SentinelDir:      stateDir,
	})
	return createdRoot, err
}
