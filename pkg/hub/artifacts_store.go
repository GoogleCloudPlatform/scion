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
	"crypto/rand"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
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
	c := s.artifactsConfig()
	return artifacts.Limits{
		MaxFileBytes: c.MaxFileBytes, MaxBundleBytes: c.MaxBundleBytes, MaxFiles: c.MaxFiles,
		RemoteImages: artifacts.RemoteImageLimits{
			Enabled:      c.RemoteImagesEnabled,
			MaxCount:     c.RemoteImageMaxCount,
			MaxBytes:     c.RemoteImageMaxBytes,
			FetchTimeout: time.Duration(c.RemoteImageFetchTimeoutS) * time.Second,
			TotalBudget:  time.Duration(c.RemoteImageTotalBudgetS) * time.Second,
		},
	}
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
	svc.SetReviewNotifier(s.notifyArtifactReview)
	return svc.Handler()
}

// artifactRefResolver returns the artifact service for resolving message
// references (resolveArtifactRefs, its only caller). Like artifactsHandler
// it is built over this server's artifacts.Host; resolveArtifactRefs
// admits only request-authenticated contexts, so the host still sees only
// identities derived from request credentials.
func (s *Server) artifactRefResolver() *artifacts.Service {
	svc := artifacts.NewService(newArtifactHost(s))
	svc.SetBackendProvider(s.artifactBackend)
	return svc
}

// artifactBackend reports the server's current artifact store, resource
// storage and hub id.
func (s *Server) artifactBackend() artifacts.Backend {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return artifacts.Backend{Store: s.artifactStore, Blobs: s.storage, HubID: s.hubID, ViewKey: s.artifactViewKey}
}

// artifactReapInterval is how often the hub reaps abandoned pending
// artifact versions.
const artifactReapInterval = 10 * time.Minute

// artifactReapBatch caps the versions one reap pass handles.
const artifactReapBatch = 500

// startArtifactReaper reaps pending artifact versions older than
// artifacts.PendingVersionTTL every artifactReapInterval until ctx ends.
// It runs whether or not the experiment is on: reaping only retires
// abandoned uploads.
func (s *Server) startArtifactReaper(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(artifactReapInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.reapArtifactVersions(ctx)
			}
		}
	}()
}

func (s *Server) reapArtifactVersions(ctx context.Context) {
	st := s.ArtifactStore()
	if st == nil {
		return
	}
	n, err := st.ReapPending(ctx, time.Now().Add(-artifacts.PendingVersionTTL), artifactReapBatch)
	if err != nil {
		slog.WarnContext(ctx, "artifacts: reaping pending versions failed", "error", err)
		return
	}
	if n > 0 {
		slog.InfoContext(ctx, "artifacts: reaped abandoned pending versions", "count", n)
	}
}

// SecretKeyArtifactViewKey is the secret key name of the key that signs
// artifact view capabilities. It is separate from every other hub key.
const SecretKeyArtifactViewKey = "artifact_view_signing_key"

// initArtifactViewKey loads or creates the artifact view capability key,
// with the same stable-key policy as the download signing key: required
// on deployments that need stable keys, otherwise an in-memory key, since
// losing it only ends views within artifacts.ViewTTL.
func (s *Server) initArtifactViewKey(ctx context.Context) error {
	key, err := s.ensureSigningKey(ctx, SecretKeyArtifactViewKey, nil)
	if err == nil && len(key) == 0 {
		err = fmt.Errorf("artifact view key resolved to an empty value")
	}
	if err != nil {
		_, isGCPBackend := s.secretBackend.(*secret.GCPBackend)
		if isGCPBackend || s.config.RequireStableSigningKey {
			return fmt.Errorf("artifact view key: %w", err)
		}
		slog.Warn("Artifact view key could not be loaded or persisted; using an ephemeral in-memory key "+
			"(artifact views will not load on other replicas or after restart)", "error", err)
		key = make([]byte, 32)
		if _, rerr := rand.Read(key); rerr != nil {
			return fmt.Errorf("generate ephemeral artifact view key: %w", rerr)
		}
	}
	s.mu.Lock()
	s.artifactViewKey = key
	s.mu.Unlock()
	return nil
}
