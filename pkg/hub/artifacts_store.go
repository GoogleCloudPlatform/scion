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
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
)

// SetArtifactStore installs the artifact service's store. The store owns the
// artifact_* tables, created by its Init outside the Ent migration graph
// (design D3). Until a store is set the artifact routes answer 503.
func (s *Server) SetArtifactStore(st artifacts.Store) {
	s.mu.Lock()
	s.artifactStore = st
	s.mu.Unlock()
}

// ArtifactStore returns the artifact store, or nil when none is set.
func (s *Server) ArtifactStore() artifacts.Store {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.artifactStore
}

// artifactsConfig is the resolved artifacts settings section: the
// operational settings when the hub has them (Postgres mode), otherwise the
// compiled defaults.
func (s *Server) artifactsConfig() opsettings.ArtifactsConfig {
	if ops := s.GetOperationalSettings(); ops != nil {
		return ops.Artifacts()
	}
	return opsettings.DefaultArtifactsConfig()
}

// artifactLimits feeds the artifacts settings section to the service on
// every write, so a limit change applies without a restart.
func (s *Server) artifactLimits(context.Context) artifacts.Limits {
	return artifacts.Limits{MaxFileBytes: s.artifactsConfig().MaxFileBytes}
}

// artifactsHandler returns the artifact service's handler, built over this
// server's artifacts.Host.
//
// The service asks the server for its store, resource storage and hub id on
// each request: they are set after the routes are registered (the database,
// storage and hub id come later in startup, and tests set them in any
// order). Blobs live in the hub's resource storage under
// hubs/{hub-id}/artifacts/.
func (s *Server) artifactsHandler() http.Handler {
	svc := artifacts.NewService(newArtifactHost(s))
	svc.SetLimits(s.artifactLimits)
	svc.SetBackendProvider(s.artifactBackend)
	return svc.Handler()
}

// artifactBackend reports the server's current artifact store, resource
// storage and hub id.
func (s *Server) artifactBackend() artifacts.Backend {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return artifacts.Backend{Store: s.artifactStore, Blobs: s.storage, HubID: s.hubID}
}
