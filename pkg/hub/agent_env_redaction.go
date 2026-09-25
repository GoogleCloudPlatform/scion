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

package hub

import (
	"context"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// redactAppliedConfigEnvForResponse returns the AgentAppliedConfig that
// should be serialized into an API response, given whether the requesting
// identity has attach-equivalent access to the agent.
//
// Rows written before the persistence fix (or by any write path that isn't
// audited against the env classification map) may still contain values
// that were never actually plain: there is no per-row record of the
// original classification, so an existing key cannot be trusted to be safe
// to redisplay just because it is present. The response layer therefore
// cannot rely on classification at all -- it applies an access-level gate
// instead:
//
//   - canAttach == false: the entire Env map is omitted. A caller who
//     cannot attach to the agent's container cannot read its live env
//     either, so withholding the persisted copy from them removes a read
//     path without removing any capability that didn't already exist
//     (mirrors the attach/port-access carve-out in ownerAdminExcludedActions,
//     capabilities.go).
//   - canAttach == true: every key is returned except GITHUB_TOKEN, which is
//     never included in a response regardless of viewer or classification --
//     it is not a legitimate value to keep surfacing from the durable config
//     record.
//
// The input AgentAppliedConfig is never mutated; a redacted response either
// reuses the original pointer (nothing to change) or returns a shallow copy
// with a fresh Env map, so callers holding the original agent object (e.g.
// to log it or pass it elsewhere) are unaffected.
func redactAppliedConfigEnvForResponse(ac *store.AgentAppliedConfig, canAttach bool) *store.AgentAppliedConfig {
	if ac == nil || len(ac.Env) == 0 {
		return ac
	}

	_, hasGitHubToken := ac.Env["GITHUB_TOKEN"]
	if canAttach && !hasGitHubToken {
		return ac
	}

	redacted := *ac
	redacted.Env = make(map[string]string, len(ac.Env))
	if canAttach {
		for k, v := range ac.Env {
			if k == "GITHUB_TOKEN" {
				continue
			}
			redacted.Env[k] = v
		}
	}
	if len(redacted.Env) == 0 {
		redacted.Env = nil
	}
	return &redacted
}

// canViewAgentEnv answers the same access-level question as
// redactAppliedConfigEnvForResponse's canAttach parameter, for handlers that
// serialize a raw *store.Agent without already computing a Capabilities set
// for it (e.g. lifecycle actions). It returns false for an unauthenticated
// caller.
func canViewAgentEnv(ctx context.Context, s *Server, agent *store.Agent) bool {
	identity := GetIdentityFromContext(ctx)
	if identity == nil || agent == nil {
		return false
	}
	return s.authzService.CheckAccess(ctx, identity, agentResource(agent), ActionAttach).Allowed
}

// redactedAgentCopy returns a shallow copy of agent with AppliedConfig passed
// through redactAppliedConfigEnvForResponse, for handlers that serialize a
// raw *store.Agent (directly, or embedded in a response envelope) without
// already computing a Capabilities set. The original agent is left
// untouched, so callers can keep using it (e.g. for further store writes)
// after building the response.
func redactedAgentCopy(ctx context.Context, s *Server, agent *store.Agent) *store.Agent {
	if agent == nil {
		return agent
	}
	redacted := *agent
	redacted.AppliedConfig = redactAppliedConfigEnvForResponse(agent.AppliedConfig, canViewAgentEnv(ctx, s, agent))
	return &redacted
}
