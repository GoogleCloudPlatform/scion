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
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/knadh/koanf/v2"
	yamlv3 "gopkg.in/yaml.v3"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
)

// Settings keys of the project storage block.
const (
	ProjectStorageKey       = "server.project_storage"
	ProjectStorageUIDKey    = "server.project_storage.nfs.uid"
	ProjectStorageGIDKey    = "server.project_storage.nfs.gid"
	projectStorageHomesKey  = "server.project_storage.homes"
	projectStorageOptsKey   = "server.project_storage.homes_options"
	workspaceStorageKey     = "server.workspace_storage"
	sharedDirStorageKey     = "server.shared_dir_storage"
	homeStorageKey          = "server.home_storage"
	removedOverrideRemedyFn = "server.project_storage decides where every agent's workspace, shared dirs and home live; " +
		"for a node-local directory use a volumes: entry instead"
)

// V1ProjectStorageConfig is server.project_storage (design fs-cleanup
// §P2): the one storage block of a hub and its brokers. Backend
// places the workspace and the shared dirs of every project (one value for
// both); Homes is the only per-location choice. It replaces
// server.workspace_storage, server.shared_dir_storage and
// server.home_storage, which are synthesized into it while they remain
// (ResolveProjectStorage).
//
// Like the blocks it replaces it is Layer-0: read from the global settings
// file (and the hub's SCION_SERVER_* environment) at startup, never from
// the DB and never from a project's own settings.
type V1ProjectStorageConfig struct {
	// Backend is "", "local", "nfs", "cloudrun-volume" or
	// "gke-shared-volume"; "" means "local".
	Backend string `json:"backend,omitempty" yaml:"backend,omitempty" koanf:"backend"`
	// Homes is "", "local" or "nfs". "" and "local" keep agent homes
	// node-local (in the pod, or on the broker host); "nfs" places them on
	// the backend's share and needs backend "nfs".
	Homes           string                   `json:"homes,omitempty" yaml:"homes,omitempty" koanf:"homes"`
	NFS             *V1NFSConfig             `json:"nfs,omitempty" yaml:"nfs,omitempty" koanf:"nfs"`
	CloudRunVolume  *V1CloudRunVolumeConfig  `json:"cloudrun_volume,omitempty" yaml:"cloudrun_volume,omitempty" koanf:"cloudrun_volume"`
	GKESharedVolume *V1GKESharedVolumeConfig `json:"gke_shared_volume,omitempty" yaml:"gke_shared_volume,omitempty" koanf:"gke_shared_volume"`
	// HomesOptions are the operational knobs of server.home_storage, with
	// unchanged meaning.
	HomesOptions *V1HomesOptionsConfig `json:"homes_options,omitempty" yaml:"homes_options,omitempty" koanf:"homes_options"`

	// LegacySharedDirsNFS is not a setting (it has no key): it is set only
	// by SynthesizeProjectStorage, for a hub still on the deprecated blocks
	// whose shared dirs are on server.shared_dir_storage's nfs export while
	// its workspaces are not on nfs. Shared dirs (and nfs homes) then stay
	// on that export and workspaces stay where they are: a transitional
	// split that keeps today's placement until the operator writes
	// server.project_storage (ptone/scion#5821). It cannot be
	// configured; phase 6 removes it with the old blocks.
	LegacySharedDirsNFS *V1NFSConfig `json:"-" yaml:"-" koanf:"-"`
	// LegacySharedDirsLocal is not a setting either: set only by
	// SynthesizeProjectStorage for a hub whose deprecated workspace_storage
	// is cloudrun-volume or gke-shared-volume with no shared_dir_storage.
	// Shared dirs then stay where they are today (per-dir claims on
	// Kubernetes, node-local elsewhere) until server.project_storage is
	// written; writing it moves them onto the volume and leaves the per-dir
	// claims' contents behind (ptone/scion#5932).
	LegacySharedDirsLocal bool `json:"-" yaml:"-" koanf:"-"`
}

