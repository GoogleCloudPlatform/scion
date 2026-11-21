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
	brokerPolicyRegisterRemedy = "Ask a hub administrator to register the broker with --admin-managed."
	brokerPolicyUATRemedy      = "Sign in as a hub administrator and run 'scion runtime-broker register --admin-managed', or mint a join token with 'scion hub broker join-token --admin-managed' from a signed-in machine and run '/brokers/join' on the host."
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

// writeBrokerPolicyDenial logs and writes the structured 403 refusal for a
// broker policy denial (design ptone/scion#7829 §4.4).
func writeBrokerPolicyDenial(w http.ResponseWriter, r *http.Request, op string, b *store.RuntimeBroker) {
	writeBrokerPolicyDenialWithRemedy(w, r, op, b, "")
}

func writeBrokerPolicyDenialWithRemedy(w http.ResponseWriter, r *http.Request, op string, b *store.RuntimeBroker, remedy string) {
	if remedy == "" {
		switch op {
		case brokerPolicyOpRegister:
			remedy = brokerPolicyRegisterRemedy
		case brokerPolicyOpReregister:
			if b != nil && b.Name != "" {
				remedy = fmt.Sprintf("Ask a hub administrator to approve broker %s, or register an admin-managed broker with --admin-managed.", b.Name)
			} else {
				remedy = brokerPolicyRegisterRemedy
			}
		default:
			if b != nil && b.Name != "" {
				remedy = fmt.Sprintf("Ask a hub administrator to approve broker %s, or choose an admin-managed broker.", b.Name)
			} else {
				remedy = "Ask a hub administrator to approve the broker, or choose an admin-managed broker."
			}
		}
	}

	details := map[string]interface{}{
		"setting":   brokerPolicySettingKey,
		"operation": op,
	}
	res := Resource{Type: "broker"}
	if b != nil && b.ID != "" {
		details["brokerId"] = b.ID
		res = brokerResource(b)
	}
	if r != nil {
		logAuthzDenial(r, GetIdentityFromContext(r.Context()), res, Action(op), brokerPolicyDenialReason)
	}
	msg := fmt.Sprintf("%s refused: this hub only allows admin-managed Runtime Brokers (setting %s). %s",
		op, brokerPolicySettingKey, remedy)
	writeError(w, http.StatusForbidden, ErrCodeBrokerPolicyAdminManagedOnly, msg, details)
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
