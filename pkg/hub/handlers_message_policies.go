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
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messagepolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Project message policy routes (ptone/scion#8047, ptone/scion#8048):
//
//	GET    /api/v1/projects/{id}/message-policies                      list policies
//	POST   /api/v1/projects/{id}/message-policies                      create policy
//	POST   /api/v1/projects/{id}/message-policies/test                 test inline policy + sample (runs System One live unless synthetic judgments are supplied)
//	GET    /api/v1/projects/{id}/message-policies/decisions            list content-free decisions
//	GET    /api/v1/projects/{id}/message-policies/{policyId}           get policy (by ID or name)
//	PATCH  /api/v1/projects/{id}/message-policies/{policyId}           edit policy (bumps revision + invalidates cache)
//	DELETE /api/v1/projects/{id}/message-policies/{policyId}           delete policy (invalidates cache)
//	POST   /api/v1/projects/{id}/message-policies/{policyId}/test      test stored policy + sample (runs System One live unless synthetic judgments are supplied)
//	GET    /api/v1/projects/{id}/message-policies/{policyId}/decisions list content-free decisions for one policy
//
// Every route answers 404 while the hub.message_policies experiment is off.

const (
	// maxMessagePolicyWriteBodyBytes bounds create/patch JSON request bodies.
	maxMessagePolicyWriteBodyBytes = 64 * 1024

	// maxMessagePolicyTestBodyBytes bounds policy test JSON request bodies so
	// a full 16 KiB (or larger, to exercise UTF-8 body truncation) sample plus
	// inline policy spec fits comfortably.
	maxMessagePolicyTestBodyBytes = 256 * 1024

	defaultMessagePolicyListLimit     = 50
	maxMessagePolicyListLimit         = 200
	defaultMessagePolicyDecisionLimit = 50
	maxMessagePolicyDecisionLimit     = 200

	mutationTypeMessagePolicyCreate = "project_message_policy_create"
	mutationTypeMessagePolicyUpdate = "project_message_policy_update"
	mutationTypeMessagePolicyDelete = "project_message_policy_delete"
)

// MessagePolicyResponse represents a persisted message policy along with the
// build's surface enforcement coverage declaration (consumer design §5.3, §11).
type MessagePolicyResponse struct {
	store.MessagePolicy
	Coverage messagepolicy.CoverageInfo `json:"coverage"`
}

// ListMessagePoliciesResponse is the response for GET .../message-policies.
type ListMessagePoliciesResponse struct {
	Policies   []MessagePolicyResponse    `json:"policies"`
	Coverage   messagepolicy.CoverageInfo `json:"coverage"`
	NextCursor string                     `json:"next_cursor,omitempty"`
	TotalCount int                        `json:"total_count"`
}

// ListMessagePolicyDecisionsResponse is the content-free response for
// GET .../message-policies[/...]/decisions. It never carries message body text.
type ListMessagePolicyDecisionsResponse struct {
	Decisions  []store.MessagePolicyDecision `json:"decisions"`
	NextCursor string                        `json:"next_cursor,omitempty"`
	TotalCount int                           `json:"total_count"`
}

// CreateMessagePolicyRequest is the request body for POST .../message-policies.
type CreateMessagePolicyRequest struct {
	Name              string                   `json:"name"`
	Description       string                   `json:"description,omitempty"`
	Enabled           *bool                    `json:"enabled,omitempty"`
	Mode              string                   `json:"mode,omitempty"`
	Scope             store.MessagePolicyScope `json:"scope"`
	ViolationRule     string                   `json:"violation_rule"`
	SystemOneRule     string                   `json:"system_one_rule,omitempty"`
	OnUncertain       string                   `json:"on_uncertain,omitempty"`
	OnFailed          string                   `json:"on_failed"`
	OnFailedReasons   map[string]string        `json:"on_failed_reasons,omitempty"`
	RejectReason      string                   `json:"reject_reason,omitempty"`
	RejectRemediation string                   `json:"reject_remediation,omitempty"`
}

// PatchMessagePolicyRequest is the request body for PATCH .../message-policies/{policyId}.
// Omitted pointer fields preserve the stored policy's value.
type PatchMessagePolicyRequest struct {
	Name              *string                   `json:"name,omitempty"`
	Description       *string                   `json:"description,omitempty"`
	Enabled           *bool                     `json:"enabled,omitempty"`
	Mode              *string                   `json:"mode,omitempty"`
	Scope             *store.MessagePolicyScope `json:"scope,omitempty"`
	ViolationRule     *string                   `json:"violation_rule,omitempty"`
	SystemOneRule     *string                   `json:"system_one_rule,omitempty"`
	OnUncertain       *string                   `json:"on_uncertain,omitempty"`
	OnFailed          *string                   `json:"on_failed,omitempty"`
	OnFailedReasons   *map[string]string        `json:"on_failed_reasons,omitempty"`
	RejectReason      *string                   `json:"reject_reason,omitempty"`
	RejectRemediation *string                   `json:"reject_remediation,omitempty"`
	ExpectedRevision  int64                     `json:"expected_revision,omitempty"`
}

// MessagePolicyInlineSpec carries an inline policy definition for the policy
// test endpoint.
type MessagePolicyInlineSpec struct {
	Name              string                    `json:"name,omitempty"`
	Description       string                    `json:"description,omitempty"`
	Mode              string                    `json:"mode,omitempty"`
	Scope             *store.MessagePolicyScope `json:"scope,omitempty"`
	ViolationRule     string                    `json:"violation_rule,omitempty"`
	SystemOneRule     string                    `json:"system_one_rule,omitempty"`
	OnUncertain       string                    `json:"on_uncertain,omitempty"`
	OnFailed          string                    `json:"on_failed,omitempty"`
	OnFailedReasons   map[string]string         `json:"on_failed_reasons,omitempty"`
	RejectReason      string                    `json:"reject_reason,omitempty"`
	RejectRemediation string                    `json:"reject_remediation,omitempty"`
}

// MessagePolicyTestSample carries synthetic send facts for the policy test
// endpoint. The sample body is never persisted or echoed in the response. When
// the tested policy has a system_one_rule and synthetic judgments are not
// supplied, selected sample fields are sent to the configured provider, which
// may incur provider cost, with no automatic replay or retry.
type MessagePolicyTestSample struct {
	Surface           string                           `json:"surface,omitempty"`
	Body              string                           `json:"body"`
	Type              string                           `json:"type,omitempty"`
	HasChoices        bool                             `json:"has_choices,omitempty"`
	Urgent            bool                             `json:"urgent,omitempty"`
	AttachmentCount   int                              `json:"attachment_count,omitempty"`
	MentionedAgentIDs []string                         `json:"mentioned_agent_ids,omitempty"`
	MentionedUserIDs  []string                         `json:"mentioned_user_ids,omitempty"`
	Sender            *messagepolicy.SenderFacts       `json:"sender,omitempty"`
	Recipient         *messagepolicy.RecipientFacts    `json:"recipient,omitempty"`
	Conversation      *messagepolicy.ConversationFacts `json:"conversation,omitempty"`
	Judgments         *messagepolicy.Judgments         `json:"judgments,omitempty"`
	JudgmentFlags     map[string]bool                  `json:"judgment_flags,omitempty"`
	JudgmentLabels    map[string]string                `json:"judgment_labels,omitempty"`
}

// TestMessagePolicyRequest is the request body for POST .../message-policies/test
// and POST .../message-policies/{policyId}/test. Callers may nest the policy
// under "policy" or pass policy fields at the top level alongside "sample".
type TestMessagePolicyRequest struct {
	Policy            *MessagePolicyInlineSpec  `json:"policy,omitempty"`
	Name              string                    `json:"name,omitempty"`
	Mode              string                    `json:"mode,omitempty"`
	Scope             *store.MessagePolicyScope `json:"scope,omitempty"`
	ViolationRule     string                    `json:"violation_rule,omitempty"`
	SystemOneRule     string                    `json:"system_one_rule,omitempty"`
	OnUncertain       string                    `json:"on_uncertain,omitempty"`
	OnFailed          string                    `json:"on_failed,omitempty"`
	OnFailedReasons   map[string]string         `json:"on_failed_reasons,omitempty"`
	RejectReason      string                    `json:"reject_reason,omitempty"`
	RejectRemediation string                    `json:"reject_remediation,omitempty"`

	Sample  *MessagePolicyTestSample `json:"sample,omitempty"`
	Surface string                   `json:"surface,omitempty"`
	Body    string                   `json:"body,omitempty"`
}

// TestMessagePolicyResponse is the content-free policy test evaluation response.
// It never echoes the sample message body.
type TestMessagePolicyResponse struct {
	PolicyID       string          `json:"policy_id,omitempty"`
	PolicyName     string          `json:"policy_name"`
	PolicyRevision int64           `json:"policy_revision,omitempty"`
	Mode           string          `json:"mode"`
	Surface        string          `json:"surface"`
	ScopeMatched   bool            `json:"scope_matched"`
	Class          string          `json:"class"`
	ReasonCode     string          `json:"reason_code,omitempty"`
	Outcome        string          `json:"outcome"`
	Action         string          `json:"action"`
	Reason         string          `json:"reason,omitempty"`
	Remediation    string          `json:"remediation,omitempty"`
	Evaluation     json.RawMessage `json:"evaluation,omitempty"`
	ElapsedMs      int64           `json:"elapsed_ms"`
	BodyBytes      int             `json:"body_bytes"`
	BodyTruncated  bool            `json:"body_truncated"`
}

func toMessagePolicyResponse(pol store.MessagePolicy) MessagePolicyResponse {
	cp := pol.Clone()
	if cp.Scope.Surfaces == nil {
		cp.Scope.Surfaces = []string{}
	}
	return MessagePolicyResponse{
		MessagePolicy: *cp,
		Coverage:      messagepolicy.Coverage(),
	}
}

func isReservedMessagePolicyName(name string) bool {
	return name == "test" || name == "decisions"
}

// handleProjectMessagePolicies dispatches all sub-routes under
// /api/v1/projects/{id}/message-policies.
func (s *Server) handleProjectMessagePolicies(w http.ResponseWriter, r *http.Request, projectID, rest string) {
	if !s.experimentEnabled(experiments.MessagePolicies) {
		NotFound(w, "route")
		return
	}

	sub := strings.Trim(rest, "/")

	// Validate method before project/authz lookup so unsupported methods get 405.
	switch sub {
	case "":
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			MethodNotAllowed(w, http.MethodGet, http.MethodPost)
			return
		}
	case "test":
		if r.Method != http.MethodPost {
			MethodNotAllowed(w, http.MethodPost)
			return
		}
	case "decisions":
		if r.Method != http.MethodGet {
			MethodNotAllowed(w, http.MethodGet)
			return
		}
	default:
		_, action, hasAction := strings.Cut(sub, "/")
		if !hasAction {
			switch r.Method {
			case http.MethodGet, http.MethodPatch, http.MethodDelete:
			default:
				MethodNotAllowed(w, http.MethodGet, http.MethodPatch, http.MethodDelete)
				return
			}
		} else {
			if strings.Contains(action, "/") {
				NotFound(w, "Message policy route")
				return
			}
			switch action {
			case "test":
				if r.Method != http.MethodPost {
					MethodNotAllowed(w, http.MethodPost)
					return
				}
			case "decisions":
				if r.Method != http.MethodGet {
					MethodNotAllowed(w, http.MethodGet)
					return
				}
			default:
				NotFound(w, "Message policy route")
				return
			}
		}
	}

	ctx := r.Context()
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		writeErrorFromErr(w, err, "Project")
		return
	}

	switch {
	case sub == "" && r.Method == http.MethodGet:
		if !s.authorizeMessagePolicyRead(w, r, project) {
			return
		}
		s.handleListMessagePolicies(w, r, project)
	case sub == "" && r.Method == http.MethodPost:
		actorID, ok := s.authorizeMessagePolicyManage(w, r, project)
		if !ok {
			return
		}
		s.handleCreateMessagePolicy(w, r, project, actorID)
	case sub == "test":
		if _, ok := s.authorizeMessagePolicyManage(w, r, project); !ok {
			return
		}
		s.handleTestMessagePolicy(w, r, project, "")
	case sub == "decisions":
		if _, ok := s.authorizeMessagePolicyManage(w, r, project); !ok {
			return
		}
		s.handleListMessagePolicyDecisions(w, r, project, "")
	default:
		policyRef, action, hasAction := strings.Cut(sub, "/")
		if !hasAction {
			switch r.Method {
			case http.MethodGet:
				if !s.authorizeMessagePolicyRead(w, r, project) {
					return
				}
				s.handleGetMessagePolicy(w, r, project, policyRef)
			case http.MethodPatch:
				actorID, ok := s.authorizeMessagePolicyManage(w, r, project)
				if !ok {
					return
				}
				s.handlePatchMessagePolicy(w, r, project, policyRef, actorID)
			case http.MethodDelete:
				if _, ok := s.authorizeMessagePolicyManage(w, r, project); !ok {
					return
				}
				s.handleDeleteMessagePolicy(w, r, project, policyRef)
			}
			return
		}
		if _, ok := s.authorizeMessagePolicyManage(w, r, project); !ok {
			return
		}
		switch action {
		case "test":
			s.handleTestMessagePolicy(w, r, project, policyRef)
		case "decisions":
			s.handleListMessagePolicyDecisions(w, r, project, policyRef)
		}
	}
}