// V1HomesOptionsConfig is server.project_storage.homes_options. Each field
// has the meaning of the server.home_storage field of the same name.
type V1HomesOptionsConfig struct {
	Leaf                   string `json:"leaf,omitempty" yaml:"leaf,omitempty" koanf:"leaf"`
	StopGraceSeconds       int    `json:"stop_grace_seconds,omitempty" yaml:"stop_grace_seconds,omitempty" koanf:"stop_grace_seconds"`
	TerminationWaitSeconds int    `json:"termination_wait_seconds,omitempty" yaml:"termination_wait_seconds,omitempty" koanf:"termination_wait_seconds"`
	SkeletonMaxBytes       int64  `json:"skeleton_max_bytes,omitempty" yaml:"skeleton_max_bytes,omitempty" koanf:"skeleton_max_bytes"`
	AllowIncompletePhases  bool   `json:"allow_incomplete_phases,omitempty" yaml:"allow_incomplete_phases,omitempty" koanf:"allow_incomplete_phases"`
}

// EffectiveBackend returns Backend, or "local" when it is empty or p is nil.
func (p *V1ProjectStorageConfig) EffectiveBackend() string {
	if p == nil || p.Backend == "" {
		return WorkspaceStorageBackendLocal
	}
	return p.Backend
}

// WorkspaceStorage returns the workspace view of p: a
// V1WorkspaceStorageConfig with p's backend and backend blocks (shared,
// not copied), for the code that still consumes that type. Nil when p is
// nil.
func (p *V1ProjectStorageConfig) WorkspaceStorage() *V1WorkspaceStorageConfig {
	if p == nil {
		return nil
	}
	return &V1WorkspaceStorageConfig{
		Backend:         p.Backend,
		NFS:             p.NFS,
		CloudRunVolume:  p.CloudRunVolume,
		GKESharedVolume: p.GKESharedVolume,
	}
}

// HomeStorage returns the home view of p: a V1HomeStorageConfig whose
// Backend is Homes and whose other fields are HomesOptions. Nil when p is
// nil or sets neither.
func (p *V1ProjectStorageConfig) HomeStorage() *V1HomeStorageConfig {
	if p == nil || (p.Homes == "" && p.HomesOptions == nil) {
		return nil
	}
	h := &V1HomeStorageConfig{Backend: p.Homes}
	if o := p.HomesOptions; o != nil {
		h.Leaf = o.Leaf
		h.StopGraceSeconds = o.StopGraceSeconds
		h.TerminationWaitSeconds = o.TerminationWaitSeconds
		h.SkeletonMaxBytes = o.SkeletonMaxBytes
		h.AllowIncompletePhases = o.AllowIncompletePhases
	}
	return h
}

// ApplyDefaults fills the defaults of the block Backend selects, as
// ApplyWorkspaceStorageDefaults does for server.workspace_storage.
// Idempotent.
func (p *V1ProjectStorageConfig) ApplyDefaults() {
	if p == nil {
		return
	}
	ws := p.WorkspaceStorage()
	ws.ApplyWorkspaceStorageDefaults()
	p.NFS, p.CloudRunVolume, p.GKESharedVolume = ws.NFS, ws.CloudRunVolume, ws.GKESharedVolume
}

// Validate returns an error if the block cannot be used: anything
// ValidateWorkspaceStorage rejects for the backend (reported under
// server.project_storage), an unknown homes value, homes "nfs" without
// backend "nfs", or an invalid homes option. A nil block is valid (the
// local backend). Call after ApplyDefaults.
func (p *V1ProjectStorageConfig) Validate() error {
	if p == nil {
		return nil
	}
	if err := p.WorkspaceStorage().validateWorkspaceStorage(ProjectStorageKey, ProjectStorageUIDKey, ProjectStorageGIDKey); err != nil {
		return err
	}
	if err := checkHomeStorageBackend(p.Homes); err != nil {
		return fmt.Errorf("%s %w", projectStorageHomesKey, err)
	}
	if p.Homes == HomeStorageBackendNFS && p.Backend != WorkspaceStorageBackendNFS && p.LegacySharedDirsNFS == nil {
		return fmt.Errorf("%s is \"nfs\" but %s.backend is %q; homes on nfs use the backend's share, so set backend to \"nfs\"",
			projectStorageHomesKey, ProjectStorageKey, p.EffectiveBackend())
	}
	if o := p.HomesOptions; o != nil {
		opts := &V1HomeStorageConfig{
			Leaf:                   o.Leaf,
			StopGraceSeconds:       o.StopGraceSeconds,
			TerminationWaitSeconds: o.TerminationWaitSeconds,
			SkeletonMaxBytes:       o.SkeletonMaxBytes,
		}
		if err := opts.validate(projectStorageOptsKey, "backend"); err != nil {
			return err
		}
	}
	return nil
}

