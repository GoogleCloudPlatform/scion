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
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/brokersettings"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// Broker settings API types (design.md §5.4)
// ---------------------------------------------------------------------------

// EffectiveSetting is the resolved value of one broker-settings key plus the
// precedence step that produced it (design.md §5.2, §5.9).
type EffectiveSetting struct {
	// Value is nil only when resolution could not determine a value at all
	// (no quota service configured, or no matching limit definition); a
	// resolved-but-unlimited value is reported as 0, not omitted, so the UI
	// can always show a number next to Source.
	Value  *int64 `json:"value"`
	Source string `json:"source"`
}

// BrokerSettingsEffective holds the effective value for every registered
// broker-settings key. New keys get a new field here alongside a new field
// on store.BrokerSettings and a new brokersettings.KeyDef.
type BrokerSettingsEffective struct {
	MaxAgents EffectiveSetting `json:"maxAgents"`
}

// brokerSettingsCapabilities reports, per key, whether the caller may write
// it — design.md §5.4's `_capabilities` shape. Kept local to this file
// rather than reusing the generic Capabilities{Actions} type: the broker
// settings document is keyed, not action-keyed, and per-key growth
// (design.md §5.1) fits a bool-per-key map better as new keys are added.
type brokerSettingsCapabilities struct {
	// Update is true when the caller holds every registered key's write
	// permission. P2.1 has one key (maxAgents, quota.update), so this is
	// exactly "can the caller write maxAgents"; a future PUT-permission
	// gated key with a different permission would need a per-key map here,
	// deferred until there is a second key to justify it.
	Update bool `json:"update"`
}

// BrokerSettingsResponse is the GET/PUT response body.
type BrokerSettingsResponse struct {
	BrokerID     string                     `json:"brokerId"`
	Settings     store.BrokerSettings       `json:"settings"`
	Effective    BrokerSettingsEffective    `json:"effective"`
	Revision     int64                      `json:"revision"`
	UpdatedBy    string                     `json:"updatedBy,omitempty"`
	Updated      time.Time                  `json:"updated"`
	Capabilities brokerSettingsCapabilities `json:"_capabilities"`
}

