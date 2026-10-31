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
	"errors"
	"fmt"
	"net/http"
	"path"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// projectPathNotInitializedErrorCode is the runtime broker's 409 code for a
// plain project whose explicit project path (the project's provider
// LocalPath on that broker) holds no .scion directory or marker
// (ptone/scion#6647).
const projectPathNotInitializedErrorCode = api.BrokerErrCodeProjectPathNotInitialized

// projectPathRefusalGeneric is the hub's text for the broker's
// project_path_not_initialized refusal when no path may be shown. The
// broker's own message is never relayed: it is broker text, and it names a
// host path.
const projectPathRefusalGeneric = "the project path configured for this project on the Runtime Broker is not initialized as a project; a project or broker admin must create a .scion directory inside the project path (not a symlink to another location) and retry"

// isProjectPathNotInitialized reports whether err is a runtime broker's 409
// project_path_not_initialized.
func isProjectPathNotInitialized(err error) bool {
	var se *brokerStatusError
	return errors.As(err, &se) && se.StatusCode == http.StatusConflict &&
		se.brokerErrorCode() == projectPathNotInitializedErrorCode
}

// projectPathRefusalText is the hub-generated text for the refusal. It names
// the broker and the stored provider LocalPath (never a path the broker
// resolved) when localPath is set, and drops the path otherwise.
func projectPathRefusalText(brokerName, localPath string) string {
	if brokerName == "" {
		brokerName = "the Runtime Broker"
	} else {
		brokerName = "Runtime Broker " + brokerName
	}
	if localPath == "" {
		return fmt.Sprintf("the project path configured for this project on %s is not initialized as a project; a project or broker admin must create a .scion directory inside the project path (not a symlink to another location) and retry", brokerName)
	}
	scionDir := localPath
	if path.Base(localPath) != ".scion" {
		scionDir = path.Join(localPath, ".scion")
	}
	return fmt.Sprintf("the project path configured for this project on %s is not initialized as a project (no .scion directory or marker inside it); a project or broker admin must create %s as a directory (not a symlink to another location) and retry",
		brokerName, scionDir)
}

// relayProjectPathRefusal answers a runtime broker's 409
// project_path_not_initialized for agent as 409 with that code and a
// hub-generated text, and reports whether it did. The text names the
// provider's stored LocalPath only for a caller that may already read the
// project's provider records (GET /api/v1/projects/{id}/providers shows
// LocalPath to every caller with read access to the project); for any other
// caller, or when a lookup fails, the path is left out.
func (s *Server) relayProjectPathRefusal(ctx context.Context, w http.ResponseWriter, agent *store.Agent, err error) bool {
	if !isProjectPathNotInitialized(err) {
		return false
	}
	writeError(w, http.StatusConflict, projectPathNotInitializedErrorCode, s.projectPathRefusalFor(ctx, agent), nil)
	return true
}

// projectPathRefusalFor builds projectPathRefusalText for agent and the
// caller in ctx (see relayProjectPathRefusal).
func (s *Server) projectPathRefusalFor(ctx context.Context, agent *store.Agent) string {
	if agent == nil || agent.RuntimeBrokerID == "" {
		return projectPathRefusalGeneric
	}
	brokerName := ""
	if b, err := s.store.GetRuntimeBroker(ctx, agent.RuntimeBrokerID); err == nil && b != nil {
		brokerName = b.Name
	}
	localPath := ""
	if s.callerMayReadProviders(ctx, agent.ProjectID) {
		if p, err := s.store.GetProjectProvider(ctx, agent.ProjectID, agent.RuntimeBrokerID); err == nil && p != nil {
			localPath = p.LocalPath
		}
	}
	return projectPathRefusalText(brokerName, localPath)
}

// callerMayReadProviders reports whether the caller in ctx passes the check
// that gates reading projectID's provider records, as handleProjectProviders
// checks it: an agent caller's project and standing, then read access to the
// project.
func (s *Server) callerMayReadProviders(ctx context.Context, projectID string) bool {
	identity := GetIdentityFromContext(ctx)
	if identity == nil || projectID == "" || s.authzService == nil {
		return false
	}
	// An agent caller passes only as handleProjectProviders admits it: its
	// own project, and its standing (agentStanding) intact.
	if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
		if agentIdent.ProjectID() != projectID || s.agentStanding(ctx, agentIdent.ID()) != nil {
			return false
		}
	}
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil || project == nil {
		return false
	}
	return s.authzService.CheckAccess(ctx, identity, projectResource(project), ActionRead).Allowed
}