// ResolveProjectStorage returns the project storage block a process uses
// (design migration M1), with startup warnings. srv is the server
// section of the global settings.
//
//   - server.project_storage set: it is the block, after defaults and
//     Validate. Setting it together with any of the blocks it replaces is
//     an error: exactly one storage block configures a hub.
//   - otherwise the block is synthesized from server.workspace_storage
//     (backend and backend blocks) and server.home_storage (backend ->
//     homes, the rest -> homes_options), with a warning that prints the
//     block to write. Shared dirs on server.shared_dir_storage's nfs export
//     stay there even when the workspace is not on nfs (LegacySharedDirsNFS,
//     a transitional split); otherwise shared dirs follow the workspace
//     backend, as one backend places both. Only the genuinely ambiguous cases are errors:
//     two nfs blocks naming different trees, or nfs homes with no nfs share.
//   - nothing set: nil (the local backend).
func ResolveProjectStorage(srv *V1ServerConfig) (*V1ProjectStorageConfig, []string, error) {
	if srv == nil {
		return nil, nil, nil
	}
	return SynthesizeProjectStorage(srv.ProjectStorage, srv.WorkspaceStorage, srv.SharedDirStorage, srv.HomeStorage)
}

// SynthesizeProjectStorage implements ResolveProjectStorage over the four
// blocks. It never modifies its arguments.
func SynthesizeProjectStorage(ps *V1ProjectStorageConfig, ws *V1WorkspaceStorageConfig, sd *V1SharedDirStorageConfig, hs *V1HomeStorageConfig) (*V1ProjectStorageConfig, []string, error) {
	var legacy []string
	if ws != nil && !isZeroWorkspaceStorage(ws) {
		legacy = append(legacy, workspaceStorageKey)
	}
	if sd != nil && (sd.Backend != "" || sd.NFS != nil) {
		legacy = append(legacy, sharedDirStorageKey)
	}
	if hs != nil && *hs != (V1HomeStorageConfig{}) {
		legacy = append(legacy, homeStorageKey)
	}

	if ps != nil {
		if len(legacy) > 0 {
			return nil, nil, fmt.Errorf("%s replaces %s; remove %s (exactly one storage block may be set)",
				ProjectStorageKey, strings.Join([]string{workspaceStorageKey, sharedDirStorageKey, homeStorageKey}, ", "),
				strings.Join(legacy, " and "))
		}
		out := cloneProjectStorage(ps)
		out.ApplyDefaults()
		if err := out.Validate(); err != nil {
			return nil, nil, err
		}
		return out, nil, nil
	}
	if len(legacy) == 0 {
		return nil, nil, nil
	}

	out := &V1ProjectStorageConfig{}
	if ws != nil {
		wsCopy := *ws
		if wsCopy.NFS != nil {
			nfs := *wsCopy.NFS
			nfs.Shares = append([]V1NFSShare(nil), ws.NFS.Shares...)
			wsCopy.NFS = &nfs
		}
		wsCopy.ApplyWorkspaceStorageDefaults()
		if err := wsCopy.ValidateWorkspaceStorage(); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", workspaceStorageKey, err)
		}
		out.Backend, out.NFS, out.CloudRunVolume, out.GKESharedVolume = wsCopy.Backend, wsCopy.NFS, wsCopy.CloudRunVolume, wsCopy.GKESharedVolume
	}
	if hs != nil {
		if err := hs.Validate(); err != nil {
			return nil, nil, err
		}
		out.Homes = hs.Backend
		opts := V1HomesOptionsConfig{
			Leaf:                   hs.Leaf,
			StopGraceSeconds:       hs.StopGraceSeconds,
			TerminationWaitSeconds: hs.TerminationWaitSeconds,
			SkeletonMaxBytes:       hs.SkeletonMaxBytes,
			AllowIncompletePhases:  hs.AllowIncompletePhases,
		}
		if opts != (V1HomesOptionsConfig{}) {
			out.HomesOptions = &opts
		}
	}

	if sd != nil {
		if err := sd.Validate(); err != nil {
			return nil, nil, err
		}
	}
	var warnings []string
	if sd != nil && sd.Backend == WorkspaceStorageBackendNFS {
		w, err := placeLegacySharedDirs(out, sd)
		if err != nil {
			return nil, nil, err
		}
		warnings = append(warnings, w...)
	} else if (sd == nil || sd.Backend == "" || sd.Backend == WorkspaceStorageBackendLocal) &&
		(out.Backend == WorkspaceStorageBackendCloudRunVolume || out.Backend == WorkspaceStorageBackendGKESharedVolume) {
		// No shared_dir_storage, or an explicit local one: the shared dirs
		// are not on the volume today, so they stay where they are.
		out.LegacySharedDirsLocal = true
		warnings = append(warnings, fmt.Sprintf("%s is not set to nfs and the workspace is on %s: shared dirs stay where they are today "+
			"(per-dir claims on Kubernetes) until %s is written. Writing it moves shared dirs onto the volume; "+
			"the contents of the existing per-dir claims are NOT moved automatically (copy them first)",
			sharedDirStorageKey, out.Backend, ProjectStorageKey))
	} else if sd == nil && out.Backend == WorkspaceStorageBackendNFS {
		warnings = append(warnings, fmt.Sprintf("%s is not set and the workspace is on nfs: shared dirs now follow the workspace onto the nfs export "+
			"on every runtime (Docker, Podman and Apple agents kept them node-local before). Existing data: %s",
			sharedDirStorageKey, ProjectStorageMigrationText))
	} else if sd != nil && sd.Backend == WorkspaceStorageBackendLocal && out.Backend == WorkspaceStorageBackendNFS {
		warnings = append(warnings, fmt.Sprintf("%s.backend is \"local\" but the workspace is on nfs: "+
			"shared dirs are now placed on the nfs export with the workspace. Existing data: %s",
			sharedDirStorageKey, ProjectStorageMigrationText))
	}
	if out.Homes == HomeStorageBackendNFS && out.Backend != WorkspaceStorageBackendNFS && out.LegacySharedDirsNFS == nil {
		return nil, nil, fmt.Errorf("%s.backend is \"nfs\" but no nfs share is configured (%s and %s are not nfs): "+
			"agent homes on nfs need the share they are on; write %s with backend \"nfs\" and that share",
			homeStorageKey, workspaceStorageKey, sharedDirStorageKey, ProjectStorageKey)
	}
	if err := out.Validate(); err != nil {
		return nil, nil, err
	}
	warnings = append([]string{legacyStorageWarning(legacy, out)}, warnings...)
	return out, warnings, nil
}

