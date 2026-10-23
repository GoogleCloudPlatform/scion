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

package projectfs

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
)

// KindProjectConfig is the repository configuration entry (.scion) inside
// a project-scoped workspace (design §P1a rule 3): mounted read-only
// over the workspace it sits in, never the project root (which holds the
// hub-managed project configuration and is inside no Mount).
const KindProjectConfig Kind = "project-config"

// ProjectConfigEntryName is the name of the workspace's configuration
// entry.
const ProjectConfigEntryName = ".scion"

// ProjectConfigEntry returns where the configuration entry (.scion) of the
// resolved workspace ws lives: <ws>/.scion on the host, on the share
// (<ws subPath>/.scion) and in the container (<ws target>/.scion),
// read-only. It is the read-only Mount of #4045 (ptone/scion#5732) in the
// Mount model; whether the entry exists, and what kind of entry it is, is
// for the caller to check before realizing it.
func ProjectConfigEntry(ws Resolved) (Resolved, error) {
	if ws.Kind != "" && ws.Kind != KindWorkspace {
		return Resolved{}, fmt.Errorf("projectfs: a configuration entry lives in a workspace, not a %q mount", ws.Kind)
	}
	if ws.HostPath == "" || ws.Target == "" {
		return Resolved{}, errors.New("projectfs: the workspace has no host path or container target")
	}
	r := ws
	r.Kind, r.Name, r.ReadOnly = KindProjectConfig, ProjectConfigEntryName, true
	r.HostPath = filepath.Join(ws.HostPath, ProjectConfigEntryName)
	r.Target = path.Join(ws.Target, ProjectConfigEntryName)
	if ws.RelPath != "" {
		r.RelPath = filepath.Join(ws.RelPath, ProjectConfigEntryName)
	}
	if ws.SubPath != "" {
		r.SubPath = path.Join(ws.SubPath, ProjectConfigEntryName)
	}
	if ws.ServerPath != "" {
		r.ServerPath = path.Join(ws.ServerPath, ProjectConfigEntryName)
	}
	return r, nil
}

// RuntimeKind says how a runtime places a directory in a container.
type RuntimeKind string

const (
	// RuntimeBind: a host bind mount (Docker, Podman, Apple).
	RuntimeBind RuntimeKind = "bind"
	// RuntimePod: a Kubernetes volume mount by subPath of a claim.
	RuntimePod RuntimeKind = "pod"
)

// Realized is a resolved directory realized for one runtime. Target is
// always the Resolved's (the Mount's) container target, unchanged: callers
// never compute container targets themselves (design §P1 invariants;
// attachment paths are built from Mount.Target).
type Realized struct {
	// Source is the host path of a bind mount (RuntimeBind).
	Source string
	// ClaimName is the PersistentVolumeClaim of a pod mount (RuntimePod):
	// the share's pv_name (nfs) or claim (gke-shared-volume).
	ClaimName string
	// SubPath is the claim-relative path of a pod mount (RuntimePod).
	SubPath  string
	Target   string
	ReadOnly bool
}

// Realize places r in a container of runtime kind: a bind mount of
// r.HostPath, or (on a shared backend) a pod mount of the share's claim at
// r.SubPath. The target is r.Target and read-only follows r.
func (l Layout) Realize(r Resolved, kind RuntimeKind) (Realized, error) {
	if r.Target == "" {
		return Realized{}, errors.New("projectfs: cannot realize a mount without a container target")
	}
	out := Realized{Target: r.Target, ReadOnly: r.ReadOnly}
	switch kind {
	case RuntimeBind:
		if r.HostPath == "" {
			return Realized{}, errors.New("projectfs: cannot bind a mount without a host path")
		}
		out.Source = r.HostPath
	case RuntimePod:
		if !r.Backend.Shared() {
			return Realized{}, fmt.Errorf("projectfs: a %q mount is not on a shared claim", r.Backend)
		}
		if r.SubPath == "" || path.IsAbs(r.SubPath) {
			return Realized{}, fmt.Errorf("projectfs: invalid subPath %q", r.SubPath)
		}
		// The claim of the share r was resolved on: for a directory that
		// came through the transitional SharedDirs split it is that
		// share's, not l's.
		claim := r.ClaimName
		if claim == "" {
			claim = l.Share.PVName
		}
		out.ClaimName, out.SubPath = claim, r.SubPath
	default:
		return Realized{}, fmt.Errorf("projectfs: unknown runtime kind %q", kind)
	}
	return out, nil
}