// messagePolicyManageRuleMessage is the denial text when a caller lacks active
// project owner, project admin, or unscoped local Hub admin authority to manage
// a project's message policies.
const messagePolicyManageRuleMessage = "Only active project owners, project administrators, or local Hub administrators can manage message policies"

// messagePolicyManageCredentialAllowed reports whether the request's identity
// and credential kind may attempt message policy management (create, patch,
// delete, test, decisions):
//   - a local user or dev identity with an interactive session or dev credential;
//   - a scoped user identity with a user access token (UAT).
//
// Agents, agent-delegated credentials, broker credentials (including broker
// on-behalf-of), hub-delivery credentials, federated credentials, and visitors
// are refused with 403.
func messagePolicyManageCredentialAllowed(ctx context.Context, identity Identity) bool {
	if isNilIdentity(identity) {
		return false
	}
	if _, federated := identity.(FederatedIdentity); federated {
		return false
	}
	kind := requestCredentialKind(ctx, identity)
	if IsScopedUserIdentity(identity) {
		if identity.Type() != "user" {
			return false
		}
		_, ok := identity.(UserIdentity)
		return ok && kind == CredentialKindUAT
	}
	switch identity.Type() {
	case "user", "dev":
		_, ok := identity.(UserIdentity)
		return ok && allowedMutationCredentials[kind]
	default:
		return false
	}
}