// brokerSettingsPutRequest is the PUT request body. Settings is decoded as a
// raw map, not directly into store.BrokerSettings, so that a key absent from
// the brokersettings registry can be rejected with 400 rather than silently
// dropped (encoding/json ignores unknown struct fields by default).
type brokerSettingsPutRequest struct {
	Settings         map[string]json.RawMessage `json:"settings"`
	ExpectedRevision int64                      `json:"expectedRevision"`
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

// handleBrokerSettings handles GET/PUT /api/v1/runtime-brokers/{id}/settings
// (ptone/scion#2061 P2, ptone/scion#2177, design.md §5.4).
func (s *Server) handleBrokerSettings(w http.ResponseWriter, r *http.Request, brokerID string) {
	switch r.Method {
	case http.MethodGet:
		s.handleGetBrokerSettings(w, r, brokerID)
	case http.MethodPut:
		s.handlePutBrokerSettings(w, r, brokerID)
	default:
		MethodNotAllowed(w, http.MethodGet, http.MethodPut)
	}
}

// handleGetBrokerSettings returns brokerID's settings document and effective
// values. Requires broker.read. 404 if the broker doesn't exist; settings={}
// and revision=0 when the broker has no settings row.
func (s *Server) handleGetBrokerSettings(w http.ResponseWriter, r *http.Request, brokerID string) {
	ctx := r.Context()

	broker, err := s.store.GetRuntimeBroker(ctx, brokerID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if !s.authorize(w, r, brokerResource(broker), ActionRead) {
		return
	}

	rec, err := s.store.GetBrokerSettings(ctx, brokerID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, s.buildBrokerSettingsResponse(r, brokerID, rec))
}

// handlePutBrokerSettings replaces brokerID's settings document. Each
// present key must be a known brokersettings key (400 otherwise), pass its
// own validation (400), and the caller must hold its declared write
// permission (403). The write is a full replace with optimistic concurrency
// (409 on a stale expectedRevision, with the current record in the body).
func (s *Server) handlePutBrokerSettings(w http.ResponseWriter, r *http.Request, brokerID string) {
	ctx := r.Context()

	broker, err := s.store.GetRuntimeBroker(ctx, brokerID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	// GET-level read access is a precondition for writing at all; the
	// per-key permission check below (quota.update for maxAgents) is the
	// real write gate (design.md §5.3).
	if !s.authorize(w, r, brokerResource(broker), ActionRead) {
		return
	}

	var req brokerSettingsPutRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	var settings store.BrokerSettings
	for key, raw := range req.Settings {
		def, ok := brokersettings.Lookup(key)
		if !ok {
			BadRequest(w, fmt.Sprintf("unknown broker setting %q", key))
			return
		}
		if !s.authorizeBrokerSettingWrite(w, r, def) {
			return
		}
		switch key {
		case brokersettings.MaxAgents.Name:
			var value *int64
			if err := json.Unmarshal(raw, &value); err != nil {
				BadRequest(w, fmt.Sprintf("invalid value for %q: must be a number or null", key))
				return
			}
			if err := brokersettings.ValidateMaxAgents(value); err != nil {
				BadRequest(w, err.Error())
				return
			}
			settings.MaxAgents = value
		}
	}

	updatedBy := ""
	if identity := GetIdentityFromContext(ctx); identity != nil {
		updatedBy = identity.ID()
	}

	rec, err := s.store.PutBrokerSettings(ctx, brokerID, settings, req.ExpectedRevision, updatedBy)
	if err != nil {
		if errors.Is(err, store.ErrRevisionConflict) {
			current, getErr := s.store.GetBrokerSettings(ctx, brokerID)
			if getErr != nil && !errors.Is(getErr, store.ErrNotFound) {
				RuntimeError(w, "Failed to load current broker settings")
				return
			}
			writeJSON(w, http.StatusConflict, map[string]interface{}{
				"error":   ErrCodeRevisionConflict,
				"message": "Broker settings were modified concurrently. Refresh and retry.",
				"current": s.buildBrokerSettingsResponse(r, brokerID, current),
			})
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	s.emitMutationAudit(ctx, &store.MutationAuditRecord{
		MutationType: "broker_settings_update",
		TargetType:   "runtime_broker",
		TargetID:     brokerID,
	})

	writeJSON(w, http.StatusOK, s.buildBrokerSettingsResponse(r, brokerID, rec))
}

// canWriteBrokerSettingKey reports whether identity holds def's declared
// write permission (design.md §5.3), without writing an HTTP response. Used
// both to gate the PUT handler and to compute _capabilities.update on GET.
func (s *Server) canWriteBrokerSettingKey(ctx context.Context, identity Identity, def brokersettings.KeyDef) bool {
	if identity == nil || s.authzService == nil {
		return false
	}
	decision := s.authzService.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(identity),
		Credential: credentialContextForIdentity(identity),
		Resource:   Resource{Type: "quota", ID: "hub"},
		Action:     ActionUpdate,
		Permission: def.Permission,
	})
	return decision.Allowed
}

// authorizeBrokerSettingWrite is canWriteBrokerSettingKey plus writing a
// 401/403 response on denial, mirroring s.authorize's shape (design.md
// §5.3: writing a key requires that key's declared permission — quota.update
// for maxAgents, checked against Resource{quota, hub} exactly as the
// existing quota handlers do).
func (s *Server) authorizeBrokerSettingWrite(w http.ResponseWriter, r *http.Request, def brokersettings.KeyDef) bool {
	identity := GetIdentityFromContext(r.Context())
	if identity == nil {
		Unauthorized(w)
		return false
	}
	if !s.canWriteBrokerSettingKey(r.Context(), identity, def) {
		logAuthzDenial(r, identity, Resource{Type: "quota", ID: "hub"}, ActionUpdate, "missing "+def.Permission)
		Forbidden(w)
		return false
	}
	return true
}

// buildBrokerSettingsResponse assembles the GET/PUT response for brokerID.
// rec is nil when the broker has no settings row (settings={}, revision=0).
// The effective value comes from brokerCapacity (broker_capacity.go), the
// same read model the providers listing and Reserve use (AC-P2-10); this
// never fails the request outright — a resolution failure just leaves
// Effective.MaxAgents.Value unset, matching how resolveBrokerCapacity treats
// per-provider failures in the providers listing.
func (s *Server) buildBrokerSettingsResponse(r *http.Request, brokerID string, rec *store.BrokerSettingsRecord) BrokerSettingsResponse {
	ctx := r.Context()

	resp := BrokerSettingsResponse{BrokerID: brokerID}
	if rec != nil {
		resp.Settings = rec.Settings
		resp.Revision = rec.Revision
		resp.UpdatedBy = rec.UpdatedBy
		resp.Updated = rec.Updated
	}

	limitDef := s.lookupAgentLimitDefinition(ctx)
	bc := s.brokerCapacity(ctx, brokerID, limitDef)
	resp.Effective.MaxAgents = EffectiveSetting{Source: bc.Source}
	if bc.Source != "" {
		// bc.Limit is nil to mean unlimited (providers-listing convention);
		// the settings API instead always shows a concrete number when
		// resolution succeeded, with 0 meaning unlimited (design.md §5.4).
		value := int64(0)
		if bc.Limit != nil {
			value = *bc.Limit
		}
		resp.Effective.MaxAgents.Value = &value
	}

	resp.Capabilities.Update = s.canWriteBrokerSettingKey(ctx, GetIdentityFromContext(ctx), brokersettings.MaxAgents)

	return resp
}
