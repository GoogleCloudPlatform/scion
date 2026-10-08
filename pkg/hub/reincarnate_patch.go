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
	"fmt"
	"net/http"
	"strconv"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// reincarnatePatch is the validated, authorized set of settings a
// `scion reincarnate` request changes on the next generation
// (ptone/scion#3302). A zero field means "unchanged".
//
// Image, Model, ThinkingLevel and HarnessAuth are recorded into
// CreateInputs as explicit edits (recordReincarnatePatchEdits), the same
// fields a PATCH records, so later reincarnations replay them. Role and
// GCPIdentity are kept fields: buildFreshAppliedConfig copies them verbatim
// from the current AppliedConfig, so writing them onto the fresh config is
// what carries them forward.
type reincarnatePatch struct {
	Image         string
	Model         string
	HarnessAuth   string
	ThinkingLevel *int
	Role          AgentRole
	// GCPIdentity is the resolved service-account binding for
	// --service-account; nil when the request did not set one.
	GCPIdentity *store.GCPIdentityConfig
}

// reincarnatePatchFields lists the request fields that patch the next
// generation, in display order.
var reincarnatePatchFields = []string{"serviceAccount", "role", "image", "model", "thinkingLevel", "harnessAuth"}

// hasPatch reports whether the request sets any patch field.
func (r ReincarnateAgentRequest) hasPatch() bool {
	return r.ServiceAccount != "" || r.Role != "" || r.Image != "" || r.Model != "" ||
		r.ThinkingLevel != nil || r.HarnessAuth != ""
}

// patchedFields returns the request's patch fields that are set, in
// reincarnatePatchFields order.
func (r ReincarnateAgentRequest) patchedFields() []string {
	set := map[string]bool{
		"serviceAccount": r.ServiceAccount != "",
		"role":           r.Role != "",
		"image":          r.Image != "",
		"model":          r.Model != "",
		"thinkingLevel":  r.ThinkingLevel != nil,
		"harnessAuth":    r.HarnessAuth != "",
	}
	var out []string
	for _, f := range reincarnatePatchFields {
		if set[f] {
			out = append(out, f)
		}
	}
	return out
}

// validateReincarnatePatchRequest checks the patch fields' values, with no
// store access: the role is one create accepts, the thinking level is in
// create's 0-100 range, and the harness auth is one of create's values. On
// failure it writes a 400 and returns false.
func validateReincarnatePatchRequest(w http.ResponseWriter, req ReincarnateAgentRequest) bool {
	if req.Role != "" && !ValidAgentRole(AgentRole(req.Role)) {
		ValidationError(w, fmt.Sprintf("invalid role %q: must be one of none, readonly, baseline, full", req.Role), nil)
		return false
	}
	if req.ThinkingLevel != nil {
		if tl := *req.ThinkingLevel; tl < 0 || tl > 100 {
			ValidationError(w, "thinkingLevel must be between 0 and 100", nil)
			return false
		}
	}
	if req.HarnessAuth != "" {
		switch req.HarnessAuth {
		case "api-key", "oauth-token", "auth-file", "vertex-ai":
		default:
			ValidationError(w, fmt.Sprintf("invalid harnessAuth %q: must be one of api-key, oauth-token, auth-file, vertex-ai", req.HarnessAuth), nil)
			return false
		}
	}
	return true
}

// checkReincarnateRoleLattice applies create's explicit-role rules to a
// reincarnate --role request: the role may not exceed the project maximum,
// and an agent requester may not grant a role above its own stored role
// (for a self request that means a role can be lowered but never raised).
// CanDelegate and the effect ceiling for the role run in
// reincarnateAuthorityFor (except for a self request that keeps the stored
// role, which is not a role change). Unlike create, which silently caps an agent
// caller's request at the project maximum, a reincarnate over-request is
// always refused: the requester named the role explicitly. On a refusal it
// writes a 403 and returns false.
func (s *Server) checkReincarnateRoleLattice(w http.ResponseWriter, r *http.Request, project *store.Project, requested AgentRole) bool {
	ctx := r.Context()
	projectMax := projectMaxAgentRole(project)
	if CompareRoles(requested, projectMax) > 0 {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			fmt.Sprintf("Cannot grant agent role %q: project maximum is %q", requested, projectMax), nil)
		return false
	}
	if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
		callerRole, _ := s.callerAgentRoleCeiling(ctx, agentIdent.ID())
		if CompareRoles(requested, callerRole) > 0 {
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				fmt.Sprintf("Cannot grant agent role %q: requesting agent role is %q", requested, callerRole), nil)
			return false
		}
	}
	return true
}

// resolveReincarnateServiceAccount resolves and authorizes the service
// account a reincarnate --service-account request assigns. The checks are
// the create path's assign branch (createAgentInProject), kept greppably
// identical to it and to the PATCH path (applyAgentUpdate) on purpose: see
// the comment at the PATCH copy. On a refusal it writes the response and
// returns false; nothing has been written to the agent.
func (s *Server) resolveReincarnateServiceAccount(w http.ResponseWriter, r *http.Request, agent *store.Agent, serviceAccountID string) (*store.GCPIdentityConfig, bool) {
	ctx := r.Context()
	sa, err := s.store.GetGCPServiceAccount(ctx, serviceAccountID)
	if err != nil {
		// errors.Is, not ==: see the create path.
		if errors.Is(err, store.ErrNotFound) {
			ValidationError(w, msgSANotAvailableInProject, nil)
			return nil, false
		}
		writeErrorFromErr(w, err, "")
		return nil, false
	}
	if !sa.ReachableFromProject(agent.ProjectID) {
		ValidationError(w, msgSANotAvailableInProject, nil)
		return nil, false
	}
	if !gcpServiceAccountVerified(sa) {
		ValidationError(w, "GCP service account is not verified; verify it before assigning to agents", nil)
		return nil, false
	}
	if !s.authorizeSAAssignment(w, r, sa, SurfaceAgentReincarnate) {
		return nil, false
	}
	return &store.GCPIdentityConfig{
		MetadataMode:        store.GCPMetadataModeAssign,
		ServiceAccountID:    sa.ID,
		ServiceAccountEmail: sa.Email,
		ProjectID:           sa.ProjectID,
	}, true
}