// messagePolicyReadCredentialAllowed reports whether the request's identity and
// credential kind may attempt message policy reads (list, get): every
// credential admitted by messagePolicyManageCredentialAllowed, plus an agent of
// projectID presenting its own agent JWT (never an agent-delegated credential
// or a cross-project agent).
func messagePolicyReadCredentialAllowed(ctx context.Context, identity Identity, projectID string) bool {
	if messagePolicyManageCredentialAllowed(ctx, identity) {
		return true
	}
	if isNilIdentity(identity) || IsScopedUserIdentity(identity) {
		return false
	}
	if _, federated := identity.(FederatedIdentity); federated {
		return false
	}
	if identity.Type() != "agent" {
		return false
	}
	agentIdent, ok := identity.(AgentIdentity)
	if !ok || agentIdent.ProjectID() != projectID {
		return false
	}
	return requestCredentialKind(ctx, identity) == CredentialKindAgentJWT
}

// messagePolicyOwnerOrAdminRule reports whether identity's underlying user is
// an active project owner or project admin on projectID, or an unscoped local
// Hub administrator (super-admin or hub-admin). It applies across interactive
// sessions, dev credentials, and user access tokens.
func (s *Server) messagePolicyOwnerOrAdminRule(ctx context.Context, identity Identity, projectID string) bool {
	if isNilIdentity(identity) || s.authzService == nil {
		return false
	}
	if _, federated := identity.(FederatedIdentity); federated {
		return false
	}
	switch identity.Type() {
	case "user", "dev":
	default:
		return false
	}
	userIdent, ok := identity.(UserIdentity)
	if !ok {
		return false
	}
	if IsUnscopedLocalPlatformAdmin(userIdent) {
		return true
	}
	if !IsScopedUserIdentity(userIdent) && s.authzService.IsHubAdmin(ctx, userIdent.ID()) {
		return true
	}
	return s.authzService.isProjectOwnerOrAdmin(ctx, userIdent.ID(), projectID)
}

// authorizeMessagePolicyRead enforces read admission on GET list/get per the
// M1 §4 credential matrix: project.read_message_policies on the project for
// user/dev callers (or project.manage_message_policies for a scoped UAT held by
// an active owner/admin or local Hub admin), and project.read (via the agent's
// own hub-signed JWT carrying project:read plus its delegation ceiling) for an
// agent of the project.
func (s *Server) authorizeMessagePolicyRead(w http.ResponseWriter, r *http.Request, project *store.Project) bool {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if isNilIdentity(identity) {
		Unauthorized(w)
		return false
	}
	res := projectResource(project)
	if !messagePolicyReadCredentialAllowed(ctx, identity, project.ID) {
		logAuthzDenial(r, identity, res, ActionReadMessagePolicies, "credential or principal kind cannot read message policies")
		writeForbiddenStructured(w, "", res.Type, ActionReadMessagePolicies)
		return false
	}
	if identity.Type() == "agent" {
		decision := s.authzService.CheckAccess(ctx, identity, res, ActionRead)
		if decision.Allowed {
			return true
		}
		logAuthzDenial(r, identity, res, ActionReadMessagePolicies, decision.Reason)
		writeForbiddenStructuredDenialCause(w, "", res.Type, ActionReadMessagePolicies, decision.DeniedBy, decision.adoptionDetailsCause())
		return false
	}
	decision := s.authzService.CheckAccess(ctx, identity, res, ActionReadMessagePolicies)
	if decision.Allowed {
		return true
	}
	// §4 row 3: an owner/admin UAT carrying project:manage_message_policies on
	// a covering boundary also satisfies read without requiring a separate
	// project:read_message_policies selector.
	if IsScopedUserIdentity(identity) {
		manageDec := s.authzService.CheckAccess(ctx, identity, res, ActionManageMessagePolicies)
		if manageDec.Allowed && s.messagePolicyOwnerOrAdminRule(ctx, identity, project.ID) {
			return true
		}
	}
	logAuthzDenial(r, identity, res, ActionReadMessagePolicies, decision.Reason)
	writeForbiddenStructuredDenialCause(w, "", res.Type, ActionReadMessagePolicies, decision.DeniedBy, decision.adoptionDetailsCause())
	return false
}

// authorizeMessagePolicyManage enforces management admission on create, patch,
// delete, test, and decisions per the M1 §4 credential matrix:
// project.manage_message_policies on the project followed by
// messagePolicyOwnerOrAdminRule on the underlying user across every credential.
func (s *Server) authorizeMessagePolicyManage(w http.ResponseWriter, r *http.Request, project *store.Project) (string, bool) {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if isNilIdentity(identity) {
		Unauthorized(w)
		return "", false
	}
	res := projectResource(project)
	if !messagePolicyManageCredentialAllowed(ctx, identity) {
		logAuthzDenial(r, identity, res, ActionManageMessagePolicies, "credential or principal kind cannot manage message policies")
		writeForbiddenStructured(w, "", res.Type, ActionManageMessagePolicies)
		return "", false
	}
	if !s.authorizeMsg(w, r, res, ActionManageMessagePolicies, messagePolicyManageRuleMessage) {
		return "", false
	}
	if !s.messagePolicyOwnerOrAdminRule(ctx, identity, project.ID) {
		logAuthzDenial(r, identity, res, ActionManageMessagePolicies, "caller is not an active project owner, project admin, or local hub admin")
		writeForbiddenStructured(w, messagePolicyManageRuleMessage, res.Type, ActionManageMessagePolicies)
		return "", false
	}
	return identity.ID(), true
}

// systemOneRuleMayReadProjectSecret reports whether evaluating rawSystemOneRule
// could resolve a System One instance whose auth reads a project-scoped named
// secret ("project" or "project_or_hub"). For explicitly bound rules it checks
// the referenced instance(s); for unbound rules it checks all enabled instances.
func (s *Server) systemOneRuleMayReadProjectSecret(ctx context.Context, rawSystemOneRule string) (bool, error) {
	refs := messagepolicy.ExtractSystemOneRefs(rawSystemOneRule)
	if len(refs) > 0 {
		for _, name := range refs {
			inst, err := s.store.GetSystemOneInstanceByName(ctx, name)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					continue
				}
				return false, err
			}
			if probeRequiresProjectSecretRead(inst) {
				return true, nil
			}
		}
		return false, nil
	}
	enabledTrue := true
	res, err := s.store.ListSystemOneInstances(ctx, store.SystemOneInstanceFilter{
		Enabled: &enabledTrue,
	}, store.ListOptions{
		Limit:          systemOneInstancesListPageSize,
		SkipTotalCount: true,
	})
	if err != nil {
		return false, err
	}
	for i := range res.Items {
		if probeRequiresProjectSecretRead(&res.Items[i]) {
			return true, nil
		}
	}
	return false, nil
}

