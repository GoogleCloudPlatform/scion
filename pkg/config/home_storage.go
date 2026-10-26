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
)

// Home storage backends and leaf modes.
const (
	HomeStorageBackendLocal = "local"
	HomeStorageBackendNFS   = "nfs"

	// HomeStorageLeafPod creates the agent's home directory on the export
	// from an init container in the agent's pod.
	HomeStorageLeafPod = "pod"
	// HomeStorageLeafBroker creates it from the broker, through the
	// broker's own mount of the export.
	HomeStorageLeafBroker = "broker"
)

// Defaults for server.home_storage.
const (
	DefaultHomeStorageStopGraceSeconds       = 30
	DefaultHomeStorageTerminationWaitSeconds = 15
	DefaultHomeStorageSkeletonMaxBytes       = 256 << 20
)

// Source keys ResolveHomeStorage returns for a value set in
// server.home_storage.
const (
	HomeStorageBackendGlobalSource = "server.home_storage.backend"
	HomeStorageLeafGlobalSource    = "server.home_storage.leaf"
)

// V1HomeStorageConfig is server.home_storage. It selects where the home of
// a Kubernetes agent lives. With the nfs backend the home is a per-agent
// directory on the NFS export of the same dispatch's shared_dir_storage,
// kept across stops and restarts. Every field is global only (the former
// per-profile and per-runtime overrides are rejected by validation). Like
// server.shared_dir_storage, it is read from global settings only.
type V1HomeStorageConfig struct {
	// Backend is "", "local" or "nfs". "" and "local" keep the home in the
	// pod.
	Backend string `json:"backend,omitempty" yaml:"backend,omitempty" koanf:"backend"`
	// Leaf is "", "pod" or "broker"; "" means "pod".
	Leaf string `json:"leaf,omitempty" yaml:"leaf,omitempty" koanf:"leaf"`
	// StopGraceSeconds is the termination grace period of pods with an NFS
	// home. 0 means DefaultHomeStorageStopGraceSeconds.
	StopGraceSeconds int `json:"stop_grace_seconds,omitempty" yaml:"stop_grace_seconds,omitempty" koanf:"stop_grace_seconds"`
	// TerminationWaitSeconds is how long a start waits, beyond the grace
	// period, for the agent's previous pod to stop. 0 means
	// DefaultHomeStorageTerminationWaitSeconds.
	TerminationWaitSeconds int `json:"termination_wait_seconds,omitempty" yaml:"termination_wait_seconds,omitempty" koanf:"termination_wait_seconds"`
	// SkeletonMaxBytes caps the image home skeleton copied into a new
	// home. 0 means DefaultHomeStorageSkeletonMaxBytes.
	SkeletonMaxBytes int64 `json:"skeleton_max_bytes,omitempty" yaml:"skeleton_max_bytes,omitempty" koanf:"skeleton_max_bytes"`
	// AllowIncompletePhases must be true for the nfs backend to be used
	// while the feature is still incomplete. For development only.
	AllowIncompletePhases bool `json:"allow_incomplete_phases,omitempty" yaml:"allow_incomplete_phases,omitempty" koanf:"allow_incomplete_phases"`
}

// Validate checks the values of the block. Unknown backend or leaf values
// and negative numbers are errors; values are matched exactly, so a typo
// never falls back to the local home.
func (h *V1HomeStorageConfig) Validate() error {
	return h.validate("server.home_storage", "backend")
}

// validate implements Validate, naming key (the block's settings key) and
// backendKey (the backend field under it) in its errors.
func (h *V1HomeStorageConfig) validate(key, backendKey string) error {
	if h == nil {
		return nil
	}
	if err := checkHomeStorageBackend(h.Backend); err != nil {
		return fmt.Errorf("%s.%s %w", key, backendKey, err)
	}
	if err := checkHomeStorageLeaf(h.Leaf); err != nil {
		return fmt.Errorf("%s.leaf %w", key, err)
	}
	if h.StopGraceSeconds < 0 {
		return fmt.Errorf("%s.stop_grace_seconds must not be negative (got %d)", key, h.StopGraceSeconds)
	}
	if h.TerminationWaitSeconds < 0 {
		return fmt.Errorf("%s.termination_wait_seconds must not be negative (got %d)", key, h.TerminationWaitSeconds)
	}
	if h.SkeletonMaxBytes < 0 {
		return fmt.Errorf("%s.skeleton_max_bytes must not be negative (got %d)", key, h.SkeletonMaxBytes)
	}
	return nil
}

