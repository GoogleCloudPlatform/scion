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
	"sort"
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

// Source keys ResolveHomeStorage returns when no profile or runtime entry
// overrides a value.
const (
	HomeStorageBackendGlobalSource = "server.home_storage.backend"
	HomeStorageLeafGlobalSource    = "server.home_storage.leaf"
)

// V1HomeStorageConfig is server.home_storage. It selects where the home of
// a Kubernetes agent lives. With the nfs backend the home is a per-agent
// directory on the NFS export of the same dispatch's shared_dir_storage,
// kept across stops and restarts. The backend and leaf mode can be
// overridden per profile and runtime entry (home_storage_backend,
// home_storage_leaf); the other fields are global only. Like
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
	if h == nil {
		return nil
	}
	if err := checkHomeStorageBackend(h.Backend); err != nil {
		return fmt.Errorf("server.home_storage.backend %w", err)
	}
	if err := checkHomeStorageLeaf(h.Leaf); err != nil {
		return fmt.Errorf("server.home_storage.leaf %w", err)
	}
	if h.StopGraceSeconds < 0 {
		return fmt.Errorf("server.home_storage.stop_grace_seconds must not be negative (got %d)", h.StopGraceSeconds)
	}
	if h.TerminationWaitSeconds < 0 {
		return fmt.Errorf("server.home_storage.termination_wait_seconds must not be negative (got %d)", h.TerminationWaitSeconds)
	}
	if h.SkeletonMaxBytes < 0 {
		return fmt.Errorf("server.home_storage.skeleton_max_bytes must not be negative (got %d)", h.SkeletonMaxBytes)
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

// ResolvedHomeStorage is the home storage setting that applies to one
// profile. Backend is "local" or "nfs" and Leaf is "pod" or "broker"; the
// sources name the settings key each value came from, or are empty for a
// built-in default.
type ResolvedHomeStorage struct {
	Backend       string
	BackendSource string
	Leaf          string
	LeafSource    string
}

// ResolveHomeStorage returns the home storage backend and leaf mode for
// agents using profileName. Each value comes from the profile, else its
// runtime entry, else server.home_storage, else the default (local, pod),
// through ResolveProfileValue. It reads settings only: whether the value
// applies to a dispatch (the runtime type, the shared-dir share, an agent's
// record) is decided by the caller. Call it only on global settings.
func (vs *VersionedSettings) ResolveHomeStorage(profileName string) ResolvedHomeStorage {
	out := ResolvedHomeStorage{Backend: HomeStorageBackendLocal, Leaf: HomeStorageLeafPod}
	var global *V1HomeStorageConfig
	if vs != nil && vs.Server != nil {
		global = vs.Server.HomeStorage
	}
	backend, src := ResolveProfileValue(vs, profileName, "home_storage_backend",
		func(p V1ProfileConfig) string { return p.HomeStorageBackend },
		func(r V1RuntimeConfig) string { return r.HomeStorageBackend })
	if backend == "" && global != nil && global.Backend != "" {
		backend, src = global.Backend, HomeStorageBackendGlobalSource
	}
	if backend != "" {
		out.Backend, out.BackendSource = backend, src
	}
	leaf, lsrc := ResolveProfileValue(vs, profileName, "home_storage_leaf",
		func(p V1ProfileConfig) string { return p.HomeStorageLeaf },
		func(r V1RuntimeConfig) string { return r.HomeStorageLeaf })
	if leaf == "" && global != nil && global.Leaf != "" {
		leaf, lsrc = global.Leaf, HomeStorageLeafGlobalSource
	}
	if leaf != "" {
		out.Leaf, out.LeafSource = leaf, lsrc
	}
	return out
}

// ValidateHomeStorageOverrides checks home_storage_backend and
// home_storage_leaf on every runtime and profile entry: the backend must be
// empty, "local" or "nfs" and the leaf empty, "pod" or "broker". It checks
// values only. Results are sorted by path.
func ValidateHomeStorageOverrides(runtimes map[string]V1RuntimeConfig, profiles map[string]V1ProfileConfig) []ValidationError {
	var errs []ValidationError
	add := func(path string, err error) {
		if err != nil {
			errs = append(errs, ValidationError{Path: path, Message: err.Error()})
		}
	}
	for name, rt := range runtimes {
		add("runtimes."+name+".home_storage_backend", checkHomeStorageBackend(rt.HomeStorageBackend))
		add("runtimes."+name+".home_storage_leaf", checkHomeStorageLeaf(rt.HomeStorageLeaf))
	}
	for name, p := range profiles {
		add("profiles."+name+".home_storage_backend", checkHomeStorageBackend(p.HomeStorageBackend))
		add("profiles."+name+".home_storage_leaf", checkHomeStorageLeaf(p.HomeStorageLeaf))
	}
	sort.Slice(errs, func(i, j int) bool { return errs[i].Path < errs[j].Path })
	return errs
}

// HomeStorageIgnoredWarnings returns one warning per runtime or profile
// entry that sets home_storage_backend "nfs" but does not target
// Kubernetes. Agents on other runtimes always keep a local home, so the
// value has no effect there. It never makes the settings invalid.
func HomeStorageIgnoredWarnings(runtimes map[string]V1RuntimeConfig, profiles map[string]V1ProfileConfig) []string {
	var warnings []string
	for name, rt := range runtimes {
		if rt.HomeStorageBackend == HomeStorageBackendNFS && !isKubernetesRuntimeEntry(name, rt) {
			warnings = append(warnings, fmt.Sprintf("runtimes.%s.home_storage_backend is \"nfs\" but runtime %q is not a Kubernetes runtime; agents on it keep a local home", name, name))
		}
	}
	for name, p := range profiles {
		if p.HomeStorageBackend != HomeStorageBackendNFS {
			continue
		}
		rt, ok := runtimes[p.Runtime]
		if ok && !isKubernetesRuntimeEntry(p.Runtime, rt) {
			warnings = append(warnings, fmt.Sprintf("profiles.%s.home_storage_backend is \"nfs\" but its runtime %q is not a Kubernetes runtime; agents on it keep a local home", name, p.Runtime))
		}
	}
	sort.Strings(warnings)
	return warnings
}

// IsKubernetesRuntimeEntry reports whether the runtime entry name targets
// Kubernetes (see HomeStorageIgnoredWarnings).
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