// authorizeMessagePolicyTestSystemOne enforces the G4/L1 credential-use
// boundary on POST .../message-policies[/{id}]/test before any live System One
// evaluation (when synthetic judgments are not supplied). When the rule can
// resolve an instance with a project or project_or_hub named secret ref, the
// caller's credential must hold both project.read and project.secret_read on
// project before any secret lookup, token mint, or provider call runs.
func (s *Server) authorizeMessagePolicyTestSystemOne(
	w http.ResponseWriter,
	r *http.Request,
	project *store.Project,
	rawSystemOneRule string,
) bool {
	ctx := r.Context()
	needsProjectSecret, err := s.systemOneRuleMayReadProjectSecret(ctx, rawSystemOneRule)
	if err != nil {
		writeErrorFromErr(w, err, "System One instance")
		return false
	}
	if !needsProjectSecret {
		return true
	}
	identity := GetIdentityFromContext(ctx)
	if isNilIdentity(identity) || s.authzService == nil {
		Forbidden(w)
		return false
	}
	res := projectResource(project)
	readDec := s.authzService.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(identity),
		Credential: credentialContextForIdentity(identity),
		Resource:   res,
		Action:     ActionRead,
		Permission: "project.read",
	})
	if !readDec.Allowed {
		logAuthzDenial(r, identity, res, ActionRead, readDec.Reason)
		writeForbiddenStructuredDenialCause(w, "", res.Type, ActionRead, readDec.DeniedBy, readDec.adoptionDetailsCause())
		return false
	}
	secDec := s.authzService.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(identity),
		Credential: credentialContextForIdentity(identity),
		Resource:   res,
		Action:     actionProjectSecretRead,
		Permission: permissionProjectSecretRead,
	})
	if !secDec.Allowed {
		logAuthzDenial(r, identity, res, actionProjectSecretRead, secDec.Reason)
		writeForbiddenStructuredDenialCause(w, "", res.Type, actionProjectSecretRead, secDec.DeniedBy, secDec.adoptionDetailsCause())
		return false
	}
	return true
}

func (s *Server) resolveProjectMessagePolicy(ctx context.Context, projectID, ref string) (*store.MessagePolicy, error) {
	if ref == "" {
		return nil, store.ErrNotFound
	}
	pol, err := s.store.GetMessagePolicy(ctx, ref)
	if err == nil {
		if pol.ProjectID != projectID {
			return nil, store.ErrNotFound
		}
		return pol, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	return s.store.GetMessagePolicyByName(ctx, projectID, ref)
}

func authoringScopeFromStore(sc store.MessagePolicyScope) messagepolicy.Scope {
	return messagepolicy.Scope{
		Surfaces: sc.Surfaces,
		Sender: messagepolicy.SenderSelector{
			AgentIDs:  sc.Sender.AgentIDs,
			AllAgents: sc.Sender.AllAgents,
		},
		Recipient: messagepolicy.RecipientSelector{
			Kinds:    sc.Recipient.Kinds,
			AgentIDs: sc.Recipient.AgentIDs,
		},
	}
}

func writeMessagePolicyValidationError(w http.ResponseWriter, err error) {
	var valErr *messagepolicy.ValidationError
	if errors.As(err, &valErr) {
		var details map[string]interface{}
		if valErr.Field != "" || valErr.Path != "" || valErr.Line > 0 || valErr.Column > 0 || valErr.Category != "" || valErr.Code == messagepolicy.ErrCodeSurfaceNotCovered {
			details = make(map[string]interface{})
			if valErr.Field != "" {
				details["field"] = valErr.Field
			}
			if valErr.Path != "" {
				details["path"] = valErr.Path
			}
			if valErr.Line > 0 {
				details["line"] = valErr.Line
			}
			if valErr.Column > 0 {
				details["column"] = valErr.Column
			}
			if valErr.Category != "" {
				details["category"] = valErr.Category
				if valErr.Code == messagepolicy.ErrCodeUnsupportedDialect {
					details["reason"] = valErr.Category
				}
			}
			if valErr.Code == messagepolicy.ErrCodeSurfaceNotCovered {
				cov := messagepolicy.Coverage()
				details["enforced_surfaces"] = cov.EnforcedSurfaces
				details["uncovered_agent_paths"] = cov.UncoveredAgentPaths
			}
		}
		code := valErr.Code
		if code == "" {
			code = ErrCodeValidationError
		}
		writeError(w, http.StatusBadRequest, code, valErr.Message, details)
		return
	}
	ValidationError(w, err.Error(), nil)
}

// validateMessagePolicyAuthoring validates spec via pkg/messagepolicy and
// verifies that any sender agent_ids belong to projectID (consumer design
// §§5.3, 12.5), returning the normalized mode and canonicalized sender agent
// IDs (ag.ID). On failure it writes a structured 400 response and returns
// ("", nil, false).
func (s *Server) validateMessagePolicyAuthoring(
	ctx context.Context,
	w http.ResponseWriter,
	projectID string,
	spec messagepolicy.AuthoringSpec,
	forbidReservedNames bool,
) (string, []string, bool) {
	ev := s.MessagePolicyEvaluator()
	if ev == nil {
		writeError(w, http.StatusBadRequest, messagepolicy.ErrCodeUnsupportedDialect,
			"message_policy rule engine evaluator is unavailable",
			map[string]interface{}{
				"field":    "violation_rule",
				"category": "consumer_not_registered",
				"reason":   "consumer_not_registered",
			})
		return "", nil, false
	}

	mode, err := ev.ValidateAuthoring(ctx, spec)
	if err != nil {
		writeMessagePolicyValidationError(w, err)
		return "", nil, false
	}

	if _, parseErr := uuid.Parse(spec.Name); parseErr == nil {
		writeError(w, http.StatusBadRequest, ErrCodeValidationError,
			fmt.Sprintf("policy name %q must not be UUID-shaped", spec.Name),
			map[string]interface{}{"field": "name"})
		return "", nil, false
	}

	if forbidReservedNames && isReservedMessagePolicyName(spec.Name) {
		writeError(w, http.StatusBadRequest, ErrCodeValidationError,
			fmt.Sprintf("policy name %q is reserved", spec.Name),
			map[string]interface{}{"field": "name"})
		return "", nil, false
	}

	var canonicalSenderIDs []string
	if len(spec.Scope.Sender.AgentIDs) > 0 {
		canonicalSenderIDs = make([]string, 0, len(spec.Scope.Sender.AgentIDs))
		for _, rawAgentID := range spec.Scope.Sender.AgentIDs {
			agentID := strings.TrimSpace(rawAgentID)
			if agentID == "" {
				writeError(w, http.StatusBadRequest, ErrCodeValidationError,
					"scope.sender.agent_ids entries must not be empty",
					map[string]interface{}{"field": "scope.sender.agent_ids"})
				return "", nil, false
			}
			ag, err := s.store.GetAgent(ctx, agentID)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					writeError(w, http.StatusBadRequest, ErrCodeValidationError,
						fmt.Sprintf("scope.sender.agent_ids: agent %q does not belong to this project", agentID),
						map[string]interface{}{"field": "scope.sender.agent_ids", "agent_id": agentID})
					return "", nil, false
				}
				writeErrorFromErr(w, err, "Agent")
				return "", nil, false
			}
			if ag.ProjectID != projectID {
				writeError(w, http.StatusBadRequest, ErrCodeValidationError,
					fmt.Sprintf("scope.sender.agent_ids: agent %q does not belong to this project", agentID),
					map[string]interface{}{"field": "scope.sender.agent_ids", "agent_id": agentID})
				return "", nil, false
			}
			if !slices.Contains(canonicalSenderIDs, ag.ID) {
				canonicalSenderIDs = append(canonicalSenderIDs, ag.ID)
			}
		}
	}

	return mode, canonicalSenderIDs, true
}

