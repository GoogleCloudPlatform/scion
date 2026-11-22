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
	"log/slog"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	brokerPolicySettingKey     = "broker_policy.admin_managed_only"
	brokerPolicyDenialReason   = "broker_policy"
	brokerPolicyOpRegister     = "register"
	brokerPolicyOpReregister   = "reregister"
	brokerPolicyOpAttach       = "attach"
	brokerPolicyOpDispatch     = "dispatch"
	brokerPolicyOpMove         = "move"
	brokerPolicyOpStart        = "start"
	brokerPolicyRegisterRemedy = "Ask a hub administrator to register the broker with --admin-managed."
	brokerPolicyUATRemedy      = "Sign in as a hub administrator and run 'scion runtime-broker register --admin-managed', or mint a join token with 'scion hub brokers join-token create <broker-name> --admin-managed' from a signed-in machine and run 'scion runtime-broker join' on the host."
	brokerPolicyEmbeddedRemedy = "Use 'scion runtime-broker register --admin-managed' from a signed-in hub administrator account."
)

// isAdminManagedBroker reports whether b qualifies as an admin-managed
// (approved) Runtime Broker under broker_policy.admin_managed_only
// (ptone/scion#7829). This is the single definition across the Hub.
func isAdminManagedBroker(b *store.RuntimeBroker) bool {
	return b != nil && (b.CreatedBy == "" || // operator-provisioned / co-located (embedded) broker
		b.AutoProvide || // offered hub-wide; only broker.auto_provide holders can set it
		b.AdminManaged) // explicit approval (persisted column)
}

// brokerPolicyAdminManagedOnlyConfigured reports whether the stored/configured
// broker_policy.admin_managed_only setting is true, independent of the
// hub.admin_managed_brokers experiment flag.
func (s *Server) brokerPolicyAdminManagedOnlyConfigured() bool {
	s.mu.RLock()
	v := s.config.BrokerPolicyAdminManagedOnly
	s.mu.RUnlock()
	return v != nil && *v
}

// adminManagedBrokersOnly reports whether the admin-managed-only Runtime Broker
// policy is actively enforced (both experiment hub.admin_managed_brokers and
// setting broker_policy.admin_managed_only are on). Single reader across the Hub.
func (s *Server) adminManagedBrokersOnly() bool {
	return s.experimentEnabled(experiments.AdminManagedBrokers) && s.brokerPolicyAdminManagedOnlyConfigured()
}

// WarnBrokerPolicyExperimentMismatch logs a startup warning when
// broker_policy.admin_managed_only is configured true while experiment
// hub.admin_managed_brokers is off (so enforcement is inactive).
func (s *Server) WarnBrokerPolicyExperimentMismatch() {
	if s.brokerPolicyAdminManagedOnlyConfigured() && !s.experimentEnabled(experiments.AdminManagedBrokers) {
		slog.Warn("broker_policy.admin_managed_only is true, but experiment hub.admin_managed_brokers is off — admin-managed broker policy is not enforced",
			"setting", brokerPolicySettingKey,
			"experiment", experiments.AdminManagedBrokers)
	}
}

// writeBrokerPolicyExperimentDisabled writes the 412 experiment_disabled
// refusal when saving broker_policy.admin_managed_only=true or sending
// adminManaged on broker registration while hub.admin_managed_brokers is off.
func writeBrokerPolicyExperimentDisabled(w http.ResponseWriter) {
	writeError(w, http.StatusPreconditionFailed, ErrCodeExperimentDisabled,
		"broker_policy.admin_managed_only and admin-managed broker registration are not enabled on this Hub (experiment "+experiments.AdminManagedBrokers+" is off)",
		map[string]interface{}{"experiment": experiments.AdminManagedBrokers})
}

// brokerPolicyRefuses reports whether the active admin-managed-only policy
// refuses operations involving b.
func (s *Server) brokerPolicyRefuses(b *store.RuntimeBroker) bool {
	return s.adminManagedBrokersOnly() && !isAdminManagedBroker(b)
}

func brokerPolicyRemedy(op string, b *store.RuntimeBroker) string {
	switch op {
	case brokerPolicyOpRegister:
		return brokerPolicyRegisterRemedy
	case brokerPolicyOpReregister:
		if b != nil && b.Name != "" {
			return fmt.Sprintf("Ask a hub administrator to approve broker %s, or register an admin-managed broker with --admin-managed.", b.Name)
		}
		return brokerPolicyRegisterRemedy
	default:
		if b != nil && b.Name != "" {
			return fmt.Sprintf("Ask a hub administrator to approve broker %s, or choose an admin-managed broker.", b.Name)
		}
		return "Ask a hub administrator to approve the broker, or choose an admin-managed broker."
	}
}