// resolveReincarnatePatch runs the access checks for the request's patch
// fields that need the store, and returns the patch to apply. It must run
// before any side effect (claim, stop, quota): a refusal leaves the agent
// untouched. The role's CanDelegate and ceiling checks are not here; they
// run in reincarnateAuthorityFor, which the caller invokes with the target
// role. Returns (nil, true) when the request patches nothing.
func (s *Server) resolveReincarnatePatch(w http.ResponseWriter, r *http.Request, agent *store.Agent, project *store.Project, req ReincarnateAgentRequest) (*reincarnatePatch, bool) {
	if !req.hasPatch() {
		return nil, true
	}
	p := &reincarnatePatch{
		Image:       req.Image,
		Model:       req.Model,
		HarnessAuth: req.HarnessAuth,
		Role:        AgentRole(req.Role),
	}
	if req.ThinkingLevel != nil {
		tl := *req.ThinkingLevel
		p.ThinkingLevel = &tl
	}
	if p.Role != "" && !s.checkReincarnateRoleLattice(w, r, project, p.Role) {
		return nil, false
	}
	if req.ServiceAccount != "" {
		gcp, ok := s.resolveReincarnateServiceAccount(w, r, agent, req.ServiceAccount)
		if !ok {
			return nil, false
		}
		p.GCPIdentity = gcp
	}
	if p.HarnessAuth != "" {
		// The same harness-capability validation PATCH applies to
		// auth_selectedType.
		resolvedHarness, caps := s.resolveAgentHarnessCapabilities(r.Context(), agent)
		if issues := validateConfigAgainstHarnessCapabilities(&api.ScionConfig{AuthSelectedType: p.HarnessAuth}, caps); len(issues) > 0 {
			ValidationError(w, "harnessAuth is not supported by harness "+resolvedHarness, map[string]interface{}{
				"harness": resolvedHarness,
				"fields":  issues,
			})
			return nil, false
		}
	}
	return p, true
}

// cloneCreateInputs deep-copies ci so a patch never writes through to the
// outgoing generation's record.
func cloneCreateInputs(ci *store.AgentCreateInputs) *store.AgentCreateInputs {
	if ci == nil {
		return &store.AgentCreateInputs{}
	}
	out := *ci
	if ci.InlineConfig != nil {
		out.InlineConfig = deepCopyScionConfig(ci.InlineConfig)
	}
	if ci.ThinkingLevel != nil {
		tl := *ci.ThinkingLevel
		out.ThinkingLevel = &tl
	}
	return &out
}

// reincarnateNoAuthKeptWarning is the plan warning for a role patch on an
// agent whose create-time no-credentials request is kept.
const reincarnateNoAuthKeptWarning = "the agent keeps running with no injected credentials: it was created with --no-auth or role none, and reincarnate keeps that setting; the new role does not restore credentials"

// addPatchToPlan adds the old and new value of each patched field to plan.
// Image and Model are always on the plan; the other patch fields appear
// only when patched, so a request without patch flags gets the same plan as
// before.
func addPatchToPlan(plan *ReincarnationPlan, old, fresh *store.AgentAppliedConfig, req ReincarnateAgentRequest) {
	plan.Patched = req.patchedFields()
	if req.Role != "" {
		plan.Role = &FieldChange{Old: old.AgentRole, New: fresh.AgentRole}
		// A no-credentials request recorded at create (--no-auth, or
		// role=none mapped to NoAuth) is kept: it cannot be told apart
		// from an explicit --no-auth, so a role raised from none still
		// runs with no injected credentials. Say so on the plan.
		if fresh.AgentRole != string(AgentRoleNone) && fresh.NoAuth &&
			fresh.CreateInputs != nil && fresh.CreateInputs.NoAuth {
			plan.Warnings = append(plan.Warnings, reincarnateNoAuthKeptWarning)
		}
	}
	if req.ServiceAccount != "" {
		plan.ServiceAccount = &FieldChange{Old: gcpIdentityLabel(old.GCPIdentity), New: gcpIdentityLabel(fresh.GCPIdentity)}
	}
	if req.ThinkingLevel != nil {
		plan.ThinkingLevel = &FieldChange{Old: thinkingLevelLabel(old.ThinkingLevel), New: thinkingLevelLabel(fresh.ThinkingLevel)}
	}
	if req.HarnessAuth != "" {
		plan.HarnessAuth = &FieldChange{Old: old.HarnessAuth, New: fresh.HarnessAuth}
	}
}

// gcpIdentityLabel renders a GCP identity binding for the plan: the
// service account's email (or ID) for assign mode, else the mode name.
func gcpIdentityLabel(g *store.GCPIdentityConfig) string {
	if g == nil {
		return ""
	}
	if g.MetadataMode == store.GCPMetadataModeAssign {
		if g.ServiceAccountEmail != "" {
			return g.ServiceAccountEmail
		}
		return g.ServiceAccountID
	}
	return g.MetadataMode
}

func thinkingLevelLabel(tl *int) string {
	if tl == nil {
		return ""
	}
	return strconv.Itoa(*tl)
}