func parseBoundedLimit(raw string, defaultLimit, maxLimit int) (int, bool) {
	if raw == "" {
		return defaultLimit, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, false
	}
	if n > maxLimit {
		n = maxLimit
	}
	return n, true
}

func (s *Server) handleListMessagePolicies(w http.ResponseWriter, r *http.Request, project *store.Project) {
	ctx := r.Context()
	q := r.URL.Query()

	filter := store.MessagePolicyFilter{
		ProjectID: project.ID,
	}
	if mode := strings.TrimSpace(q.Get("mode")); mode != "" {
		if !store.IsValidMessagePolicyMode(mode) {
			ValidationError(w, fmt.Sprintf("mode must be %q or %q", store.MessagePolicyModeShadow, store.MessagePolicyModeEnforce),
				map[string]interface{}{"field": "mode"})
			return
		}
		filter.Mode = mode
	}
	if rawEnabled := strings.TrimSpace(q.Get("enabled")); rawEnabled != "" {
		enabled, err := strconv.ParseBool(rawEnabled)
		if err != nil {
			ValidationError(w, "enabled must be a boolean", map[string]interface{}{"field": "enabled"})
			return
		}
		filter.Enabled = &enabled
	}

	limit, ok := parseBoundedLimit(strings.TrimSpace(q.Get("limit")), defaultMessagePolicyListLimit, maxMessagePolicyListLimit)
	if !ok {
		ValidationError(w, "limit must be a positive integer", map[string]interface{}{"field": "limit"})
		return
	}

	res, err := s.store.ListMessagePolicies(ctx, filter, store.ListOptions{
		Limit:  limit,
		Cursor: strings.TrimSpace(q.Get("cursor")),
	})
	if err != nil {
		if errors.Is(err, store.ErrInvalidCursor) {
			writeInvalidCursor(w)
			return
		}
		writeErrorFromErr(w, err, "Message policies")
		return
	}

	out := make([]MessagePolicyResponse, 0, len(res.Items))
	for i := range res.Items {
		out = append(out, toMessagePolicyResponse(res.Items[i]))
	}
	writeJSON(w, http.StatusOK, ListMessagePoliciesResponse{
		Policies:   out,
		Coverage:   messagepolicy.Coverage(),
		NextCursor: res.NextCursor,
		TotalCount: res.TotalCount,
	})
}

func (s *Server) handleGetMessagePolicy(w http.ResponseWriter, r *http.Request, project *store.Project, policyRef string) {
	pol, err := s.resolveProjectMessagePolicy(r.Context(), project.ID, policyRef)
	if err != nil {
		writeErrorFromErr(w, err, "Message policy")
		return
	}
	writeJSON(w, http.StatusOK, toMessagePolicyResponse(*pol))
}