func brokerPolicyMessage(op string, b *store.RuntimeBroker, remedy string) string {
	if remedy == "" {
		remedy = brokerPolicyRemedy(op, b)
	}
	return fmt.Sprintf("%s refused: this hub only allows admin-managed Runtime Brokers (setting %s). %s",
		op, brokerPolicySettingKey, remedy)
}

func brokerPolicyDetails(op string, b *store.RuntimeBroker) map[string]interface{} {
	details := map[string]interface{}{
		"setting":   brokerPolicySettingKey,
		"operation": op,
	}
	if b != nil && b.ID != "" {
		details["brokerId"] = b.ID
	}
	return details
}

// brokerPolicyEligible reports whether b is eligible under the current
// admin-managed-only broker policy: true when the policy is off, or
// isAdminManagedBroker(b) when the policy is on (design ptone/scion#7829 §4.2).
func (s *Server) brokerPolicyEligible(b *store.RuntimeBroker) bool {
	return !s.brokerPolicyRefuses(b)
}

// authorizeBrokerManage enforces the gate for approving or revoking a broker's
// admin-managed flag on PATCH /api/v1/runtime-brokers/{id} (design
// ptone/scion#7829 §3.1, §3.2, §4.1, §4.7.2):
//   - interactive session or dev credential only (no UAT);
//   - test-admin identities refused via testAdminPolicyRefusesUser;
//   - broker.manage permission (not broker.update; owners cannot self-approve);
//   - experiment hub.admin_managed_brokers must be on (412 experiment_disabled).
func (s *Server) authorizeBrokerManage(w http.ResponseWriter, r *http.Request, actor UserIdentity, broker *store.RuntimeBroker) bool {
	ctx := r.Context()
	if !brokerUserCredentialKindAdmitted(ctx, actor, false) {
		logAuthzDenial(r, actor, brokerResource(broker), ActionManage, "credential kind not admitted for broker.manage")
		Forbidden(w)
		return false
	}
	if s.testAdminPolicyRefusesUser(ctx, actor.ID(), "broker_policy.go:authorizeBrokerManage") {
		logAuthzDenial(r, actor, brokerResource(broker), ActionManage, testAdminPolicyDenyReason)
		Forbidden(w)
		return false
	}
	decision := s.authzService.CheckAccess(ctx, actor, Resource{Type: "broker"}, ActionManage)
	if !decision.Allowed {
		logAuthzDenial(r, actor, brokerResource(broker), ActionManage, decision.Reason)
		Forbidden(w)
		return false
	}
	if !s.experimentEnabled(experiments.AdminManagedBrokers) {
		writeBrokerPolicyExperimentDisabled(w)
		return false
	}
	return true
}

// callerCanManageBrokers reports whether the caller in ctx holds broker.manage
// on an admitted credential (interactive or dev) for UI capability reporting.
func (s *Server) callerCanManageBrokers(ctx context.Context) bool {
	if s.authzService == nil {
		return false
	}
	user := GetUserIdentityFromContext(ctx)
	if user == nil || !brokerUserCredentialKindAdmitted(ctx, user, false) {
		return false
	}
	return s.authzService.CheckAccess(ctx, user, Resource{Type: "broker"}, ActionManage).Allowed
}

// reportBrokerManageCapability appends ActionManage to caps when allowed.
func reportBrokerManageCapability(caps *Capabilities, allowed bool) {
	if caps == nil || !allowed || capabilityAllows(caps, ActionManage) {
		return
	}
	caps.Actions = append(caps.Actions, string(ActionManage))
}

// writeBrokerPolicyDenial logs and writes the structured 403 refusal for a
// broker policy denial (design ptone/scion#7829 §4.4).
func writeBrokerPolicyDenial(w http.ResponseWriter, r *http.Request, op string, b *store.RuntimeBroker) {
	writeBrokerPolicyDenialWithRemedy(w, r, op, b, "")
}

func writeBrokerPolicyDenialWithRemedy(w http.ResponseWriter, r *http.Request, op string, b *store.RuntimeBroker, remedy string) {
	writeBrokerPolicyDenialForIdentity(w, r, nil, op, b, remedy)
}

// writeBrokerPolicyDenialForIdentity logs and writes the structured 403 refusal
// for a broker policy denial. r may be nil when the caller has no request at
// hand (such as agent-create auto-link), in which case identity is still logged
// via logAuthzDenial with reason broker_policy.
func writeBrokerPolicyDenialForIdentity(w http.ResponseWriter, r *http.Request, identity Identity, op string, b *store.RuntimeBroker, remedy string) {
	writeBrokerPolicyDenialFull(w, r, identity, op, b, remedy, nil)
}