// placeLegacySharedDirs keeps shared dirs where server.shared_dir_storage
// (nfs) puts them today. When the workspace is on the same nfs tree
// nothing is split. When the workspace is not on nfs, the shared dirs stay
// on the shared_dir_storage export through LegacySharedDirsNFS (a
// transitional split, never configurable). Two nfs blocks that name
// different trees cannot be synthesized without choosing which tree
// vanishes: that is an error.
func placeLegacySharedDirs(ps *V1ProjectStorageConfig, sd *V1SharedDirStorageConfig) ([]string, error) {
	sdLayout, err := NFSLayout(sd.NFS)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", sharedDirStorageKey, err)
	}
	if ps.Backend != WorkspaceStorageBackendNFS {
		nfs := *sd.NFS
		nfs.Shares = append([]V1NFSShare(nil), sd.NFS.Shares...)
		nfs.SubPathRoot = sdLayout.SubPathRoot
		// The owner ids default as the workspace block's do
		// (V1WorkspaceStorageConfig.ApplyNFSDefaults), so the shared-dir
		// leaves a root creator makes get the nfs owner in this split too.
		if nfs.UID == 0 {
			nfs.UID = 1000
		}
		if nfs.GID == 0 {
			nfs.GID = 1000
		}
		ps.LegacySharedDirsNFS = &nfs
		return []string{fmt.Sprintf("%s is nfs but the workspace is %q (from %s): shared dirs stay on the shared_dir_storage export "+
			"and workspaces stay where they are until %s is written. Writing it with backend \"nfs\" moves workspaces onto the export too "+
			"(one backend for workspaces and shared dirs). Existing data, afterwards: %s",
			sharedDirStorageKey, ps.EffectiveBackend(), workspaceStorageKey, ProjectStorageKey, ProjectStorageMigrationText)}, nil
	}
	psLayout, err := NFSLayout(ps.NFS)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", workspaceStorageKey, err)
	}
	// One tree is one server, export and subpath root. The share id and
	// mount_root are this host's naming for its mount of it (as
	// projectfs.Layout.DescriptorDiff treats them), so they may differ.
	if sdLayout.SubPathRoot != psLayout.SubPathRoot || sdLayout.Share.Server != psLayout.Share.Server ||
		api.NormalizeExportPath(sdLayout.Share.Export) != api.NormalizeExportPath(psLayout.Share.Export) {
		return nil, fmt.Errorf("%s and %s are both nfs but name different trees (workspace: share %q %s:%s, subpath_root %q; "+
			"shared dirs: share %q %s:%s, subpath_root %q): one storage block places both, and choosing one would hide the other's data; "+
			"move one tree onto the other's share, then write %s naming it",
			workspaceStorageKey, sharedDirStorageKey,
			psLayout.Share.ID, psLayout.Share.Server, psLayout.Share.Export, psLayout.SubPathRoot,
			sdLayout.Share.ID, sdLayout.Share.Server, sdLayout.Share.Export, sdLayout.SubPathRoot,
			ProjectStorageKey)
	}
	if sdLayout.HostBase != psLayout.HostBase {
		// One tree, named differently on this host: shared dirs are now
		// reached through the workspace block's mount.
		w := fmt.Sprintf("%s and %s name the same NFS tree but mount it differently on this host "+
			"(shared dirs: %q, workspace: %q): shared dirs are now reached at the workspace block's mount, %q",
			sharedDirStorageKey, workspaceStorageKey, sdLayout.HostBase, psLayout.HostBase, psLayout.HostBase)
		if !filepath.IsAbs(psLayout.HostBase) {
			w += fmt.Sprintf("; that path is not absolute (%s.nfs.mount_root is empty or relative), so shared dirs cannot be served from it until it is set", workspaceStorageKey)
		}
		return []string{w}, nil
	}
	return nil, nil
}

