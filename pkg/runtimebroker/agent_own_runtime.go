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

package runtimebroker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// An existing agent's own runtime.
//
// An agent started under a broker profile records that profile in its
// agent-info.json (agent.GetSavedProfile); start and restart already resolve
// their runtime from it. Operations on an existing agent (delete, stop,
// status, logs, exec) resolve the same runtime here, before the recorded
// runtime check, so that:
//
//   - after a broker restart, a runtime that was only registered when the
//     agent started (for example a Kubernetes profile with its own
//     namespace) is registered again;
//   - the agent is looked up and acted on only in that runtime, so other
//     runtimes (for example the broker's default Kubernetes namespace, which
//     the broker may have no access to) are not queried and cannot fail the
//     operation.
//
// An agent with no saved profile, or whose profile no longer resolves, keeps
// the previous behaviour: every registered runtime is searched.

// agentOwnRuntime is the runtime an existing agent's saved profile resolves
// to, paired with its manager and the runtime's build record, all from the
// runtimeResolution that filled the entry.
type agentOwnRuntime struct {
	mgr     agent.Manager
	rt      scionrt.Runtime
	record  config.RuntimeBuildRecord
	profile string
	// fingerprint identifies the profile's runtime configuration the entry
	// was resolved from; a memoised entry whose fingerprint differs from the
	// current settings is resolved again and replaced.
	fingerprint string
}

type agentOwnRuntimeKey struct{}

// agentOwnRuntimeFrom returns the agent's own runtime attached to ctx by
// ensureAgentOwnRuntime, or nil.
func agentOwnRuntimeFrom(ctx context.Context) *agentOwnRuntime {
	own, _ := ctx.Value(agentOwnRuntimeKey{}).(*agentOwnRuntime)
	return own
}

// withoutAgentOwnRuntime returns ctx with no own runtime attached, so
// lookups search every registered runtime again.
func withoutAgentOwnRuntime(ctx context.Context) context.Context {
	if agentOwnRuntimeFrom(ctx) == nil {
		return ctx
	}
	return context.WithValue(ctx, agentOwnRuntimeKey{}, (*agentOwnRuntime)(nil))
}

// otherManagers returns the managers a request carrying ctx may target
// other than the agent's own runtime, for the best-effort searches delete
// and restart make when that runtime holds no container for the agent.
func (s *Server) otherManagers(ctx context.Context, own *agentOwnRuntime) []agent.Manager {
	var others []agent.Manager
	for _, mgr := range s.allManagers(withoutAgentOwnRuntime(ctx)) {
		if mgr != own.mgr {
			others = append(others, mgr)
		}
	}
	return others
}

type projectPathHintKey struct{}

// withProjectPathHint attaches the hub's projectPath hint for callers that
// reach ensureAgentOwnRuntime through an interface without that argument
// (LookupAgent, used by terminal attach).
func withProjectPathHint(ctx context.Context, hint string) context.Context {
	if hint == "" {
		return ctx
	}
	return context.WithValue(ctx, projectPathHintKey{}, hint)
}

// projectPathHintFrom returns the hint attached with withProjectPathHint.
func projectPathHintFrom(ctx context.Context) string {
	hint, _ := ctx.Value(projectPathHintKey{}).(string)
	return hint
}

// ownRuntimeFor returns the agent's own runtime for a request carrying ctx,
// or nil when it is unknown or excluded by the recorded runtime type.
func (s *Server) ownRuntimeFor(ctx context.Context) *agentOwnRuntime {
	own := agentOwnRuntimeFrom(ctx)
	if own == nil || own.mgr == nil || !runtimeAllowed(ctx, own.rt) {
		return nil
	}
	return own
}

