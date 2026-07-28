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
	"errors"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Top-level, scope-addressed GCP service account routes (P4 item C).
//
// /api/v1/projects/{id}/gcp-service-accounts can only ever name one project's
// accounts, which was sufficient while a project was the only thing an account
// could belong to. Hub-scoped accounts have no project to be nested under, so
// they need a route that takes the scope as a parameter instead of encoding it
// in the path. The nested routes stay exactly as they are: they remain the
// natural address for a project's own accounts and every existing client uses
// them.
//
// The API contract here is sa-arch's, binding on P4 and P5:
//   - flat /api/v1/gcp-service-accounts, param spelled scopeId
//   - hub scope is the value "hub", matching store.ScopeHub. It is deliberately
//     NOT normalised to "global" to match TemplateScopeGlobal -- the two
//     vocabularies stay distinct rather than being papered over here
//   - scopeId is omitted on hub scope and resolved by the server

// gcpScopeRequest is a parsed and validated scope selector from the query
// string.
type gcpScopeRequest struct {
	scope   string
	scopeID string

	// includeHubScoped widens a project-scoped read to also return hub-scoped
	// accounts. Only meaningful with scope=project.
	includeHubScoped bool
}

// parseGCPScopeRequest reads scope/scopeId/includeHubScoped from the query
// string and validates them, writing the error response itself and returning
// false when the caller must stop.
//
// Validation, not coercion. Every rejected combination below could instead be
// silently repaired -- default the scope, ignore the stray parameter, overwrite
// the client's scopeId -- and each repair would turn a caller's mistake into a
// wrong answer delivered with a 200. The specific hazard is a request that
// means one thing to the client and another to the server: a hub-scope read
// carrying a stale scopeId, quietly ignored, returns a list the client believes
// is filtered.
func (s *Server) parseGCPScopeRequest(w http.ResponseWriter, r *http.Request) (gcpScopeRequest, bool) {
	query := r.URL.Query()
	scope := query.Get("scope")
	scopeID := query.Get("scopeId")
	_, scopeIDPresent := query["scopeId"]
	includeHubScoped := query.Get("includeHubScoped") == "true"

	// scope is required rather than defaulting to "everything". An unfiltered
	// list here would be a cross-project enumeration of every service account
	// on the hub, which is not something any existing route offers and not
	// something this phase should quietly introduce.
	switch scope {
	case "":
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			"missing required parameter: scope (expected \"project\" or \"hub\")", nil)
		return gcpScopeRequest{}, false

	case store.ScopeProject:
		if scopeID == "" {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
				"scopeId is required when scope=project", nil)
			return gcpScopeRequest{}, false
		}

	case store.ScopeHub:
		// The hub's scope ID is the hub instance ID, which the server knows and
		// the client does not. Accepting one from the client would let a
		// request name a hub that is not this one; the server resolving it
		// makes that unrepresentable rather than merely discouraged.
		if scopeIDPresent {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
				"scopeId must be omitted when scope=hub; the server resolves it", nil)
			return gcpScopeRequest{}, false
		}

		// Resolved for capability computation and, once P4 item A opens the
		// write path, for the value stored on a new hub-scoped account.
		//
		// INVARIANT (sa-arch, binding across all three tracks): "hub-scoped" is
		// determined by Scope ALONE. No code compares a service account's
		// ScopeID against the hub ID. On a hub-scoped account ScopeID is
		// PROVENANCE -- a record of which hub instance registered it -- and
		// never a predicate. s.hubID comes from config or a hostname hash and
		// is not stable across a redeploy, so a filter keyed on it would orphan
		// every hub-scoped account the first time a hostname changed, and would
		// do it silently, as an empty list.
		//
		// It is still written rather than left empty: an empty ScopeID collides
		// with the parentless overload in #604.
		scopeID = s.hubID

	default:
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			"invalid scope: expected \"project\" or \"hub\"", nil)
		return gcpScopeRequest{}, false
	}

	// Rejected rather than ignored: with scope=hub the flag is already implied,
	// and with any other scope it asks for a union that is not defined. Either
	// way the client has said something it does not mean, and silence would
	// hide that.
	if includeHubScoped && scope != store.ScopeProject {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			"includeHubScoped is only valid with scope=project", nil)
		return gcpScopeRequest{}, false
	}

	return gcpScopeRequest{scope: scope, scopeID: scopeID, includeHubScoped: includeHubScoped}, true
}