// StopGrace returns StopGraceSeconds, or its default.
func (h *V1HomeStorageConfig) StopGrace() int {
	if h == nil || h.StopGraceSeconds == 0 {
		return DefaultHomeStorageStopGraceSeconds
	}
	return h.StopGraceSeconds
}

// TerminationWait returns TerminationWaitSeconds, or its default.
func (h *V1HomeStorageConfig) TerminationWait() int {
	if h == nil || h.TerminationWaitSeconds == 0 {
		return DefaultHomeStorageTerminationWaitSeconds
	}
	return h.TerminationWaitSeconds
}

// SkeletonMax returns SkeletonMaxBytes, or its default.
func (h *V1HomeStorageConfig) SkeletonMax() int64 {
	if h == nil || h.SkeletonMaxBytes == 0 {
		return DefaultHomeStorageSkeletonMaxBytes
	}
	return h.SkeletonMaxBytes
}

func checkHomeStorageBackend(v string) error {
	switch v {
	case "", HomeStorageBackendLocal, HomeStorageBackendNFS:
		return nil
	}
	return fmt.Errorf("must be \"local\" or \"nfs\" (got %q)", v)
}

func checkHomeStorageLeaf(v string) error {
	switch v {
	case "", HomeStorageLeafPod, HomeStorageLeafBroker:
		return nil
	}
	return fmt.Errorf("must be \"pod\" or \"broker\" (got %q)", v)
}

// ResolvedHomeStorage is the home storage setting a broker applies.
// Backend is "local" or "nfs" and Leaf is "pod" or "broker"; the sources
// name the settings key each value came from, or are empty for a built-in
// default.
type ResolvedHomeStorage struct {
	Backend       string
	BackendSource string
	Leaf          string
	LeafSource    string
}

// ResolveHomeStorage returns the home storage backend and leaf mode from
// the global block (EffectiveHomeStorage), else the defaults (local, pod).
// There is no per-profile, per-runtime or per-instance rung: those
// overrides are removed (design OQ2-A). It reads settings only: whether
// the value applies to a dispatch (the runtime type, the share, an agent's
// record) is decided by the caller. Call it only on global settings.
func (vs *VersionedSettings) ResolveHomeStorage() ResolvedHomeStorage {
	out := ResolvedHomeStorage{Backend: HomeStorageBackendLocal, Leaf: HomeStorageLeafPod}
	var global *V1HomeStorageConfig
	if vs != nil && vs.Server != nil {
		global = vs.Server.EffectiveHomeStorage()
	}
	if global == nil {
		return out
	}
	if global.Backend != "" {
		out.Backend, out.BackendSource = global.Backend, HomeStorageBackendGlobalSource
	}
	if global.Leaf != "" {
		out.Leaf, out.LeafSource = global.Leaf, HomeStorageLeafGlobalSource
	}
	return out
}

// IsKubernetesRuntimeEntry reports whether the runtime entry name targets
// Kubernetes.
func IsKubernetesRuntimeEntry(name string, rt V1RuntimeConfig) bool {
	return isKubernetesRuntimeEntry(name, rt)
}

// Hub dispatch windows a synchronous start must fit in: the rolling window
// of a deferred dispatch and the broker client's request timeout.
const (
	HubStartRollingWindowSeconds = 90
	HubBrokerRequestSeconds      = 120
)

// HomeStorageWindowWarnings returns a warning when a start could wait
// longer for the previous pod (stop_grace_seconds plus
// termination_wait_seconds) than the hub's dispatch windows allow. The
// rest of the start also needs time inside those windows.
func HomeStorageWindowWarnings(h *V1HomeStorageConfig) []string {
	if h == nil {
		return nil
	}
	bound := h.StopGrace() + h.TerminationWait()
	if bound < HubStartRollingWindowSeconds {
		return nil
	}
	return []string{fmt.Sprintf("server.home_storage: stop_grace_seconds + termination_wait_seconds is %ds, which does not fit the hub's %ds start window (and %ds broker request timeout); starts that wait for a previous pod may time out", bound, HubStartRollingWindowSeconds, HubBrokerRequestSeconds)}
}