// legacyStorageWarning is the startup (and health/admin) warning of a
// synthesized block: which deprecated blocks are in use, and the exact
// server.project_storage block to write instead, so the operator's fix is a
// paste. For a split synthesis the suggested block is backend "nfs" on the
// shared-dir export, which also moves workspaces there.
func legacyStorageWarning(legacy []string, ps *V1ProjectStorageConfig) string {
	return fmt.Sprintf("legacy storage blocks in use (%s); write server.project_storage instead and check the file with scion config validate. "+
		"Existing data the new block places elsewhere: %s. The block to write:\n%s",
		strings.Join(legacy, ", "), ProjectStorageMigrationText, SuggestedProjectStorageYAML(ps))
}

// SuggestedProjectStorageYAML renders the server.project_storage block an
// operator should write for ps (indented under server:). A split synthesis
// (LegacySharedDirsNFS) suggests backend "nfs" on the shared-dir export.
func SuggestedProjectStorageYAML(ps *V1ProjectStorageConfig) string {
	if ps == nil {
		return ""
	}
	out := cloneProjectStorage(ps)
	if out.LegacySharedDirsNFS != nil {
		out.Backend, out.NFS = WorkspaceStorageBackendNFS, out.LegacySharedDirsNFS
		out.CloudRunVolume, out.GKESharedVolume = nil, nil
		out.LegacySharedDirsNFS = nil
	}
	data, err := yamlv3.Marshal(map[string]interface{}{"server": map[string]interface{}{"project_storage": out}})
	if err != nil {
		return ""
	}
	return string(data)
}

func isZeroWorkspaceStorage(ws *V1WorkspaceStorageConfig) bool {
	return ws.Backend == "" && ws.NFS == nil && ws.CloudRunVolume == nil && ws.GKESharedVolume == nil
}

// cloneProjectStorage returns a copy of p that shares no pointers with it.
func cloneProjectStorage(p *V1ProjectStorageConfig) *V1ProjectStorageConfig {
	out := *p
	if p.NFS != nil {
		nfs := *p.NFS
		nfs.Shares = append([]V1NFSShare(nil), p.NFS.Shares...)
		out.NFS = &nfs
	}
	if p.CloudRunVolume != nil {
		v := *p.CloudRunVolume
		out.CloudRunVolume = &v
	}
	if p.GKESharedVolume != nil {
		v := *p.GKESharedVolume
		out.GKESharedVolume = &v
	}
	if p.HomesOptions != nil {
		v := *p.HomesOptions
		out.HomesOptions = &v
	}
	if p.LegacySharedDirsNFS != nil {
		nfs := *p.LegacySharedDirsNFS
		nfs.Shares = append([]V1NFSShare(nil), p.LegacySharedDirsNFS.Shares...)
		out.LegacySharedDirsNFS = &nfs
	}
	return &out
}

