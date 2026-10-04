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
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

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
// to, paired with its manager.
type agentOwnRuntime struct {
	mgr     agent.Manager
	rt      scionrt.Runtime
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
// one configuration share a single resolution; a failed resolution is not
// stored and is retried on the next request.
//
// Per-request cost: every existing-agent request reads the agent's
// agent-info.json and the project's settings (local file reads only). The
// runtime itself is resolved (for Kubernetes, a client build and an API
// server check) only on the first request for a configuration, or after
// that configuration changes.
func (s *Server) ensureAgentOwnRuntime(ctx context.Context, id, projectID, projectPathHint string) context.Context {
	if s.manager == nil || s.runtime == nil {
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
	rtConfig, runtimeType, err := vs.ResolveRuntime(profile)
	if err != nil {
		s.agentLifecycleLog.Warn("Agent's saved profile no longer resolves; searching all registered runtimes",
			"agent_id", id, "project_id", projectID, "profile", profile, "error", err)
		return ctx
	}
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
		mgr, name := s.resolveManagerForOpts(api.StartOptions{Name: id, ProjectPath: projectDir, Profile: profile})
		if mgr == nil || name == "error" {
			return nil, errors.New("profile runtime could not be resolved")
		}
		rt := s.runtimeOfManager(mgr)
		if rt == nil {
			return nil, fmt.Errorf("manager for runtime %q exposes no runtime", name)
		}
		own := &agentOwnRuntime{mgr: mgr, rt: rt, profile: profile, fingerprint: fp}
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

// runtimeOfManager returns the runtime behind mgr, or nil.
func (s *Server) runtimeOfManager(mgr agent.Manager) scionrt.Runtime {
	if mgr == s.manager {
		return s.runtime
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