// handleGCPServiceAccounts handles /api/v1/gcp-service-accounts.
func (s *Server) handleGCPServiceAccounts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listGCPServiceAccountsScoped(w, r)
	case http.MethodPost:
		s.createGCPServiceAccountScoped(w, r)
	default:
		MethodNotAllowed(w)
	}
}

// listGCPServiceAccountsScoped lists service accounts for an explicit scope.
func (s *Server) listGCPServiceAccountsScoped(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	req, ok := s.parseGCPScopeRequest(w, r)
	if !ok {
		return
	}

	filter := store.GCPServiceAccountFilter{
		Scope:            req.scope,
		IncludeHubScoped: req.includeHubScoped,
	}
	if req.scope == store.ScopeProject {
		filter.ScopeID = req.scopeID

		// A missing project is a 404 rather than an empty list. The two are
		// indistinguishable to a caller otherwise, and a typo'd project ID
		// silently reading as "this project has no service accounts" is the
		// kind of answer that gets believed.
		if _, err := s.store.GetProject(ctx, req.scopeID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				NotFound(w, "Project")
				return
			}
			writeErrorFromErr(w, err, "")
			return
		}
	}
	// Note what is NOT set for hub scope: ScopeID stays empty, so the filter
	// matches on Scope alone. This is deliberate and matches the OR arm of
	// IncludeHubScoped, so the hub list and the hub half of a project union
	// always agree. Pinning it to s.hubID instead would make the two disagree
	// whenever a stored row's ScopeID differed -- and hubID is derived from
	// config or a hostname hash, so it is not guaranteed stable across a
	// redeploy. Accounts orphaned by their own hub changing hostname is a worse
	// failure than a filter that is one term looser.

	sas, err := s.store.ListGCPServiceAccounts(ctx, filter)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if sas == nil {
		sas = []store.GCPServiceAccount{}
	}

	// No read authorization call, matching the nested list exactly. That route
	// has never had one, and adding a check on only this surface would mean the
	// same data is readable or not depending on which URL asked for it -- so a
	// caller denied here would simply use the other route. The gap is real and
	// belongs to the route-authz manifest (#598); what this phase must not do
	// is create a second, differently-behaved door to the same rows.
	identity := GetIdentityFromContext(ctx)

	items := make([]GCPServiceAccountWithCapabilities, len(sas))
	if identity != nil {
		resources := make([]Resource, len(sas))
		for i := range sas {
			resources[i] = gcpServiceAccountResource(&sas[i])
		}
		caps := s.authzService.ComputeCapabilitiesBatch(ctx, identity, resources, "gcp_service_account")
		for i := range sas {
			items[i] = GCPServiceAccountWithCapabilities{GCPServiceAccount: sas[i], Cap: caps[i]}
		}
	} else {
		for i := range sas {
			items[i] = GCPServiceAccountWithCapabilities{GCPServiceAccount: sas[i]}
		}
	}

	var scopeCap *Capabilities
	if identity != nil {
		scopeCap = s.authzService.ComputeScopeCapabilities(ctx, identity, req.scope, req.scopeID, "gcp_service_account")
	}

	// MintQuota is omitted here. It is a per-project quota surfaced for the
	// project settings view; this route is scope-general and has no single
	// project to report against when scope=hub.
	writeJSON(w, http.StatusOK, ListGCPServiceAccountsResponse{
		Items:        items,
		Capabilities: scopeCap,
	})
}

// createGCPServiceAccountScoped registers a service account at an explicit
// scope.
func (s *Server) createGCPServiceAccountScoped(w http.ResponseWriter, r *http.Request) {
	req, ok := s.parseGCPScopeRequest(w, r)
	if !ok {
		return
	}

	if req.includeHubScoped {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			"includeHubScoped is not valid on create", nil)
		return
	}

	switch req.scope {
	case store.ScopeProject:
		// Same handler as the nested route, so the two addresses for the same
		// operation cannot drift apart in validation, authorization, or the
		// auto-verify step that follows creation.
		s.createGCPServiceAccount(w, r, req.scopeID)

	case store.ScopeHub:
		// P4 item A, held. Hub-scoped creation is gated on the resolution of
		// whether it requires gcpIamCheckMode to be "enforce"; until that is
		// settled the write path stays shut rather than shipping under a
		// permission model that may change.
		//
		// Refused explicitly instead of being left unrouted: a 404 here would
		// read as "wrong URL" and send P5 looking for a route that does exist.
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			"hub-scoped service account creation is not enabled on this hub", nil)

	default:
		// parseGCPScopeRequest admits no other value; this is here so that
		// widening it later cannot silently fall through to a success path.
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			"unsupported scope for service account creation", nil)
	}
}