// RemovedStorageOverrideErrors reports every per-runtime, per-profile and
// per-instance storage backend override (design: no per-runtime storage overrides):
// shared_dir_storage_backend, shared_dir_storage_backends,
// home_storage_backend and home_storage_leaf. They are removed: each
// error names server.project_storage and volumes: as the replacement.
// instancePrefix entries map a settings path prefix (for example
// "server.broker.instances[0].runtime_target") to its target. Results are
// sorted by path.
func RemovedStorageOverrideErrors(runtimes map[string]V1RuntimeConfig, profiles map[string]V1ProfileConfig, targets map[string]V1RuntimeTargetConfig) []ValidationError {
	var errs []ValidationError
	check := func(prefix, sdBackend string, sdBackends map[string]string, homeBackend, homeLeaf string) {
		add := func(key string) {
			errs = append(errs, ValidationError{
				Path:    prefix + "." + key,
				Message: key + " is no longer supported: " + removedOverrideRemedyFn,
			})
		}
		if sdBackend != "" {
			add("shared_dir_storage_backend")
		}
		if len(sdBackends) > 0 {
			add("shared_dir_storage_backends")
		}
		if homeBackend != "" {
			add("home_storage_backend")
		}
		if homeLeaf != "" {
			add("home_storage_leaf")
		}
	}
	for name, rt := range runtimes {
		check("runtimes."+name, rt.SharedDirStorageBackend, rt.SharedDirStorageBackends, rt.HomeStorageBackend, rt.HomeStorageLeaf)
	}
	for name, p := range profiles {
		check("profiles."+name, p.SharedDirStorageBackend, p.SharedDirStorageBackends, p.HomeStorageBackend, p.HomeStorageLeaf)
	}
	for prefix, t := range targets {
		check(prefix, t.SharedDirStorageBackend, t.SharedDirStorageBackends, t.HomeStorageBackend, t.HomeStorageLeaf)
	}
	sort.Slice(errs, func(i, j int) bool { return errs[i].Path < errs[j].Path })
	return errs
}

// ResolveGlobalProjectStorage is ResolveProjectStorage over a hub's loaded
// GlobalConfig (after the SCION_SERVER_* overlay).
func ResolveGlobalProjectStorage(gc *GlobalConfig) (*V1ProjectStorageConfig, []string, error) {
	if gc == nil {
		return nil, nil, nil
	}
	return SynthesizeProjectStorage(gc.ProjectStorage, gc.WorkspaceStorage, gc.SharedDirStorage, gc.HomeStorage)
}

// ValidateStorageBlocks is the settings-file check (ValidateSettings) of
// the storage blocks: server.project_storage set with any block it
// replaces, an invalid server.project_storage, and the synthesis errors of
// SynthesizeProjectStorage (two nfs blocks naming different trees, nfs
// homes with no nfs share). The deprecated blocks' own per-block value
// checks are left to the process that uses them, as before.
func ValidateStorageBlocks(ps *V1ProjectStorageConfig, ws *V1WorkspaceStorageConfig, sd *V1SharedDirStorageConfig, hs *V1HomeStorageConfig) error {
	if ps == nil {
		if ws != nil {
			wsCopy := *ws
			if ws.NFS != nil {
				nfs := *ws.NFS
				wsCopy.NFS = &nfs
			}
			wsCopy.ApplyWorkspaceStorageDefaults()
			if wsCopy.ValidateWorkspaceStorage() != nil {
				return nil
			}
		}
		if sd != nil && sd.Validate() != nil {
			return nil
		}
		if hs != nil && hs.Validate() != nil {
			return nil
		}
	}
	_, _, err := SynthesizeProjectStorage(ps, ws, sd, hs)
	return err
}

// EffectiveWorkspaceStorage is the workspace storage this server section
// configures: server.project_storage's workspace view when that block is
// set, else the deprecated server.workspace_storage. Every reader of
// workspace placement goes through it, so a settings file with only
// server.project_storage places workspaces exactly as the equivalent
// workspace_storage block did.
func (s *V1ServerConfig) EffectiveWorkspaceStorage() *V1WorkspaceStorageConfig {
	if s == nil {
		return nil
	}
	if s.ProjectStorage != nil {
		return s.ProjectStorage.WorkspaceStorage()
	}
	return s.WorkspaceStorage
}