func writeBrokerPolicyDenialCtx(ctx context.Context, w http.ResponseWriter, r *http.Request, op string, b *store.RuntimeBroker, remedy string, extraDetails map[string]interface{}) {
	var identity Identity
	if r != nil {
		identity = GetIdentityFromContext(r.Context())
	} else if ctx != nil {
		identity = GetIdentityFromContext(ctx)
	}
	writeBrokerPolicyDenialFull(w, r, identity, op, b, remedy, extraDetails)
}

func writeBrokerPolicyDenialFull(w http.ResponseWriter, r *http.Request, identity Identity, op string, b *store.RuntimeBroker, remedy string, extraDetails map[string]interface{}) {
	details := brokerPolicyDetails(op, b)
	for k, v := range extraDetails {
		details[k] = v
	}
	res := Resource{Type: "broker"}
	if b != nil && b.ID != "" {
		res = brokerResource(b)
	}
	if identity == nil && r != nil {
		identity = GetIdentityFromContext(r.Context())
	}
	logAuthzDenial(r, identity, res, Action(op), brokerPolicyDenialReason)
	writeError(w, http.StatusForbidden, ErrCodeBrokerPolicyAdminManagedOnly,
		brokerPolicyMessage(op, b, remedy), details)
}

// brokerPolicyError is a typed error returned by internal enforcement
// chokepoints (such as startAgentCore and selectProjectRuntimeBroker) when
// broker_policy.admin_managed_only refuses an operation (ptone/scion#7829).
type brokerPolicyError struct {
	Operation    string
	Broker       *store.RuntimeBroker
	Remedy       string
	ExtraDetails map[string]interface{}
}

func (e *brokerPolicyError) Error() string {
	return brokerPolicyMessage(e.Operation, e.Broker, e.Remedy)
}

func (e *brokerPolicyError) Details() map[string]interface{} {
	d := brokerPolicyDetails(e.Operation, e.Broker)
	for k, v := range e.ExtraDetails {
		d[k] = v
	}
	return d
}

// writeBrokerPolicyError writes the §4.4 403 response if err wraps a
// *brokerPolicyError and reports whether it handled the error.
func writeBrokerPolicyError(w http.ResponseWriter, r *http.Request, err error) bool {
	var ctx context.Context
	if r != nil {
		ctx = r.Context()
	}
	return writeBrokerPolicyErrorCtx(ctx, w, r, err)
}

func writeBrokerPolicyErrorCtx(ctx context.Context, w http.ResponseWriter, r *http.Request, err error) bool {
	var bpe *brokerPolicyError
	if !errors.As(err, &bpe) {
		return false
	}
	writeBrokerPolicyDenialCtx(ctx, w, r, bpe.Operation, bpe.Broker, bpe.Remedy, bpe.ExtraDetails)
	return true
}

// logBrokerPolicyDenial logs a non-HTTP broker policy refusal (such as message
// wake or scheduled dispatch) with reason broker_policy (design ptone/scion#7829 §4.7.7).
func logBrokerPolicyDenial(ctx context.Context, op string, b *store.RuntimeBroker) {
	res := Resource{Type: "broker"}
	if b != nil && b.ID != "" {
		res = brokerResource(b)
	}
	var identity Identity
	if ctx != nil {
		identity = GetIdentityFromContext(ctx)
	}
	logAuthzDenial(nil, identity, res, Action(op), brokerPolicyDenialReason)
}

// brokerPolicyDMError maps a *brokerPolicyError to a non-retryable 403
// AgentDMError for the message wake path (ptone/scion#7829 §4.3.D) and logs
// the policy denial (§4.7.7).
func brokerPolicyDMError(ctx context.Context, err error) *AgentDMError {
	var bpe *brokerPolicyError
	if !errors.As(err, &bpe) {
		return nil
	}
	logBrokerPolicyDenial(ctx, bpe.Operation, bpe.Broker)
	return &AgentDMError{
		Code:       ErrCodeBrokerPolicyAdminManagedOnly,
		Message:    bpe.Error(),
		HTTPStatus: http.StatusForbidden,
		Details:    bpe.Details(),
	}
}

