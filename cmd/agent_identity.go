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

package cmd

import (
	"context"
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// agentGCPIdentity converts the GCP identity the Hub applied to an agent
// (appliedConfig.gcpIdentity) into the CLI's view. Nil when the Hub recorded
// none.
func agentGCPIdentity(a hubclient.Agent) *api.AgentGCPIdentity {
	if a.AppliedConfig == nil || a.AppliedConfig.GCPIdentity == nil {
		return nil
	}
	id := a.AppliedConfig.GCPIdentity
	return &api.AgentGCPIdentity{
		Mode:                id.MetadataMode,
		ServiceAccountID:    id.ServiceAccountID,
		ServiceAccountEmail: id.ServiceAccountEmail,
	}
}

// serviceAccountDisplayNames returns the display names of the service
// accounts assignable in each given Scion project, keyed by service account
// ID. It is best effort: a project whose accounts the caller cannot read
// contributes nothing, and callers fall back to the email.
func serviceAccountDisplayNames(ctx context.Context, client hubclient.Client, projectIDs []string) map[string]string {
	names := make(map[string]string)
	seen := make(map[string]bool)
	for _, pid := range projectIDs {
		if pid == "" || seen[pid] {
			continue
		}
		seen[pid] = true
		accounts, err := client.GCPServiceAccounts().List(ctx, hubclient.ListForProjectIncludingHubScoped(pid))
		if err != nil {
			continue
		}
		for _, sa := range accounts {
			if sa.DisplayName != "" {
				names[sa.ID] = sa.DisplayName
			}
		}
	}
	return names
}

// fillGCPIdentityDisplayNames sets DisplayName on each agent's assigned GCP
// identity from the service account registrations the caller can read.
func fillGCPIdentityDisplayNames(ctx context.Context, client hubclient.Client, agents []api.AgentInfo) {
	var projectIDs []string
	for _, a := range agents {
		if a.GCPIdentity != nil && a.GCPIdentity.ServiceAccountID != "" && a.GCPIdentity.DisplayName == "" {
			projectIDs = append(projectIDs, a.ProjectID)
		}
	}
	if len(projectIDs) == 0 {
		return
	}
	names := serviceAccountDisplayNames(ctx, client, projectIDs)
	for i := range agents {
		id := agents[i].GCPIdentity
		if id == nil || id.DisplayName != "" {
			continue
		}
		if name, ok := names[id.ServiceAccountID]; ok {
			id.DisplayName = name
		}
	}
}

// formatGCPIdentity renders an identity for human output: the mode, plus the
// account for "assign", preferring the display name over the email.
//
// TODO(ptone/scion#4034): once the Hub reports the Kubernetes block guarantee
// level, render it here, for example "block (block KSA)" or
// "block (namespace default KSA)". Until then block shows the plain mode.
func formatGCPIdentity(id *api.AgentGCPIdentity) string {
	if id == nil || id.Mode == "" {
		return "none"
	}
	if id.Mode != "assign" {
		return id.Mode
	}
	account := id.DisplayName
	if account == "" {
		account = id.ServiceAccountEmail
	}
	if account == "" {
		return "assign (unknown account)"
	}
	return fmt.Sprintf("assign as %q", account)
}

// lookIdentityHeader is the one-line identity header scion look prints before
// the terminal output: mode, account and profile. Empty when the Hub recorded
// no identity.
func lookIdentityHeader(id *api.AgentGCPIdentity, profile string) string {
	if id == nil || id.Mode == "" {
		return ""
	}
	header := "GCP identity: " + formatGCPIdentity(id)
	if profile != "" {
		header += fmt.Sprintf(" (profile: %s)", profile)
	}
	return header
}
