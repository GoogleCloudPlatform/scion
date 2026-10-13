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
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// validatePublicURLRequest checks the server.hub.public_url a settings
// database save sends with config.ValidatePublicURL, normalizes it in place,
// and writes a 422 validation_error naming the key when it fails. An omitted
// key, an explicit "" (clear) and an echo of current (the value the GET view
// shows) pass without the check; the echo is then dropped from the save
// (echoedPublicURL), so it is never stored. It reports whether the request
// may continue.
func validatePublicURLRequest(w http.ResponseWriter, req *ServerConfigUpdateRequest, current string) bool {
	if req == nil || req.Server == nil || req.Server.Hub == nil {
		return true
	}
	sent := req.Server.Hub.PublicURL
	if sent == "" || samePublicURL(sent, current) {
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

// storedManagedPublicURL returns the public_url an admin saved in the
// endpoints row, or "" when there is no row or the row is seeded. A seeded
// row holds bootstrap material (settings.yaml and the environment), not a
// saved value, so clearing or resetting it changes no stored value. By the
// time a request runs, startup has applied BackfillOrigin, so the origin
// column is authoritative; an empty origin (a row predating it) counts as
// managed, as BackfillOrigin would mark it.
func storedManagedPublicURL(ops *OperationalSettings) string {
	if ops == nil {
		return ""
	}
	ops.mu.RLock()
	state, ok := ops.cache["endpoints"]
	ops.mu.RUnlock()
	return managedPublicURL(state, ok)
}

// managedPublicURL returns the public_url of the endpoints cache entry
// state (ok reports whether there is one) when the entry is managed, or "".
// See storedManagedPublicURL for the origin rule.
func managedPublicURL(state sectionState, ok bool) string {
	if !ok || state.Origin == "seeded" {
		return ""
	}
	return publicURLFromSectionDoc(state.Value)
}

// echoedPublicURL reports whether a settings save sends back the public URL
// the GET view shows (Snapshot's PublicURL: the saved value, or else the
// bootstrap value without the SCION_SEED_* layer). Such a value is an echo,
// not a change: it is dropped from the save so a bootstrap value is never
// stored as a saved value, and a later settings.yaml change still takes
// effect (ptone/scion#3905). A seed value, which startup ignores, is not an
// echo (ptone/scion#4826).
func echoedPublicURL(ops *OperationalSettings, req *ServerConfigUpdateRequest) bool {
	if ops == nil || req == nil || req.Server == nil || req.Server.Hub == nil {
		return false
	}
	sent := req.Server.Hub.PublicURL
	return sent != "" && samePublicURL(sent, ops.Snapshot().PublicURL)
}

// samePublicURL reports whether a and b name the same public URL: equal
// after config.ValidatePublicURL normalisation, or, when either does not
// normalise, equal after trimming a trailing slash.
func samePublicURL(a, b string) bool {
	na, errA := config.ValidatePublicURL(a)
	nb, errB := config.ValidatePublicURL(b)
	if errA == nil && errB == nil {
		return na == nb
	}
	return strings.TrimRight(a, "/") == strings.TrimRight(b, "/")
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

// publicURLInEffectDoc returns the endpoints document doc with public_url
// set to the public URL the hub resolves at its next start, for comparing
// the value in effect before and after a save: saved, the public_url saved
// in a managed endpoints row, or with none saved, operator, the operator
// endpoint (HubEndpoints().Operator: environment, flags, settings.yaml and
// fallbacks). The bootstrap material is not used: it may hold a
// SCION_SEED_* value that startup ignores (StoredPublicURL,
// ptone/scion#4826). public_url is removed when both are "". doc is returned
// as it is when it cannot be decoded.
func publicURLInEffectDoc(doc json.RawMessage, saved, operator string) json.RawMessage {
	m := map[string]json.RawMessage{}
	if len(doc) > 0 {
		if err := json.Unmarshal(doc, &m); err != nil {
			return doc
		}
	}
	if m == nil {
		m = map[string]json.RawMessage{}
	}
	value := saved
	if value == "" {
		value = operator
	}
	if value == "" {
		delete(m, "public_url")
	} else {
		v, err := json.Marshal(value)
		if err != nil {
			return doc
		}
		m["public_url"] = v
	}
	out, err := json.Marshal(m)
	if err != nil {
		return doc
	}
	return out
}

// auditPublicURLChange records a change to the stored server.hub.public_url
// before it is written, through the hub settings audit path the GCP
// permission-check settings use: a hub_setting_update mutation audit record
// naming the key, the old and new stored values, and the actor. If the record
// cannot be written, it returns an error and the caller does not make the
// change. If the write then fails, the caller records that with
// auditPublicURLNotApplied.
func (s *Server) auditPublicURLChange(ctx context.Context, surface, before, after string) error {
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

// auditPublicURLNotApplied records that a change recorded by
// auditPublicURLChange was not written (the section write failed or hit a
// revision conflict), as auditGCPIAMNotApplied does for the GCP
// permission-check settings. A failed record is logged and does not change
// the response.
func (s *Server) auditPublicURLNotApplied(ctx context.Context, surface, before, after string, cause error) {
	slog.Error("Hub public URL setting change was recorded but not written",
		"key", config.PublicURLKey, "new", after, "error", cause)
	rec := &store.MutationAuditRecord{
		MutationType:  gcpIAMNotAppliedMutation, // hub_setting_update_not_applied
		TargetType:    gcpIAMAuditTarget,
		TargetID:      config.PublicURLKey,
		BeforeSummary: before,
		AfterSummary:  after,
		ExecutorKind:  gcpIAMSurfaceKind,
		ExecutorID:    surface,
	}
	auditActorFromContext(ctx).ApplyActor(rec)
	rec.Timestamp = time.Now().UTC()
	if s.store == nil {
		slog.Warn("failed to emit mutation audit record",
			"mutation_type", rec.MutationType, "error", "no store")
		return
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditWriteTimeout)
	defer cancel()
	if err := s.store.CreateMutationAudit(writeCtx, rec); err != nil {
		slog.Warn("failed to emit mutation audit record",
			"mutation_type", rec.MutationType, "error", err)
	}
}
