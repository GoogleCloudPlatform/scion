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
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
)

// Agent home storage.
//
// A Kubernetes agent's home is either local (in the pod, filled from the
// broker's copy at every start) or on NFS: a per-agent directory on the
// export of the dispatch's shared_dir_storage, kept across starts. Which
// one an agent gets is decided once, at its first start, and recorded in
// homeStorageRecordFile next to the shared-dir storage record. Every later
// start (restart, resume, reincarnation) uses the record, so a settings
// change never moves an existing home.

// homeStorageNFSAvailable reports whether this build can start agents with
// an NFS home. While it is false, a start that resolves to an NFS home
// fails with a clear error instead of starting with a different home.
var homeStorageNFSAvailable = false

// homeStorageRecordFile is the per-agent file, in the agent directory next
// to scion-agent.json and the shared-dir storage record, that records the
// home storage an agent was first started with. Like that record it is
// outside the agent home, so the agent's container never mounts it.
const homeStorageRecordFile = "home-storage.json"

// homeStoragePending is the record ProvisionAgent writes for a newly
// created agent. It marks an agent whose home storage is chosen at its
// first start. An agent with no record at all was created before home
// storage was recorded and keeps a local home.
const homeStoragePending = "pending"

// homeStorageRecord is the content of homeStorageRecordFile. For an NFS
// home it holds the share, claim, subpath root and leaf mode the home was
// created with.
type homeStorageRecord struct {
	Backend     string `json:"backend"`
	Leaf        string `json:"leaf,omitempty"`
	ShareID     string `json:"share_id,omitempty"`
	PVClaimName string `json:"pv_claim_name,omitempty"`
	SubPathRoot string `json:"subpath_root,omitempty"`
}

// homeStoragePlan is the home storage one start uses. Backend is "local" or
// "nfs"; the other fields are set for "nfs" only. MountRoot comes from the
// current settings and is used by the broker leaf mode only.
type homeStoragePlan struct {
	Backend     string
	Leaf        string
	ShareID     string
	PVClaimName string
	SubPathRoot string
	MountRoot   string
	AgentID     string
	Settings    *config.V1HomeStorageConfig
}

func localHomeStoragePlan() *homeStoragePlan {
	return &homeStoragePlan{Backend: config.HomeStorageBackendLocal}
}

// homeStorageUnavailable builds the error a start returns when an agent
// that needs an NFS home cannot get one.
func homeStorageUnavailable(format string, args ...any) error {
	return fmt.Errorf("home_storage_unavailable: "+format, args...)
}

// readHomeStorageRecord returns the agent's home storage record, or nil
// when there is none. A record that exists but cannot be read or parsed,
// or that names no or an unknown backend, is an error, so a damaged record
// never falls back to the current settings.
func readHomeStorageRecord(agentDir string) (*homeStorageRecord, error) {
	if agentDir == "" {
		return nil, nil
	}
	path := filepath.Join(agentDir, homeStorageRecordFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the agent's home storage record: %w", err)
	}
	var rec homeStorageRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("parsing the agent's home storage record %s: %w", path, err)
	}
	switch rec.Backend {
	case homeStoragePending, config.HomeStorageBackendLocal:
	case config.HomeStorageBackendNFS:
		if rec.ShareID == "" || rec.PVClaimName == "" || !validSubPathRoot(rec.SubPathRoot) ||
			(rec.Leaf != config.HomeStorageLeafPod && rec.Leaf != config.HomeStorageLeafBroker) {
			return nil, fmt.Errorf("the agent's home storage record %s is incomplete", path)
		}
	default:
		return nil, fmt.Errorf("the agent's home storage record %s names an unknown backend %q", path, rec.Backend)
	}
	return &rec, nil
}

// writeHomeStorageRecord records rec for the agent whose directory is
// agentDir, with the same writer as the shared-dir storage record.
func writeHomeStorageRecord(agentDir string, rec homeStorageRecord) error {
	if agentDir == "" {
		return fmt.Errorf("no agent directory to record the home storage in")
	}
	return writeAgentRecordFile(agentDir, homeStorageRecordFile, rec)
}

// markHomeStoragePending writes the pending record for a newly created
// agent directory. An existing record is never replaced.
func markHomeStoragePending(agentDir string) error {
	if _, err := os.Lstat(filepath.Join(agentDir, homeStorageRecordFile)); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return writeHomeStorageRecord(agentDir, homeStorageRecord{Backend: homeStoragePending})
}