// EffectiveSharedDirStorage is the shared-dir storage this server section
// configures: with server.project_storage, an nfs backend gives shared dirs
// on its nfs block and any other backend gives nil (the local layout, until
// the agent manager resolves shared dirs through the Layout); without it,
// the deprecated server.shared_dir_storage.
func (s *V1ServerConfig) EffectiveSharedDirStorage() *V1SharedDirStorageConfig {
	if s == nil {
		return nil
	}
	if ps := s.ProjectStorage; ps != nil {
		if ps.Backend == WorkspaceStorageBackendNFS {
			return &V1SharedDirStorageConfig{Backend: WorkspaceStorageBackendNFS, NFS: ps.NFS}
		}
		return nil
	}
	return s.SharedDirStorage
}

// EffectiveHomeStorage is the home storage this server section configures:
// server.project_storage's home view when that block is set, else the
// deprecated server.home_storage.
func (s *V1ServerConfig) EffectiveHomeStorage() *V1HomeStorageConfig {
	if s == nil {
		return nil
	}
	if s.ProjectStorage != nil {
		return s.ProjectStorage.HomeStorage()
	}
	return s.HomeStorage
}

// storageSettingKeys are the settings keys of the storage blocks, as they
// appear in a settings file.
var storageSettingKeys = []string{"project_storage", "workspace_storage", "shared_dir_storage", "home_storage"}

// CheckStorageBlocksLoaded fails closed when the global settings file
// (content data; check false skips the check) holds a storage block that
// the loader did not carry into gc, whatever the file's format: for example
// a block that is not under server:, a missing schema_version, or a server
// section that fails to decode. Running on the local backend instead would
// put project data on the wrong disk. Keys are matched as parsed mapping
// keys, so a comment, or a key that only contains a storage key's name
// (shared_dir_storage_class), never triggers it. A file that does not parse
// at all is refused when its bytes mention a storage key.
func CheckStorageBlocksLoaded(gc *GlobalConfig, check bool, data []byte) error {
	if gc != nil && (gc.ProjectStorage != nil || gc.WorkspaceStorage != nil || gc.SharedDirStorage != nil || gc.HomeStorage != nil) {
		return nil
	}
	if !check || len(data) == 0 {
		return nil
	}
	var doc interface{}
	if err := unmarshalSettingsYAML(data, &doc); err != nil {
		for _, key := range storageSettingKeys {
			if strings.Contains(string(data), key) {
				return fmt.Errorf("the global settings file cannot be parsed and mentions %s; refusing to fall back to the local backend", key)
			}
		}
		return nil
	}
	if key := findMappingKey(doc, storageSettingKeys); key != "" {
		return fmt.Errorf("the global settings file has a %s block that was not loaded; "+
			"check that the file has schema_version: \"1\", that the block is under server: and that its fields decode; refusing to fall back to the local backend", key)
	}
	return nil
}

// CheckGlobalStorageBlocksLoaded is CheckStorageBlocksLoaded for the global
// settings file of this process, for any file format: a parsed storage
// block key whose block did not load refuses (for example a schema_version
// "1" file whose server section failed typed decoding, or a legacy-format
// file with a misplaced block). Comments and keys that only contain a
// storage key's name never count.
func CheckGlobalStorageBlocksLoaded(gc *GlobalConfig) error {
	data, ok := readGlobalSettingsFile()
	if !ok {
		return nil
	}
	return CheckStorageBlocksLoaded(gc, true, data)
}

