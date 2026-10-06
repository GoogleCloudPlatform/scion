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

package runtimebroker

import (
	"fmt"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// FlatInstanceConfig makes a Runtime Broker server host exactly one flat
// Runtime Broker instance (.design/flat-runtime-brokers-contract.md): one
// persisted identity bound to one runtime target. The server's manager and
// runtime are that target's; profile resolution (settings active_profile,
// saved agent profiles, auxiliary runtimes) is never consulted.
type FlatInstanceConfig struct {
	// Identity is the instance's persisted identity (brokeridentity). Its
	// RuntimeBrokerID is the server's BrokerID and its RuntimeTarget the
	// single target every create, start and restart is checked against.
	Identity *brokeridentity.Identity
	// Instance is the strictly loaded server.broker.instances entry.
	Instance config.V1RuntimeBrokerInstanceConfig
	// HubInProcess is exactly the predicate that admits the embedded
	// registration (colocatedBrokerRegisters). P1 hosts a flat instance only
	// co-located with its Hub; Start refuses otherwise.
	HubInProcess bool
}

// flatRuntimeTargetRequiredMessage is the frozen 412 runtime_target_required
// message (section 10).
const flatRuntimeTargetRequiredMessage = "this Runtime Broker serves a single runtime target and requires expectedRuntimeTargetId; upgrade the Hub"

// flatInstance returns the hosted flat instance configuration, or nil for a
// legacy (profile-resolving) Runtime Broker.
func (s *Server) flatInstance() *FlatInstanceConfig {
	if s == nil || s.config.FlatInstance == nil || s.config.FlatInstance.Identity == nil {
		return nil
	}
	return s.config.FlatInstance
}

// isFlat reports whether this server hosts a flat Runtime Broker instance.
func (s *Server) isFlat() bool { return s.flatInstance() != nil }

// flatHostingError is the P1 fail-closed gate for a flat instance: it refuses
// a flat instance whose Hub is not in the same process. Nil for a legacy
// server or a co-located flat instance.
func (s *Server) flatHostingError() error {
	fi := s.flatInstance()
	if fi == nil {
		return nil
	}
	err := config.CheckRuntimeBrokerInstanceHosting([]config.V1RuntimeBrokerInstanceConfig{fi.Instance}, fi.HubInProcess)
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", api.ErrCodeFlatRuntimeBrokerRemoteUnsupported, err)
}

// runtimeTargetRefusal is a flat-contract refusal written before any side
// effect. It never carries start markers.
type runtimeTargetRefusal struct {
	status  int
	code    string
	message string
	details map[string]interface{}
}

func (r *runtimeTargetRefusal) write(w http.ResponseWriter) {
	writeError(w, r.status, r.code, r.message, r.details)
}

// expectedTargetRefusal checks a request's expectedRuntimeTargetId against
// the target this server serves. A flat instance compares it with its
// target; a legacy Runtime Broker serves no target, so any non-empty value
// is a mismatch (it is never ignored). requireExpected (create only) makes a
// missing value on a flat instance a 412.
func (s *Server) expectedTargetRefusal(expected string, requireExpected bool) *runtimeTargetRefusal {
	brokerID := s.config.BrokerID
	actual := ""
	if fi := s.flatInstance(); fi != nil {
		brokerID = fi.Identity.RuntimeBrokerID
		actual = fi.Identity.RuntimeTarget.ID
		if expected == "" && requireExpected {
			return &runtimeTargetRefusal{
				status:  http.StatusPreconditionFailed,
				code:    ErrCodeRuntimeTargetRequired,
				message: flatRuntimeTargetRequiredMessage,
				details: map[string]interface{}{"runtimeBrokerId": brokerID},
			}
		}
	}
	if m := api.CheckExpectedRuntimeTarget(brokerID, actual, expected); m != nil {
		return &runtimeTargetRefusal{
			status:  http.StatusConflict,
			code:    ErrCodeRuntimeTargetMismatch,
			message: m.Message(),
			details: m.Details(),
		}
	}
	return nil
}

// createRuntimeTargetRefusal applies the flat-contract create checks in their
// frozen order (decode has already passed): target required (flat only),
// then mismatch, then an explicit profile (flat only). It runs before
// beginCreateAttempt, so a refusal leaves no attempt record, launch registry
// entry, project directory or runtime call, and a requestId replay gets the
// same answer.
func (s *Server) createRuntimeTargetRefusal(req *CreateAgentRequest) *runtimeTargetRefusal {
	if r := s.expectedTargetRefusal(req.ExpectedRuntimeTargetID, true); r != nil {
		return r
	}
	fi := s.flatInstance()
	if fi != nil && req.Config != nil && req.Config.Profile != "" {
		return &runtimeTargetRefusal{
			status: http.StatusUnprocessableEntity,
			code:   ErrCodeRuntimeProfileUnsupported,
			message: fmt.Sprintf("Runtime Broker %s serves a single runtime target and does not accept Runtime Broker Profile %q",
				fi.Identity.RuntimeBrokerID, req.Config.Profile),
			details: map[string]interface{}{
				"runtimeBrokerId": fi.Identity.RuntimeBrokerID,
				"profile":         req.Config.Profile,
			},
		}
	}
	return nil
}
