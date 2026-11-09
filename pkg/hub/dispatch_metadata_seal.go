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
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The broker trusts the SCION_METADATA_* keys of a dispatch's resolved env
// (with SCION_METADATA_MODE_SOURCE=hub) for the agent's GCP identity. So
// the hub seals the resolved env of every create and start request at the
// point it hands the request to the broker transport: the send wrappers
// below are the only callers of RuntimeBrokerClient.CreateAgent,
// CreateAgentWithGather, StartAgent and RestartAgent, and each seals first
// (sealCreateRequest with the request's own GCPIdentity mode, or
// sealDispatchMetadataEnv with the agent's applied identity for a start or
// restart). A merge on any path before the send,
// present or future, cannot survive it.
// TestDispatchSendSitesSealMetadataEnv enumerates the transport calls and
// fails if one bypasses the wrappers.

// sealDispatchMetadataEnv seals the resolved env of a start or restart of
// agent: it is sealMetadataEnv with the mode, account, project and
// local-runtime flag of agent's applied GCP identity, which is all a start
// or restart carries of it.
func sealDispatchMetadataEnv(ctx context.Context, agent *store.Agent, env map[string]string, cls *map[string]api.EnvKind) {
	var gcpID *store.GCPIdentityConfig
	if agent != nil && agent.AppliedConfig != nil {
		gcpID = agent.AppliedConfig.GCPIdentity
	}
	mode := ""
	if gcpID != nil {
		mode = gcpID.MetadataMode
	}
	sealMetadataEnv(ctx, env, cls, mode, gcpID)
}

// sealMetadataEnv is the last step on a dispatch's resolved env before it
// is sent: it deletes every SCION_METADATA_* key (clearMergedMetadataEnv),
// then writes only the hub's own decision:
//
//   - with start (a start or restart, which has no GCPIdentity config), the
//     assigned account's email and project for mode "assign", and the
//     local-runtime flag of a hub-default passthrough grant; a create
//     carries both in its GCPIdentity config instead and passes nil;
//   - mode as the metadata mode, and SCION_METADATA_MODE_SOURCE=hub
//     (applyHubGCPMetadataModeEnv);
//   - the native Cloud Run marker, only when this dispatch was decided
//     native (applyNativeCloudRunEnv).
//
// It is idempotent: sealing a sealed env changes nothing.
func sealMetadataEnv(ctx context.Context, env map[string]string, cls *map[string]api.EnvKind, mode string, start *store.GCPIdentityConfig) {
	if env == nil {
		return
	}
	if cls == nil {
		cls = new(map[string]api.EnvKind)
	}
	clearMergedMetadataEnv(env, cls)
	if start != nil {
		if mode == store.GCPMetadataModeAssign {
			env["SCION_METADATA_SA_EMAIL"] = start.ServiceAccountEmail
			classifyEnv(cls, "SCION_METADATA_SA_EMAIL", api.EnvKindPlain)
			env["SCION_METADATA_PROJECT_ID"] = start.ProjectID
			classifyEnv(cls, "SCION_METADATA_PROJECT_ID", api.EnvKindPlain)
		}
		if start.RequireLocalRuntime {
			env["SCION_METADATA_REQUIRE_LOCAL_RUNTIME"] = "true"
			classifyEnv(cls, "SCION_METADATA_REQUIRE_LOCAL_RUNTIME", api.EnvKindPlain)
		}
	}
	applyHubGCPMetadataModeEnv(env, cls, mode)
	applyNativeCloudRunEnv(env, cls, cloudRunNativeFrom(ctx))
}

// clearMergedMetadataEnv deletes every SCION_METADATA_* key from a
// dispatch's resolved env and its classification. sealDispatchMetadataEnv
// calls it before it writes the hub's own GCP identity decision.
func clearMergedMetadataEnv(env map[string]string, cls *map[string]api.EnvKind) {
	for k := range env {
		if !strings.HasPrefix(k, "SCION_METADATA_") {
			continue
		}
		delete(env, k)
		if cls != nil && *cls != nil {
			delete(*cls, k)
		}
	}
}

// sendCreate seals req's resolved env and sends the create.
func (d *HTTPAgentDispatcher) sendCreate(ctx context.Context, agent *store.Agent, endpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, error) {
	sealCreateRequest(ctx, req)
	return d.client.CreateAgent(ctx, agent.RuntimeBrokerID, endpoint, req)
}

// sendCreateWithGather seals req's resolved env and sends the create with
// env gathering.
func (d *HTTPAgentDispatcher) sendCreateWithGather(ctx context.Context, agent *store.Agent, endpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
	sealCreateRequest(ctx, req)
	return d.client.CreateAgentWithGather(ctx, agent.RuntimeBrokerID, endpoint, req)
}

// sendStart seals resolvedEnv and sends the start.
func (d *HTTPAgentDispatcher) sendStart(ctx context.Context, agent *store.Agent, endpoint, agentID, projectID, task, projectPath, projectSlug, harnessConfig, harnessConfigID, harnessConfigHash string, resolvedEnv map[string]string, resolvedSecrets []ResolvedSecret, inlineConfig *api.ScionConfig, sharedDirs []api.SharedDir, sharedWorkspace, resume bool, extras StartExtras) (*RemoteAgentResponse, error) {
	sealDispatchMetadataEnv(ctx, agent, resolvedEnv, &extras.EnvClassifications)
	return d.client.StartAgent(ctx, agent.RuntimeBrokerID, endpoint, agentID, projectID, task, projectPath, projectSlug, harnessConfig, harnessConfigID, harnessConfigHash, resolvedEnv, resolvedSecrets, inlineConfig, sharedDirs, sharedWorkspace, resume, extras)
}

// sendRestart seals resolvedEnv and sends the restart.
func (d *HTTPAgentDispatcher) sendRestart(ctx context.Context, agent *store.Agent, endpoint, agentID, projectID string, resolvedEnv map[string]string, extras StartExtras) (*RemoteAgentResponse, error) {
	sealDispatchMetadataEnv(ctx, agent, resolvedEnv, &extras.EnvClassifications)
	return d.client.RestartAgent(ctx, agent.RuntimeBrokerID, endpoint, agentID, projectID, resolvedEnv, extras)
}

// sealCreateRequest seals a create request's resolved env with the mode of
// the request's own GCPIdentity config (the identity the broker is sent),
// creating the env when absent so the hub's own source marker is always
// sent.
func sealCreateRequest(ctx context.Context, req *RemoteCreateAgentRequest) {
	if req == nil {
		return
	}
	if req.ResolvedEnv == nil {
		req.ResolvedEnv = map[string]string{}
	}
	mode := ""
	if req.Config != nil && req.Config.GCPIdentity != nil {
		mode = req.Config.GCPIdentity.MetadataMode
	}
	sealMetadataEnv(ctx, req.ResolvedEnv, &req.EnvClassifications, mode, nil)
}