// readGlobalSettingsFile returns the content of this process's global
// settings file, or false when there is none or it cannot be read.
func readGlobalSettingsFile() ([]byte, bool) {
	globalDir, err := GetGlobalDir()
	if err != nil {
		return nil, false
	}
	path := GetSettingsPath(globalDir)
	if path == "" {
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	return data, true
}

// GlobalSettingsStorageKey returns the first of keys that the global
// settings file holds as a parsed mapping key (anywhere in the document),
// or "" when it holds none, cannot be read, or does not parse. Unlike
// GlobalSettingsMentions it ignores comments and keys that only contain a
// key's name (shared_dir_storage_class), so a loaded file with no storage
// block never refuses because of one.
func GlobalSettingsStorageKey(keys []string) string {
	data, ok := readGlobalSettingsFile()
	if !ok {
		return ""
	}
	var doc interface{}
	if unmarshalSettingsYAML(data, &doc) != nil {
		return ""
	}
	return findMappingKey(doc, keys)
}

// findMappingKey returns the first of keys that is a mapping key anywhere
// in the parsed YAML document doc, or "".
func findMappingKey(doc interface{}, keys []string) string {
	switch v := doc.(type) {
	case map[string]interface{}:
		for _, key := range keys {
			if _, ok := v[key]; ok {
				return key
			}
		}
		for _, child := range v {
			if k := findMappingKey(child, keys); k != "" {
				return k
			}
		}
	case []interface{}:
		for _, child := range v {
			if k := findMappingKey(child, keys); k != "" {
				return k
			}
		}
	}
	return ""
}

// SharedDirPlacementKeys are the global settings keys that place shared
// dirs: server.project_storage, and the deprecated blocks it is synthesized
// from that carry a shared backend (workspace_storage, shared_dir_storage).
// A start that cannot read the global settings fails closed when the raw
// file mentions one of them (MentionedSharedDirPlacementKey), so shared
// dirs never silently fall back to node-local disk while the hub serves
// them from a share.
var SharedDirPlacementKeys = []string{"project_storage", "workspace_storage", "shared_dir_storage"}

// MentionedSharedDirPlacementKey returns the first key of
// SharedDirPlacementKeys that mentions reports (GlobalSettingsMentions in
// production), or "" when none is mentioned.
func MentionedSharedDirPlacementKey(mentions func(string) bool) string {
	for _, key := range SharedDirPlacementKeys {
		if mentions(key) {
			return key
		}
	}
	return ""
}

// HasSharedDirPlacementBlock reports whether s carries any block that
// places shared dirs (server.project_storage, server.workspace_storage or
// server.shared_dir_storage).
func (s *V1ServerConfig) HasSharedDirPlacementBlock() bool {
	return s != nil && (s.ProjectStorage != nil || s.WorkspaceStorage != nil || s.SharedDirStorage != nil)
}

// SharedDirStorageLayout is the shared-dir Layout of a broker: srv (the
// server section of the global-only settings) resolved through
// ResolveProjectStorage and ProjectStorageLayout, then
// projectfs.Layout.SharedDirLayout. split reports the transitional
// legacy split (shared dirs on server.shared_dir_storage's nfs export
// while workspaces are not on nfs). A nil srv gives the local Layout.
func SharedDirStorageLayout(srv *V1ServerConfig) (layout projectfs.Layout, split bool, err error) {
	ps, _, err := ResolveProjectStorage(srv)
	if err != nil {
		return projectfs.Layout{}, false, err
	}
	full, err := ProjectStorageLayout(ps, DefaultVolumeMountBase)
	if err != nil {
		return projectfs.Layout{}, false, err
	}
	return full.SharedDirLayout(), full.SharedDirs != nil, nil
}

// layer0StorageKeys are the settings paths of the storage blocks. They are
// Layer-0: read from the global settings (and the process environment),
// never from a project's settings file.
var layer0StorageKeys = []string{
	ProjectStorageKey, workspaceStorageKey, sharedDirStorageKey, homeStorageKey,
}

// snapshotLayer0StorageKeys copies the storage blocks k holds now (the
// defaults and global layers), keyed by path; a block k does not hold maps
// to nil.
func snapshotLayer0StorageKeys(k *koanf.Koanf) map[string]*koanf.Koanf {
	snap := make(map[string]*koanf.Koanf, len(layer0StorageKeys))
	for _, key := range layer0StorageKeys {
		if k.Exists(key) {
			snap[key] = k.Cut(key)
		} else {
			snap[key] = nil
		}
	}
	return snap
}

// restoreLayer0StorageKeys drops whatever the project layers set under the
// storage blocks and puts back the snapshot taken before them, so a
// project's settings.yaml can never place a project's storage. sources, when
// non-nil, loses the project attribution of the dropped keys.
func restoreLayer0StorageKeys(k *koanf.Koanf, snap map[string]*koanf.Koanf, sources map[string]string) error {
	for _, key := range layer0StorageKeys {
		if k.Exists(key) {
			cur := k.Cut(key)
			saved := snap[key]
			if saved == nil || !reflect.DeepEqual(cur.Raw(), saved.Raw()) {
				slog.Warn("a project's settings set a storage block, which is read from the global settings only; the project's value is ignored",
					"key", key)
			}
		}
		k.Delete(key)
		for src := range sources {
			if src == key || strings.HasPrefix(src, key+".") {
				delete(sources, src)
			}
		}
		if saved := snap[key]; saved != nil {
			if err := k.MergeAt(saved, key); err != nil {
				return fmt.Errorf("restoring %s: %w", key, err)
			}
			for _, sub := range saved.Keys() {
				if sources != nil {
					sources[key+"."+sub] = "global"
				}
			}
		}
	}
	return nil
}