// ensureAgentOwnRuntime resolves the runtime agent id's saved profile
// selects, registering it as an auxiliary runtime when it is not the
// broker's default (resolveManagerForOpts), and returns ctx carrying it.
// ctx is returned unchanged when the agent's project directory or saved
// profile is unknown, or the profile does not resolve.
//
// Results are memoised per project directory and profile. Each entry keeps
// the fingerprint of the profile runtime configuration it was resolved from;
// when settings no longer match it (for example an edited namespace), the
// profile is resolved again and the entry replaced. Concurrent requests for
// one configuration share a single resolution; a failed runtime resolution
// is not stored and is retried on the next request.
//
// A saved profile that does not resolve in the loaded settings (it is no
// longer in the profile catalog, or names a missing runtime) is memoised too,
// keyed by project directory and profile, with the fingerprint of the
// catalog it failed in (profileCatalogFingerprint). While the catalog is
// unchanged, later requests skip the resolution; a settings change that
// touches the catalog (for example an admin save restoring the profile)
// changes the fingerprint, so the next request resolves again. The fallback
// is logged at warn level once per agent, profile and catalog, then at debug
// level (logSavedProfileFallback). The agent's saved profile is never
// rewritten: the configuration error stays visible and start keeps refusing
// the missing profile.
//
// Per-request cost: every existing-agent request reads the agent's
// agent-info.json and the project's settings (local file reads only). The
// runtime itself is resolved (for Kubernetes, a client build and an API
// server check) only on the first request for a configuration, or after
// that configuration changes.
func (s *Server) ensureAgentOwnRuntime(ctx context.Context, id, projectID, projectPathHint string) context.Context {
	// A flat instance has one runtime and never resolves a saved profile.
	if defMgr, defRT, _ := s.defaultPair(); defMgr == nil || defRT == nil || s.isFlat() {
		return ctx
	}
	projectDir := s.knownAgentProjectDir(id, projectID, projectPathHint)
	if projectDir == "" {
		return ctx
	}
	profile := agent.GetSavedProfile(id, projectDir)
	if profile == "" {
		return ctx
	}
	vs, _, err := config.LoadEffectiveSettings(projectDir)
	if err != nil || vs == nil {
		return ctx
	}
	catalog := profileCatalogFingerprint(vs, profile)
	rtConfig, runtimeType, err := s.resolveSavedProfile(vs, projectDir, profile, catalog)
	if err != nil {
		s.logSavedProfileFallback(id, projectID, projectDir, profile, catalog, err)
		return ctx
	}
	// The profile resolves: a later failure, even in a catalog seen before,
	// is logged at warn level again.
	s.savedProfileWarned.Delete(savedProfileWarnedKey(projectDir, id, profile))
	fingerprint, err := json.Marshal(rtConfig)
	if err != nil {
		return ctx
	}
	key := projectDir + "\x00" + profile
	fp := runtimeType + "\x00" + string(fingerprint)

	if v, ok := s.agentOwnRuntimes.Load(key); ok && v.(*agentOwnRuntime).fingerprint == fp {
		return context.WithValue(ctx, agentOwnRuntimeKey{}, v.(*agentOwnRuntime))
	}
	v, err, _ := s.agentOwnRuntimeGroup.Do(key+"\x00"+fp, func() (any, error) {
		if v, ok := s.agentOwnRuntimes.Load(key); ok && v.(*agentOwnRuntime).fingerprint == fp {
			return v, nil
		}
		res := s.resolveManagerForOpts(api.StartOptions{Name: id, ProjectPath: projectDir, Profile: profile})
		mgr, name := res.Manager, res.RuntimeType
		if mgr == nil || name == "error" {
			return nil, errors.New("profile runtime could not be resolved")
		}
		// The runtime and record come from the same resolution as mgr, never
		// from runtimeOfManager.
		rt := res.Runtime
		if rt == nil {
			return nil, fmt.Errorf("manager for runtime %q exposes no runtime", name)
		}
		own := &agentOwnRuntime{mgr: mgr, rt: rt, record: res.Record, profile: profile, fingerprint: fp}
		s.agentOwnRuntimes.Store(key, own)
		return own, nil
	})
	if err != nil {
		s.agentLifecycleLog.Warn("Could not resolve the agent's own runtime; searching all registered runtimes",
			"agent_id", id, "project_id", projectID, "profile", profile, "error", err)
		return ctx
	}
	return context.WithValue(ctx, agentOwnRuntimeKey{}, v.(*agentOwnRuntime))
}

// savedProfileFailure is a memoised failed resolution of a saved profile.
type savedProfileFailure struct {
	catalog string // profileCatalogFingerprint of the settings it failed in
	err     error
}

// savedProfileKey keys the memo of failed saved-profile resolutions.
func savedProfileKey(projectDir, profile string) string {
	return projectDir + "\x00" + profile
}

// resolveSavedProfile resolves profile's runtime configuration in vs, whose
// profileCatalogFingerprint is catalog, except when the same profile already
// failed to resolve for projectDir in a catalog with that fingerprint; then
// the memoised error is returned without resolving again. A failure is
// stored, replacing any entry for the key, and a success removes the entry,
// so the memo holds at most one entry per project directory and saved
// profile that currently fails.
func (s *Server) resolveSavedProfile(vs *config.VersionedSettings, projectDir, profile, catalog string) (config.V1RuntimeConfig, string, error) {
	key := savedProfileKey(projectDir, profile)
	if v, ok := s.savedProfileFailures.Load(key); ok && v.(*savedProfileFailure).catalog == catalog {
		return config.V1RuntimeConfig{}, "", v.(*savedProfileFailure).err
	}
	resolve := s.resolveProfileRuntime
	if resolve == nil {
		resolve = (*config.VersionedSettings).ResolveRuntime
	}
	rtConfig, runtimeType, err := resolve(vs, profile)
	if err != nil {
		s.savedProfileFailures.Store(key, &savedProfileFailure{catalog: catalog, err: err})
		return config.V1RuntimeConfig{}, "", err
	}
	s.savedProfileFailures.Delete(key)
	return rtConfig, runtimeType, nil
}