// checkBrokerPolicyForStart enforces broker_policy.admin_managed_only before
// starting, resuming, restarting, or waking an existing agent
// (ptone/scion#7829 §4.3.D). A missing broker row or empty RuntimeBrokerID
// keeps today's behavior; any other store lookup error fails closed (§4.7.5).
func (s *Server) checkBrokerPolicyForStart(ctx context.Context, agent *store.Agent) error {
	if agent == nil || agent.RuntimeBrokerID == "" || !s.adminManagedBrokersOnly() {
		return nil
	}
	broker, err := s.store.GetRuntimeBroker(ctx, agent.RuntimeBrokerID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("failed to load runtime broker %q for policy check: %w", agent.RuntimeBrokerID, err)
	}
	if s.brokerPolicyRefuses(broker) {
		return &brokerPolicyError{
			Operation: brokerPolicyOpStart,
			Broker:    broker,
		}
	}
	return nil
}

// authorizeBrokerRegistrationPolicy enforces the admin-managed broker policy
// and explicit adminManaged registration requests on POST /api/v1/brokers
// (legacy and flat). Callers must have already passed authorizeBrokerCreate
// and (when existing != nil) brokerRemintTargetAuthorized.
//
// Returns (stampAdminManaged, ok). When ok is false, the HTTP error response
// has already been written.
func (s *Server) authorizeBrokerRegistrationPolicy(w http.ResponseWriter, r *http.Request, existing *store.RuntimeBroker, requestedManaged bool) (bool, bool) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Unauthorized(w)
		return false, false
	}

	// Sending adminManaged: true while the experiment is off always returns 412.
	if requestedManaged && !s.experimentEnabled(experiments.AdminManagedBrokers) {
		writeBrokerPolicyExperimentDisabled(w)
		return false, false
	}

	op := brokerPolicyOpRegister
	if existing != nil {
		op = brokerPolicyOpReregister
	}
	policyOn := s.adminManagedBrokersOnly()
	interactiveOrDev := brokerUserCredentialKindAdmitted(r.Context(), user, false)

	// While the policy is on, no user access token can register or re-register
	// a broker (including an admin's UAT; design D6).
	if policyOn && !interactiveOrDev {
		writeBrokerPolicyDenialWithRemedy(w, r, op, existing, brokerPolicyUATRemedy)
		return false, false
	}

	// When the caller explicitly requests adminManaged: true (policy on or off),
	// require an interactive or dev credential and broker.manage.
	if requestedManaged {
		if !interactiveOrDev {
			logAuthzDenial(r, user, Resource{Type: "broker"}, ActionManage, "credential kind not admitted for broker.manage")
			Forbidden(w)
			return false, false
		}
		decision := s.authzService.CheckAccess(r.Context(), user, Resource{Type: "broker"}, ActionManage)
		if !decision.Allowed {
			if policyOn {
				writeBrokerPolicyDenial(w, r, op, existing)
			} else {
				logAuthzDenial(r, user, Resource{Type: "broker"}, ActionManage, decision.Reason)
				Forbidden(w)
			}
			return false, false
		}
	}

	if existing != nil {
		if s.brokerPolicyRefuses(existing) {
			writeBrokerPolicyDenial(w, r, brokerPolicyOpReregister, existing)
			return false, false
		}
		return requestedManaged, true
	}

	// New row: when policy is on, registration must be explicitly adminManaged
	// (already verified interactive/dev + broker.manage above).
	if policyOn && !requestedManaged {
		writeBrokerPolicyDenial(w, r, brokerPolicyOpRegister, nil)
		return false, false
	}

	return requestedManaged, true
}

// authorizeEmbeddedBrokerRegistrationPolicy enforces the admin-managed broker
// policy on the deprecated POST /api/v1/projects/register embedded-broker path.
func (s *Server) authorizeEmbeddedBrokerRegistrationPolicy(w http.ResponseWriter, r *http.Request, existing *store.RuntimeBroker) bool {
	if !s.adminManagedBrokersOnly() {
		return true
	}
	if existing == nil {
		writeBrokerPolicyDenialWithRemedy(w, r, brokerPolicyOpRegister, nil, brokerPolicyEmbeddedRemedy)
		return false
	}
	// Defense in depth: handleProjectRegister already refuses non-session
	// credentials (including UATs) when req.Broker != nil via the
	// CREDENTIAL_MANAGEMENT gate (handlers_projects_core.go), so this branch
	// is currently unreachable from that handler but protects direct callers.
	user := GetUserIdentityFromContext(r.Context())
	if !brokerUserCredentialKindAdmitted(r.Context(), user, false) {
		writeBrokerPolicyDenialWithRemedy(w, r, brokerPolicyOpReregister, existing, brokerPolicyUATRemedy)
		return false
	}
	if !isAdminManagedBroker(existing) {
		writeBrokerPolicyDenial(w, r, brokerPolicyOpReregister, existing)
		return false
	}
	return true
}