// validHomeAgentID reports whether id can name an agent's home directory:
// a hub agent ID in canonical UUID form that differs from the slug. The
// agent name is never used as a home ID.
func validHomeAgentID(id, slug string) bool {
	if len(id) != 36 || id != strings.ToLower(id) || id == slug {
		return false
	}
	_, err := uuid.Parse(id)
	return err == nil
}

// homeStorageInput holds what resolveHomeStorage needs for one start.
type homeStorageInput struct {
	AgentDir    string
	AgentName   string
	Slug        string
	RuntimeName string
	Profile     string
	// AgentID and ProjectID are the hub agent and project IDs from the
	// dispatch (SCION_AGENT_ID, SCION_PROJECT_ID), or "" for a start the
	// hub did not dispatch.
	AgentID   string
	ProjectID string
	// ExperimentOn reports whether the hub sent experiments.K8sNFSHome as
	// enabled with this dispatch.
	ExperimentOn bool
	// LoadSettings returns the global settings with the hub overlay
	// (config.LoadGlobalSettingsWithOverlay). It is called only for
	// Kubernetes starts that need the settings.
	LoadSettings func() (*config.VersionedSettings, error)
}

// resolveHomeStorage returns the home storage for one start and, on a
// first start, records it.
//
//   - A start on any runtime other than Kubernetes always gets a local home.
//     It reads no settings and no record and touches no NFS path.
//   - An agent with a record uses it. A recorded NFS home needs the
//     experiment on and its share still configured; otherwise the start
//     fails, and never falls back to a new local home.
//   - An agent with no record was created before home storage was recorded
//     and keeps a local home; the record is written as local.
//   - A pending record (a newly created agent) resolves from the current
//     settings through config.VersionedSettings.ResolveHomeStorage. An nfs
//     result needs the experiment on, the hub agent ID, and the profile's
//     shared_dir_storage resolving to nfs with a claim; the share comes from
//     there. With the experiment off the agent gets, and records, a local
//     home.
func resolveHomeStorage(in homeStorageInput) (*homeStoragePlan, error) {
	if !isKubernetesRuntime(in.RuntimeName) {
		return localHomeStoragePlan(), nil
	}
	rec, err := readHomeStorageRecord(in.AgentDir)
	if err != nil {
		return nil, err
	}
	switch {
	case rec == nil:
		if err := writeHomeStorageRecord(in.AgentDir, homeStorageRecord{Backend: config.HomeStorageBackendLocal}); err != nil {
			slog.Warn("Start: could not record the agent's home storage", "agent", in.AgentName, "error", err)
		}
		return localHomeStoragePlan(), nil
	case rec.Backend == config.HomeStorageBackendLocal:
		return localHomeStoragePlan(), nil
	case rec.Backend == config.HomeStorageBackendNFS:
		return recordedNFSHomeStorage(in, rec)
	}

	// Pending: the agent's first start.
	gs, err := in.LoadSettings()
	if errors.Is(err, errHomeStorageNotConfigured) {
		// The settings cannot be read but do not mention home storage: the
		// agent gets, and records, a local home, so the choice is made once.
		slog.Warn("Start: global settings could not be loaded; server.home_storage is not mentioned in them, so the agent gets a local home",
			"agent", in.AgentName)
		if err := writeHomeStorageRecord(in.AgentDir, homeStorageRecord{Backend: config.HomeStorageBackendLocal}); err != nil {
			return nil, fmt.Errorf("recording the agent's home storage: %w", err)
		}
		return localHomeStoragePlan(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("loading global settings for home storage: %w", err)
	}
	resolved := gs.ResolveHomeStorage(in.Profile)
	// Values are checked where they are used: an unknown value (a typo or
	// the wrong case) fails the start and is never recorded.
	if err := checkResolvedHomeStorage(resolved); err != nil {
		return nil, err
	}
	if resolved.Backend != config.HomeStorageBackendNFS || !in.ExperimentOn {
		if resolved.Backend == config.HomeStorageBackendNFS {
			slog.Warn("Start: home storage nfs is configured but the experiment is off; the agent gets a local home",
				"agent", in.AgentName, "source", resolved.BackendSource, "experiment", experiments.K8sNFSHome)
		}
		if err := writeHomeStorageRecord(in.AgentDir, homeStorageRecord{Backend: config.HomeStorageBackendLocal}); err != nil {
			return nil, fmt.Errorf("recording the agent's home storage: %w", err)
		}
		return localHomeStoragePlan(), nil
	}
	plan, err := newNFSHomeStoragePlan(in, gs, resolved.Leaf)
	if err != nil {
		return nil, err
	}
	if err := writeHomeStorageRecord(in.AgentDir, homeStorageRecord{
		Backend:     config.HomeStorageBackendNFS,
		Leaf:        plan.Leaf,
		ShareID:     plan.ShareID,
		PVClaimName: plan.PVClaimName,
		SubPathRoot: plan.SubPathRoot,
	}); err != nil {
		return nil, fmt.Errorf("recording the agent's home storage: %w", err)
	}
	return plan, nil
}

// checkNFSHomeStart returns an error unless this start may use an NFS home:
// the build supports it, the experiment is on, the development gate is set
// and the hub agent and project IDs are valid.
func checkNFSHomeStart(in homeStorageInput, hs *config.V1HomeStorageConfig) error {
	if !in.ExperimentOn {
		return homeStorageUnavailable("agent %q has an NFS home but the %s experiment is off", in.AgentName, experiments.K8sNFSHome)
	}
	if !homeStorageNFSAvailable {
		return homeStorageUnavailable("home storage nfs is not available in this version of the broker")
	}
	if hs == nil || !hs.AllowIncompletePhases {
		return homeStorageUnavailable("home storage nfs is incomplete in this version and needs server.home_storage.allow_incomplete_phases")
	}
	if !validHomeAgentID(in.AgentID, in.Slug) {
		return homeStorageUnavailable("no agent ID")
	}
	if !shareddirs.ValidProjectID(in.ProjectID) {
		return homeStorageUnavailable("no valid hub project ID")
	}
	if !validHomeSlug(in.Slug) {
		return homeStorageUnavailable("agent name %q is not an agent slug", in.Slug)
	}
	return nil
}

// checkResolvedHomeStorage fails on a resolved backend, or (for nfs) leaf
// mode, that is not a known value, naming the settings key it came from.
func checkResolvedHomeStorage(r config.ResolvedHomeStorage) error {
	source := func(src, fallback string) string {
		if src == "" {
			return fallback
		}
		return src
	}
	switch r.Backend {
	case config.HomeStorageBackendLocal, config.HomeStorageBackendNFS:
	default:
		return fmt.Errorf("%s must be \"local\" or \"nfs\" (got %q)", source(r.BackendSource, "home storage backend"), r.Backend)
	}
	if r.Backend == config.HomeStorageBackendNFS && r.Leaf != config.HomeStorageLeafPod && r.Leaf != config.HomeStorageLeafBroker {
		return fmt.Errorf("%s must be \"pod\" or \"broker\" (got %q)", source(r.LeafSource, "home storage leaf"), r.Leaf)
	}
	return nil
}

// validHomeSlug reports whether slug is an agent slug: non-empty and
// already in the form api.ValidateAgentName produces.
func validHomeSlug(slug string) bool {
	s, err := api.ValidateAgentName(slug)
	return err == nil && s == slug && slug != ""
}

// validSubPathRoot reports whether root is a clean, non-empty relative
// path that stays inside the export.
func validSubPathRoot(root string) bool {
	return root != "" && filepath.IsLocal(root) && filepath.Clean(root) == root && !strings.Contains(root, "\\")
}

func homeStorageGlobal(gs *config.VersionedSettings) *config.V1HomeStorageConfig {
	if gs == nil || gs.Server == nil {
		return nil
	}
	return gs.Server.HomeStorage
}

// newNFSHomeStoragePlan builds the plan for an agent's first start with an
// NFS home. The share is the first share of the profile's resolved
// shared_dir_storage nfs block, the same share and claim the agent's shared
// dirs use.
func newNFSHomeStoragePlan(in homeStorageInput, gs *config.VersionedSettings, leaf string) (*homeStoragePlan, error) {
	hs := homeStorageGlobal(gs)
	if err := hs.Validate(); err != nil {
		return nil, err
	}
	if err := checkNFSHomeStart(in, hs); err != nil {
		return nil, err
	}
	sd, _ := gs.ResolveSharedDirStorage(in.Profile)
	if sd == nil || sd.Backend != config.HomeStorageBackendNFS {
		return nil, homeStorageUnavailable("home_storage nfs needs shared_dir_storage nfs for profile %q", in.Profile)
	}
	if err := sd.Validate(); err != nil {
		return nil, homeStorageUnavailable("home_storage nfs needs a complete shared_dir_storage nfs block for profile %q: %v", in.Profile, err)
	}
	share := sd.NFS.Shares[0]
	if !validSubPathRoot(nfsSubPathRoot(sd.NFS)) {
		return nil, homeStorageUnavailable("shared_dir_storage nfs subpath_root %q is not a clean relative path", nfsSubPathRoot(sd.NFS))
	}
	if share.PVName == "" {
		return nil, homeStorageUnavailable("home_storage nfs needs a claim (shares[0].pv_name) in shared_dir_storage for profile %q", in.Profile)
	}
	return &homeStoragePlan{
		Backend:     config.HomeStorageBackendNFS,
		Leaf:        leaf,
		ShareID:     share.ID,
		PVClaimName: share.PVName,
		SubPathRoot: nfsSubPathRoot(sd.NFS),
		MountRoot:   sd.NFS.MountRoot,
		AgentID:     in.AgentID,
		Settings:    hs,
	}, nil
}

// recordedNFSHomeStorage builds the plan for a later start of an agent
// whose record names an NFS home. The recorded share must still be
// configured in server.shared_dir_storage with the recorded claim.
func recordedNFSHomeStorage(in homeStorageInput, rec *homeStorageRecord) (*homeStoragePlan, error) {
	gs, err := in.LoadSettings()
	if err != nil {
		return nil, fmt.Errorf("loading global settings for home storage: %w", err)
	}
	hs := homeStorageGlobal(gs)
	if err := hs.Validate(); err != nil {
		return nil, err
	}
	if err := checkNFSHomeStart(in, hs); err != nil {
		return nil, err
	}
	var nfs *config.V1NFSConfig
	if gs.Server != nil && gs.Server.SharedDirStorage != nil {
		nfs = gs.Server.SharedDirStorage.NFS
	}
	if nfs != nil {
		for _, share := range nfs.Shares {
			if share.ID == rec.ShareID && share.PVName == rec.PVClaimName {
				return &homeStoragePlan{
					Backend:     config.HomeStorageBackendNFS,
					Leaf:        rec.Leaf,
					ShareID:     rec.ShareID,
					PVClaimName: rec.PVClaimName,
					SubPathRoot: rec.SubPathRoot,
					MountRoot:   nfs.MountRoot,
					AgentID:     in.AgentID,
					Settings:    hs,
				}, nil
			}
		}
	}
	return nil, homeStorageUnavailable("recorded home share %s not configured", rec.ShareID)
}

// errHomeStorageNotConfigured is returned by loadHomeStorageSettings when
// the global settings cannot be loaded and do not mention home storage.
var errHomeStorageNotConfigured = errors.New("global settings could not be loaded and do not mention home storage")

// loadHomeStorageSettings is homeStorageInput.LoadSettings for agent
// starts: the global settings with the hub overlay, never a project's
// settings. A file that does not load fails the start only when it
// mentions home storage (the raw-bytes check the shared-dir storage start
// uses); otherwise it returns errHomeStorageNotConfigured, so a broken
// settings file that never configured this feature does not stop agents.
func loadHomeStorageSettings() (*config.VersionedSettings, error) {
	gs, _, err := config.LoadGlobalSettingsWithOverlay()
	if err != nil {
		if config.GlobalSettingsMentions("home_storage") {
			return nil, err
		}
		return nil, errHomeStorageNotConfigured
	}
	if gs != nil && (gs.Server == nil || gs.Server.HomeStorage == nil) &&
		config.GlobalSettingsIsLegacyFormat() && config.GlobalSettingsMentions("home_storage") {
		return nil, fmt.Errorf("global settings mention home_storage but it was not loaded (missing schema_version: \"1\"?)")
	}
	return gs, nil
}

// nfsSubPathRoot is the subpath root of an nfs block, with the same default
// the NFS backend uses.
func nfsSubPathRoot(nfs *config.V1NFSConfig) string {
	if nfs == nil || nfs.SubPathRoot == "" {
		return "projects"
	}
	return nfs.SubPathRoot
}