func (s *Server) handleCreateMessagePolicy(w http.ResponseWriter, r *http.Request, project *store.Project, actorID string) {
	ctx := r.Context()
	r.Body = http.MaxBytesReader(w, r.Body, maxMessagePolicyWriteBodyBytes)
	var req CreateMessagePolicyRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	spec := messagepolicy.AuthoringSpec{
		Name:              strings.TrimSpace(req.Name),
		Mode:              req.Mode,
		Scope:             authoringScopeFromStore(req.Scope),
		ViolationRule:     req.ViolationRule,
		SystemOneRule:     req.SystemOneRule,
		OnUncertain:       strings.TrimSpace(req.OnUncertain),
		OnFailed:          strings.TrimSpace(req.OnFailed),
		OnFailedReasons:   req.OnFailedReasons,
		RejectReason:      req.RejectReason,
		RejectRemediation: req.RejectRemediation,
	}
	mode, canonicalSenderIDs, ok := s.validateMessagePolicyAuthoring(ctx, w, project.ID, spec, true)
	if !ok {
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	scope := req.Scope.Clone()
	scope.Sender.AgentIDs = canonicalSenderIDs

	policy := &store.MessagePolicy{
		ProjectID:         project.ID,
		Name:              spec.Name,
		Description:       req.Description,
		Enabled:           enabled,
		Mode:              mode,
		Scope:             scope,
		ViolationRule:     req.ViolationRule,
		SystemOneRule:     req.SystemOneRule,
		SystemOneRefs:     messagepolicy.ExtractSystemOneRefs(req.SystemOneRule),
		OnUncertain:       spec.OnUncertain,
		OnFailed:          spec.OnFailed,
		OnFailedReasons:   req.OnFailedReasons,
		RejectReason:      req.RejectReason,
		RejectRemediation: req.RejectRemediation,
		CreatedBy:         actorID,
		UpdatedBy:         actorID,
	}

	if err := s.store.CreateMessagePolicy(ctx, policy); err != nil {
		switch {
		case errors.Is(err, store.ErrAlreadyExists):
			Conflict(w, fmt.Sprintf("message policy %q already exists in this project", policy.Name))
		case errors.Is(err, store.ErrQuotaExceeded):
			writeError(w, http.StatusConflict, ErrCodeQuotaExceeded, err.Error(), nil)
		case errors.Is(err, store.ErrInvalidInput):
			ValidationError(w, err.Error(), nil)
		default:
			writeErrorFromErr(w, err, "Message policy")
		}
		return
	}

	afterSummary, _ := json.Marshal(map[string]any{
		"name":     policy.Name,
		"mode":     policy.Mode,
		"enabled":  policy.Enabled,
		"revision": policy.Revision,
	})
	s.emitMutationAudit(ctx, &store.MutationAuditRecord{
		MutationType: mutationTypeMessagePolicyCreate,
		TargetType:   "project",
		TargetID:     project.ID,
		AfterSummary: string(afterSummary),
	})

	writeJSON(w, http.StatusCreated, toMessagePolicyResponse(*policy))
}

func (s *Server) handlePatchMessagePolicy(w http.ResponseWriter, r *http.Request, project *store.Project, policyRef, actorID string) {
	ctx := r.Context()
	existing, err := s.resolveProjectMessagePolicy(ctx, project.ID, policyRef)
	if err != nil {
		writeErrorFromErr(w, err, "Message policy")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxMessagePolicyWriteBodyBytes)
	var req PatchMessagePolicyRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	prevRevision := existing.Revision
	if req.ExpectedRevision > 0 && req.ExpectedRevision != prevRevision {
		writeError(w, http.StatusConflict, ErrCodeRevisionConflict,
			"message policy has been modified by another request",
			map[string]interface{}{
				"expected_revision": req.ExpectedRevision,
				"current_revision":  prevRevision,
			})
		return
	}

	candidate := existing.Clone()
	if req.Name != nil {
		candidate.Name = strings.TrimSpace(*req.Name)
	}
	if req.Description != nil {
		candidate.Description = *req.Description
	}
	if req.Enabled != nil {
		candidate.Enabled = *req.Enabled
	}
	if req.Mode != nil {
		candidate.Mode = *req.Mode
	}
	if req.Scope != nil {
		candidate.Scope = req.Scope.Clone()
	}
	if req.ViolationRule != nil {
		candidate.ViolationRule = *req.ViolationRule
	}
	if req.SystemOneRule != nil {
		candidate.SystemOneRule = *req.SystemOneRule
	}
	if req.OnUncertain != nil {
		candidate.OnUncertain = strings.TrimSpace(*req.OnUncertain)
	}
	if req.OnFailed != nil {
		candidate.OnFailed = strings.TrimSpace(*req.OnFailed)
	}
	if req.OnFailedReasons != nil {
		if len(*req.OnFailedReasons) == 0 {
			candidate.OnFailedReasons = nil
		} else {
			cp := make(map[string]string, len(*req.OnFailedReasons))
			for k, v := range *req.OnFailedReasons {
				cp[k] = v
			}
			candidate.OnFailedReasons = cp
		}
	}
	if req.RejectReason != nil {
		candidate.RejectReason = *req.RejectReason
	}
	if req.RejectRemediation != nil {
		candidate.RejectRemediation = *req.RejectRemediation
	}
	candidate.UpdatedBy = actorID
	// Pin optimistic concurrency against the revision we loaded.
	candidate.Revision = prevRevision

	spec := messagepolicy.AuthoringSpec{
		Name:              candidate.Name,
		Mode:              candidate.Mode,
		Scope:             authoringScopeFromStore(candidate.Scope),
		ViolationRule:     candidate.ViolationRule,
		SystemOneRule:     candidate.SystemOneRule,
		OnUncertain:       candidate.OnUncertain,
		OnFailed:          candidate.OnFailed,
		OnFailedReasons:   candidate.OnFailedReasons,
		RejectReason:      candidate.RejectReason,
		RejectRemediation: candidate.RejectRemediation,
	}
	mode, canonicalSenderIDs, ok := s.validateMessagePolicyAuthoring(ctx, w, project.ID, spec, true)
	if !ok {
		return
	}
	candidate.Mode = mode
	candidate.Scope.Sender.AgentIDs = canonicalSenderIDs
	candidate.SystemOneRefs = messagepolicy.ExtractSystemOneRefs(candidate.SystemOneRule)

	if err := s.store.UpdateMessagePolicy(ctx, candidate); err != nil {
		switch {
		case errors.Is(err, store.ErrRevisionConflict):
			writeError(w, http.StatusConflict, ErrCodeRevisionConflict,
				"message policy has been modified by another request", nil)
		case errors.Is(err, store.ErrAlreadyExists):
			Conflict(w, fmt.Sprintf("message policy %q already exists in this project", candidate.Name))
		case errors.Is(err, store.ErrInvalidInput):
			ValidationError(w, err.Error(), nil)
		default:
			writeErrorFromErr(w, err, "Message policy")
		}
		return
	}

	if ev := s.MessagePolicyEvaluator(); ev != nil {
		ev.Invalidate(existing.ID, prevRevision)
	}

	beforeSummary, _ := json.Marshal(map[string]any{
		"name":     existing.Name,
		"mode":     existing.Mode,
		"enabled":  existing.Enabled,
		"revision": prevRevision,
	})
	afterSummary, _ := json.Marshal(map[string]any{
		"name":     candidate.Name,
		"mode":     candidate.Mode,
		"enabled":  candidate.Enabled,
		"revision": candidate.Revision,
	})
	s.emitMutationAudit(ctx, &store.MutationAuditRecord{
		MutationType:  mutationTypeMessagePolicyUpdate,
		TargetType:    "project",
		TargetID:      project.ID,
		BeforeSummary: string(beforeSummary),
		AfterSummary:  string(afterSummary),
	})

	writeJSON(w, http.StatusOK, toMessagePolicyResponse(*candidate))
}

func (s *Server) handleDeleteMessagePolicy(w http.ResponseWriter, r *http.Request, project *store.Project, policyRef string) {
	ctx := r.Context()
	existing, err := s.resolveProjectMessagePolicy(ctx, project.ID, policyRef)
	if err != nil {
		writeErrorFromErr(w, err, "Message policy")
		return
	}

	if err := s.store.DeleteMessagePolicy(ctx, existing.ID); err != nil {
		writeErrorFromErr(w, err, "Message policy")
		return
	}

	if ev := s.MessagePolicyEvaluator(); ev != nil {
		ev.Invalidate(existing.ID, existing.Revision)
	}

	beforeSummary, _ := json.Marshal(map[string]any{
		"policy_id":   existing.ID,
		"policy_name": existing.Name,
		"mode":        existing.Mode,
		"revision":    existing.Revision,
	})
	s.emitMutationAudit(ctx, &store.MutationAuditRecord{
		MutationType:  mutationTypeMessagePolicyDelete,
		TargetType:    "project",
		TargetID:      project.ID,
		BeforeSummary: string(beforeSummary),
		AfterSummary:  "deleted",
	})

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleTestMessagePolicy(w http.ResponseWriter, r *http.Request, project *store.Project, policyRef string) {
	ctx := r.Context()

	var stored *store.MessagePolicy
	if policyRef != "" {
		var err error
		stored, err = s.resolveProjectMessagePolicy(ctx, project.ID, policyRef)
		if err != nil {
			writeErrorFromErr(w, err, "Message policy")
			return
		}
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxMessagePolicyTestBodyBytes)
	var req TestMessagePolicyRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// Build effective policy spec: start from stored policy (when testing a
	// persisted policy), overlay inline fields, or default omitted metadata for
	// pure inline testing.
	inline := MessagePolicyInlineSpec{
		Name:              req.Name,
		Mode:              req.Mode,
		Scope:             req.Scope,
		ViolationRule:     req.ViolationRule,
		SystemOneRule:     req.SystemOneRule,
		OnUncertain:       req.OnUncertain,
		OnFailed:          req.OnFailed,
		OnFailedReasons:   req.OnFailedReasons,
		RejectReason:      req.RejectReason,
		RejectRemediation: req.RejectRemediation,
	}
	if req.Policy != nil {
		inline = *req.Policy
	}

	var effective store.MessagePolicy
	if stored != nil {
		effective = *stored.Clone()
		if inline.Name != "" {
			effective.Name = strings.TrimSpace(inline.Name)
		}
		if inline.Mode != "" {
			effective.Mode = inline.Mode
		}
		if inline.Scope != nil {
			effective.Scope = inline.Scope.Clone()
		}
		if inline.ViolationRule != "" {
			effective.ViolationRule = inline.ViolationRule
		}
		if inline.SystemOneRule != "" {
			effective.SystemOneRule = inline.SystemOneRule
		}
		if inline.OnUncertain != "" {
			effective.OnUncertain = strings.TrimSpace(inline.OnUncertain)
		}
		if inline.OnFailed != "" {
			effective.OnFailed = strings.TrimSpace(inline.OnFailed)
		}
		if inline.OnFailedReasons != nil {
			effective.OnFailedReasons = inline.OnFailedReasons
		}
		if inline.RejectReason != "" {
			effective.RejectReason = inline.RejectReason
		}
		if inline.RejectRemediation != "" {
			effective.RejectRemediation = inline.RejectRemediation
		}
	} else {
		name := strings.TrimSpace(inline.Name)
		if name == "" {
			name = "inline-test"
		}
		var scope store.MessagePolicyScope
		if inline.Scope != nil {
			scope = inline.Scope.Clone()
		} else {
			scope = store.MessagePolicyScope{
				Surfaces: messagepolicy.Coverage().EnforcedSurfaces,
			}
		}
		effective = store.MessagePolicy{
			ProjectID:         project.ID,
			Name:              name,
			Enabled:           true,
			Mode:              inline.Mode,
			Scope:             scope,
			ViolationRule:     inline.ViolationRule,
			SystemOneRule:     inline.SystemOneRule,
			OnUncertain:       strings.TrimSpace(inline.OnUncertain),
			OnFailed:          strings.TrimSpace(inline.OnFailed),
			OnFailedReasons:   inline.OnFailedReasons,
			RejectReason:      inline.RejectReason,
			RejectRemediation: inline.RejectRemediation,
		}
	}

	spec := messagepolicy.AuthoringSpec{
		Name:              effective.Name,
		Mode:              effective.Mode,
		Scope:             authoringScopeFromStore(effective.Scope),
		ViolationRule:     effective.ViolationRule,
		SystemOneRule:     effective.SystemOneRule,
		OnUncertain:       effective.OnUncertain,
		OnFailed:          effective.OnFailed,
		OnFailedReasons:   effective.OnFailedReasons,
		RejectReason:      effective.RejectReason,
		RejectRemediation: effective.RejectRemediation,
	}
	mode, canonicalSenderIDs, ok := s.validateMessagePolicyAuthoring(ctx, w, project.ID, spec, false)
	if !ok {
		return
	}
	effective.Mode = mode
	effective.Scope.Sender.AgentIDs = canonicalSenderIDs

	sample := MessagePolicyTestSample{
		Surface: req.Surface,
		Body:    req.Body,
	}
	if req.Sample != nil {
		sample = *req.Sample
	}

	surface := strings.TrimSpace(sample.Surface)
	if surface == "" {
		if len(effective.Scope.Surfaces) > 0 {
			surface = effective.Scope.Surfaces[0]
		} else {
			surface = store.MessagePolicySurfaceAgentToUser
		}
	}
	if !messagepolicy.IsCoveredSurface(surface) {
		cov := messagepolicy.Coverage()
		writeError(w, http.StatusBadRequest, messagepolicy.ErrCodeSurfaceNotCovered,
			fmt.Sprintf("sample.surface %q is not covered; enforced surfaces: %s",
				surface, strings.Join(cov.EnforcedSurfaces, ", ")),
			map[string]interface{}{
				"field":                 "sample.surface",
				"enforced_surfaces":     cov.EnforcedSurfaces,
				"uncovered_agent_paths": cov.UncoveredAgentPaths,
			})
		return
	}

	sender := messagepolicy.SenderFacts{
		Kind:      "agent",
		ID:        "sample-agent",
		Slug:      "sample-agent",
		ProjectID: project.ID,
	}
	if sample.Sender != nil {
		sender = *sample.Sender
		sender.ID = strings.TrimSpace(sender.ID)
		if sender.ID != "" {
			if parsedID, parseErr := uuid.Parse(sender.ID); parseErr == nil {
				sender.ID = parsedID.String()
			}
		}
		if sender.Kind == "" {
			sender.Kind = "agent"
		}
		if sender.ProjectID == "" {
			sender.ProjectID = project.ID
		}
	}

	defaultRecipKind := "user"
	defaultConvKind := "direct"
	if surface == store.MessagePolicySurfaceAgentToConversation {
		defaultRecipKind = "conversation"
		defaultConvKind = "group"
	}
	recipient := messagepolicy.RecipientFacts{
		Kind: defaultRecipKind,
		ID:   "sample-recipient",
		Slug: "sample-recipient",
	}
	if sample.Recipient != nil {
		recipient = *sample.Recipient
		if recipient.Kind == "" {
			recipient.Kind = defaultRecipKind
		}
	}
	if recipient.Kind == "agent" {
		cov := messagepolicy.Coverage()
		writeError(w, http.StatusBadRequest, messagepolicy.ErrCodeSurfaceNotCovered,
			"agent recipients are not covered by this Hub",
			map[string]interface{}{
				"field":                 "sample.recipient.kind",
				"enforced_surfaces":     cov.EnforcedSurfaces,
				"uncovered_agent_paths": cov.UncoveredAgentPaths,
			})
		return
	}

	conv := messagepolicy.ConversationFacts{
		Kind: defaultConvKind,
	}
	if sample.Conversation != nil {
		conv = *sample.Conversation
		if conv.Kind == "" {
			conv.Kind = defaultConvKind
		}
	}

	msgType := sample.Type
	if msgType == "" {
		msgType = "message"
	}

	_, bodyBytes, bodyTruncated := messagepolicy.TruncateBodyUTF8(sample.Body, messagepolicy.MaxBodyBytes)
	scopeMatched := effective.Scope.Matches(surface, sender.ID, recipient.Kind, "")
	if !scopeMatched {
		writeJSON(w, http.StatusOK, TestMessagePolicyResponse{
			PolicyID:       effective.ID,
			PolicyName:     effective.Name,
			PolicyRevision: effective.Revision,
			Mode:           effective.Mode,
			Surface:        surface,
			ScopeMatched:   false,
			Class:          store.MessagePolicyClassPass,
			Outcome:        messagepolicy.OutcomeAdmit,
			Action:         store.MessagePolicyActionAdmitted,
			BodyBytes:      bodyBytes,
			BodyTruncated:  bodyTruncated,
		})
		return
	}

	var judgments messagepolicy.Judgments
	manualJudgments := false
	if sample.Judgments != nil {
		judgments = *sample.Judgments
		manualJudgments = true
	}
	if len(sample.JudgmentFlags) > 0 {
		judgments.Flags = sample.JudgmentFlags
		manualJudgments = true
	}
	if len(sample.JudgmentLabels) > 0 {
		judgments.Labels = sample.JudgmentLabels
		manualJudgments = true
	}

	facts := messagepolicy.Facts{
		Surface:   surface,
		Sender:    sender,
		Recipient: recipient,
		Message: messagepolicy.MessageFacts{
			Type:            msgType,
			Body:            sample.Body,
			HasChoices:      sample.HasChoices,
			Urgent:          sample.Urgent,
			AttachmentCount: sample.AttachmentCount,
		},
		MentionedAgentIDs: sample.MentionedAgentIDs,
		MentionedUserIDs:  sample.MentionedUserIDs,
		Conversation:      conv,
		Judgments:         judgments,
	}

	hasSystemOne := strings.TrimSpace(effective.SystemOneRule) != ""
	if hasSystemOne && !manualJudgments {
		if !s.authorizeMessagePolicyTestSystemOne(w, r, project, effective.SystemOneRule) {
			return
		}
	}

	var cls messagepolicy.Classification
	var elapsedMs int64
	if !hasSystemOne && bodyTruncated {
		cls = messagepolicy.Classification{
			Class:  messagepolicy.ClassFailed,
			Reason: messagepolicy.ReasonBodyTruncated,
		}
	} else {
		ev := s.MessagePolicyEvaluator()
		start := time.Now()
		if hasSystemOne && !manualJudgments {
			soBudget := s.messagePolicyGate.systemOneSendBudget()
			cls = ev.EvaluatePolicyTest(
				ctx,
				effective.ID,
				effective.Revision,
				effective.ViolationRule,
				effective.SystemOneRule,
				project.ID,
				facts,
				[]messagepolicy.EvalOption{messagepolicy.WithBudget(messagepolicy.NewSendBudget())},
				[]messagepolicy.EvalOption{messagepolicy.WithMaxEvalTime(soBudget)},
			)
			judgments = cls.Judgments
		} else {
			act := messagepolicy.NewActivation(facts)
			cls = ev.EvaluateExpr(ctx, effective.ViolationRule, act,
				messagepolicy.WithBudget(messagepolicy.NewSendBudget()),
			)
		}
		elapsedMs = time.Since(start).Milliseconds()
		if elapsedMs < 0 {
			elapsedMs = 0
		}
	}

	outcome := messagepolicy.ResolveOutcome(
		cls.Class,
		cls.Reason,
		effective.OnUncertain,
		effective.OnFailed,
		effective.OnFailedReasons,
	)

	var action string
	switch {
	case effective.Mode == store.MessagePolicyModeShadow:
		action = store.MessagePolicyActionShadow
	case outcome == messagepolicy.OutcomeReject:
		action = store.MessagePolicyActionRejected
	default:
		action = store.MessagePolicyActionAdmitted
	}

	var reason, remediation string
	if outcome == messagepolicy.OutcomeReject || cls.Class == messagepolicy.ClassViolation {
		recipSlug := recipient.Slug
		if recipSlug == "" {
			recipSlug = recipient.ID
		}
		tplData := messagepolicy.TemplateData{
			Policy:         effective.Name,
			Recipient:      recipSlug,
			JudgmentFlags:  judgments.Flags,
			JudgmentLabels: judgments.Labels,
		}
		reason = renderPolicyRejectReason(&effective, tplData)
		remediation = messagepolicy.RenderTemplate(effective.RejectRemediation, tplData)
	}

	writeJSON(w, http.StatusOK, TestMessagePolicyResponse{
		PolicyID:       effective.ID,
		PolicyName:     effective.Name,
		PolicyRevision: effective.Revision,
		Mode:           effective.Mode,
		Surface:        surface,
		ScopeMatched:   true,
		Class:          cls.Class,
		ReasonCode:     cls.Reason,
		Outcome:        outcome,
		Action:         action,
		Reason:         reason,
		Remediation:    remediation,
		Evaluation:     messagepolicy.MarshalEvaluationProjection(cls.Evaluation),
		ElapsedMs:      elapsedMs,
		BodyBytes:      bodyBytes,
		BodyTruncated:  bodyTruncated,
	})
}

func (s *Server) handleListMessagePolicyDecisions(w http.ResponseWriter, r *http.Request, project *store.Project, policyRef string) {
	ctx := r.Context()
	q := r.URL.Query()

	filter := store.MessagePolicyDecisionFilter{
		ProjectID: project.ID,
	}

	if policyRef != "" {
		pol, err := s.resolveProjectMessagePolicy(ctx, project.ID, policyRef)
		if err != nil {
			writeErrorFromErr(w, err, "Message policy")
			return
		}
		filter.PolicyID = pol.ID
	} else if qPolicy := strings.TrimSpace(q.Get("policy_id")); qPolicy != "" {
		pol, err := s.resolveProjectMessagePolicy(ctx, project.ID, qPolicy)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// Allow filtering by an already-deleted policy UUID so historical
				// decision rows remain queryable after policy deletion; unknown
				// non-UUID refs return 404 (matching /{policyId}/decisions).
				if parsedID, parseErr := uuid.Parse(qPolicy); parseErr == nil && parsedID != uuid.Nil && len(qPolicy) == 36 {
					filter.PolicyID = parsedID.String()
				} else {
					writeErrorFromErr(w, err, "Message policy")
					return
				}
			} else {
				writeErrorFromErr(w, err, "Message policy")
				return
			}
		} else {
			filter.PolicyID = pol.ID
		}
	}

	if mode := strings.TrimSpace(q.Get("mode")); mode != "" {
		if !store.IsValidMessagePolicyMode(mode) {
			ValidationError(w, fmt.Sprintf("mode must be %q or %q", store.MessagePolicyModeShadow, store.MessagePolicyModeEnforce),
				map[string]interface{}{"field": "mode"})
			return
		}
		filter.Mode = mode
	}
	if surface := strings.TrimSpace(q.Get("surface")); surface != "" {
		filter.Surface = surface
	}
	if class := strings.TrimSpace(q.Get("class")); class != "" {
		if !store.IsValidMessagePolicyClass(class) {
			ValidationError(w, "invalid decision class filter", map[string]interface{}{"field": "class"})
			return
		}
		filter.Class = class
	}
	if action := strings.TrimSpace(q.Get("action")); action != "" {
		if !store.IsValidMessagePolicyDecisionAction(action) {
			ValidationError(w, "invalid decision action filter", map[string]interface{}{"field": "action"})
			return
		}
		filter.Action = action
	}

	limit, ok := parseBoundedLimit(strings.TrimSpace(q.Get("limit")), defaultMessagePolicyDecisionLimit, maxMessagePolicyDecisionLimit)
	if !ok {
		ValidationError(w, "limit must be a positive integer", map[string]interface{}{"field": "limit"})
		return
	}

	res, err := s.store.ListMessagePolicyDecisions(ctx, filter, store.ListOptions{
		Limit:  limit,
		Cursor: strings.TrimSpace(q.Get("cursor")),
	})
	if err != nil {
		if errors.Is(err, store.ErrInvalidCursor) {
			writeInvalidCursor(w)
			return
		}
		writeErrorFromErr(w, err, "Message policy decisions")
		return
	}

	items := res.Items
	if items == nil {
		items = []store.MessagePolicyDecision{}
	}
	writeJSON(w, http.StatusOK, ListMessagePolicyDecisionsResponse{
		Decisions:  items,
		NextCursor: res.NextCursor,
		TotalCount: res.TotalCount,
	})
}
