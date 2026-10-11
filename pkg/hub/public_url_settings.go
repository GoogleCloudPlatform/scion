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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// validatePublicURLRequest checks the server.hub.public_url a settings
// database save sends with config.ValidatePublicURL, normalizes it in place,
// and writes a 422 validation_error naming the key when it fails. An omitted
// key, an explicit "" (clear) and an unchanged echo of current (the value the
// GET view shows) pass without the check, so saving another field does not
// fail over a stored value that predates the rule; such a value is still
// checked at startup. It reports whether the request may continue.
func validatePublicURLRequest(w http.ResponseWriter, req *ServerConfigUpdateRequest, current string) bool {
	if req == nil || req.Server == nil || req.Server.Hub == nil {
		return true
	}
	sent := req.Server.Hub.PublicURL
	if sent == "" || sent == current {
		return true
	}
	normalized, err := config.ValidatePublicURL(sent)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, ErrCodeValidationError, err.Error(), map[string]interface{}{
			"field": config.PublicURLKey,
		})
		return false
	}
	req.Server.Hub.PublicURL = normalized
	return true
}

// PublicURLStatus reports how this hub resolved its endpoint at startup, for
// the admin settings view. The endpoint is restart-required, so after a
// save it can differ from the stored server.hub.public_url until the next
// restart.
type PublicURLStatus struct {
	// Effective is the hub endpoint in effect.
	Effective string `json:"effective"`
	// Source is where Effective came from: "env", "flag", "database",
	// "file", or a fallback label such as "localhost".
	Source string `json:"source"`
	// PinnedBy names the operator setting that pins the endpoint
	// (SCION_SERVER_HUB_ENDPOINT, --base-url or SCION_SERVER_BASE_URL), or
	// "" when none does. When set, the field is read-only in the admin UI.
	PinnedBy string `json:"pinned_by,omitempty"`
}

// publicURLStatus returns the startup resolution of the hub endpoint.
func (s *Server) publicURLStatus() *PublicURLStatus {
	e := s.HubEndpoints()
	return &PublicURLStatus{Effective: e.Public, Source: e.PublicSource, PinnedBy: e.Pin}
}

// storedPublicURL returns the public_url stored in the endpoints row, or ""
// when no row is stored (bootstrap values are not stored values).
func storedPublicURL(ops *OperationalSettings) string {
	if ops == nil {
		return ""
	}
	ops.mu.RLock()
	state, ok := ops.cache["endpoints"]
	ops.mu.RUnlock()
	if !ok {
		return ""
	}
	return publicURLFromSectionDoc(state.Value)
}

// publicURLFromSectionDoc returns public_url from an endpoints section
// document, or "" when the document is empty or does not set it.
func publicURLFromSectionDoc(doc json.RawMessage) string {
	if len(doc) == 0 {
		return ""
	}
	var d struct {
		PublicURL string `json:"public_url"`
	}
	if err := json.Unmarshal(doc, &d); err != nil {
		return ""
	}
	return d.PublicURL
}

// auditPublicURLChange records a change to the stored server.hub.public_url
// through the hub settings audit path: a hub_setting_update mutation audit
// record naming the key, the old and new stored values, and the actor. It
// does nothing when the value does not change. A record that cannot be
// written is logged and does not block the save; the settings row still
// carries updated_by.
func (s *Server) auditPublicURLChange(ctx context.Context, surface, before, after string) {
	if before == after {
		return
	}
	if err := s.writePublicURLAudit(ctx, surface, before, after); err != nil {
		slog.Error("Failed to record the hub public URL change in the audit log", "error", err)
	}
}

func (s *Server) writePublicURLAudit(ctx context.Context, surface, before, after string) error {
	if s.store == nil {
		return errors.New("no store for the audit record")
	}
	rec := &store.MutationAuditRecord{
		MutationType:  gcpIAMAuditMutation, // hub_setting_update
		TargetType:    gcpIAMAuditTarget,   // hub_setting
		TargetID:      config.PublicURLKey,
		BeforeSummary: before,
		AfterSummary:  after,
		ExecutorKind:  gcpIAMSurfaceKind,
		ExecutorID:    surface,
	}
	auditActorFromContext(ctx).ApplyActor(rec)
	if rec.ActorPrincipalKind == "" || rec.ActorPrincipalID == "" {
		return fmt.Errorf("recording %s change: no actor", config.PublicURLKey)
	}
	rec.Timestamp = time.Now().UTC()
	if err := s.store.CreateMutationAudit(ctx, rec); err != nil {
		return fmt.Errorf("recording %s change: %w", config.PublicURLKey, err)
	}
	slog.Info("Hub public URL setting changed; applies after restart",
		"key", config.PublicURLKey, "old", before, "new", after, "surface", surface,
		"principal_kind", rec.ActorPrincipalKind, "principal", rec.ActorPrincipalID,
		"audit_id", rec.ID)
	return nil
}