// profileCatalogFingerprint identifies everything in vs that decides whether
// profile resolves: the profile's runtime name and the sorted names of all
// profiles and runtimes. Any change to the catalog changes it, so a
// memoised failure is not reused after settings add, remove or rename a
// profile or runtime.
func profileCatalogFingerprint(vs *config.VersionedSettings, profile string) string {
	h := sha256.New()
	write := func(parts ...string) {
		for _, p := range parts {
			h.Write([]byte(p))
			h.Write([]byte{0})
		}
	}
	if p, ok := vs.Profiles[profile]; ok {
		write("profile", profile, "runtime", p.Runtime)
	} else {
		write("profile", profile, "missing")
	}
	write("profiles")
	write(slices.Sorted(maps.Keys(vs.Profiles))...)
	write("runtimes")
	write(slices.Sorted(maps.Keys(vs.Runtimes))...)
	return hex.EncodeToString(h.Sum(nil))
}

// savedProfileWarnedKey keys the record of logged saved-profile failures.
func savedProfileWarnedKey(projectDir, id, profile string) string {
	return projectDir + "\x00" + id + "\x00" + profile
}

// logSavedProfileFallback logs that agent id's saved profile does not
// resolve and every registered runtime is searched instead: at warn level
// the first time for the agent, profile and catalog in this process, at
// debug level after that, since every existing-agent request (each message
// delivery, status, logs, exec) repeats the same finding. One entry per
// project directory, agent and profile holds the last catalog logged at
// warn level, removed when the profile resolves again, so the map stays
// bounded by the agents seen in this process times their saved profiles.
//
// A request that read older settings, in which the profile failed, can
// record its catalog after a concurrent request that read newer settings
// removed the entry on success. The only effect is that a later loss of the
// profile back to exactly that catalog is logged at debug level rather than
// warn. Compare-and-swap on this map cannot tell the stale write apart, and
// ordering the two would need a per-key version, which a log level does not
// justify.
func (s *Server) logSavedProfileFallback(id, projectID, projectDir, profile, catalog string, err error) {
	log := s.agentLifecycleLog.Debug
	if prev, seen := s.savedProfileWarned.Swap(savedProfileWarnedKey(projectDir, id, profile), catalog); !seen || prev != catalog {
		log = s.agentLifecycleLog.Warn
	}
	log("Agent's saved profile no longer resolves; searching all registered runtimes",
		"agent_id", id, "project_id", projectID, "profile", profile, "error", err)
}

// runtimeOfManager returns the runtime behind mgr, or nil.
func (s *Server) runtimeOfManager(mgr agent.Manager) scionrt.Runtime {
	if defMgr, defRT, _ := s.defaultPair(); mgr == defMgr {
		return defRT
	}
	if am, ok := mgr.(*agent.AgentManager); ok {
		return am.Runtime
	}
	return nil
}

// knownAgentProjectDir returns the .scion directory of the project that holds
// agent id's files and, when projectID is set, identifies as projectID. The
// candidates are, in order: a hub-managed project; the linked project at the
// hub's projectPathHint (the provider's local path), accepted only when its
// recorded identity is projectID (linkedProjectAgentDir, the check the
// delete path uses); and the broker's own working project. When the hint
// names one of the candidates, that one is used. "" when none matches.
func (s *Server) knownAgentProjectDir(id, projectID, projectPathHint string) string {
	if !isSingleCleanPathElement(id) {
		return ""
	}
	var candidates []string
	if dir, err := findAgentInHubManagedProjects(id, projectID); err == nil && dir != "" {
		candidates = append(candidates, dir)
	}
	if dir := linkedProjectAgentDir(projectPathHint, id, projectID); dir != "" {
		candidates = append(candidates, dir)
	}
	if cwd, err := config.GetResolvedProjectDir(""); err == nil && cwd != "" {
		if (projectID == "" || pathIdentifiesAs(cwd, projectID)) && hubManagedProjectHasAgent(cwd, id) {
			candidates = append(candidates, cwd)
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	if projectPathHint != "" {
		hint := filepath.Clean(projectPathHint)
		for _, c := range candidates {
			if c == hint || filepath.Dir(c) == hint {
				return c
			}
		}
	}
	return candidates[0]
}

// ownRuntimeNamespace describes where a Kubernetes own runtime looks for
// pods, for logs; "" for other runtimes.
func ownRuntimeNamespace(rt scionrt.Runtime) string {
	k, ok := rt.(*scionrt.KubernetesRuntime)
	if !ok {
		return ""
	}
	if k.ListAllNamespaces {
		return "(all namespaces)"
	}
	return k.DefaultNamespace
}
