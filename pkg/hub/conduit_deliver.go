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
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Message delivery over the agent's conduit session: the hub relays one
// deliver-message flush from the agent's broker to the agent (design
// .design/conduit/v2.22-deliver-message.md §2). The hub never starts a
// delivery. The endpoint is fixed to the deliver-message route and the
// §1.2 schema: no request field names a route, a path or a target, and it
// can never become a passthrough to the agent's control handler (freeze
// note 3).

// maxDeliverRequestBody bounds the /deliver body: the largest valid
// request is MaxDeliverText of text, JSON-escaped, plus small fields.
const maxDeliverRequestBody = 1 << 20

// Outcomes the hub itself answers on /deliver (besides the agent's).
const (
	// deliverOutcomeNotReached: no eligible session, or the session does
	// not advertise deliver-message; nothing was sent (503).
	deliverOutcomeNotReached = "not_reached"
	// deliverOutcomeUnknown: the call timed out or dropped after it was
	// sent; the rows are partial (504).
	deliverOutcomeUnknown = "unknown"
)

// hubDeliverTimeout is the hub's call timeout for deliver-message
// (agentcontrol.HubDeliverTimeout); a variable so tests can shorten it.
var hubDeliverTimeout = agentcontrol.HubDeliverTimeout

// staleDeliveryOutcome is stored on rows the stale sweep moves to partial.
const staleDeliveryOutcome = `{"outcome":"partial","stage":"unknown"}`

// conduitDeliverEnabled reports whether /deliver serves: hub.conduit_deliver
// on and conduit routable (it requires hub.conduit; gap G6).
func (s *Server) conduitDeliverEnabled() bool {
	return s.experimentEnabled(experiments.ConduitDeliver) && s.conduitRoutable()
}

func writeDeliverAnswer(w http.ResponseWriter, status int, resp agentcontrol.DeliverResponse) {
	writeJSON(w, status, resp)
}

// handleBrokerAgentDeliver serves POST
// /api/v1/runtime-brokers/{brokerId}/agents/{agentId}/deliver (design
// v2.22 §2.1). Checks, in order: the experiment; the caller is the path
// broker; the body is the fixed schema; the agent (from the path, never
// the body) is assigned to that broker; then the atomic row bind (§2.6),
// which checks every message row exists and names this agent. Only then is
// the agent called, with broker_id and not_after_unix_ms set by the hub.
func (s *Server) handleBrokerAgentDeliver(w http.ResponseWriter, r *http.Request, brokerID, agentID string) {
	ctx := r.Context()
	if !s.conduitDeliverEnabled() {
		NotFound(w, "route")
		return
	}
	brokerIdent := GetBrokerIdentityFromContext(ctx)
	if brokerIdent == nil || brokerIdent.BrokerID() != brokerID {
		logAuthzDenial(r, GetIdentityFromContext(ctx), Resource{Type: permissions.ResourceBroker, ID: brokerID}, ActionUpdate,
			"deliver may only be called by the broker itself")
		Forbidden(w)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxDeliverRequestBody))
	if err != nil {
		writeDeliverAnswer(w, http.StatusRequestEntityTooLarge, agentcontrol.DeliverResponse{Outcome: agentcontrol.OutcomeInvalid, Field: agentcontrol.FieldBody})
		return
	}
	req, err := agentcontrol.DecodeDeliverRequest(body)
	if err == nil {
		err = req.ValidateFromBroker()
	}
	if err != nil {
		field := agentcontrol.FieldBody
		var fe *agentcontrol.FieldError
		if errors.As(err, &fe) {
			field = fe.Field
		}
		writeDeliverAnswer(w, http.StatusBadRequest, agentcontrol.DeliverResponse{Outcome: agentcontrol.OutcomeInvalid, Field: field})
		return
	}

	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Agent")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}
	if agent.RuntimeBrokerID != brokerID {
		logAuthzDenial(r, GetIdentityFromContext(ctx), Resource{Type: permissions.ResourceBroker, ID: brokerID}, ActionUpdate,
			"deliver for an agent not assigned to this broker")
		Forbidden(w)
		return
	}

	// The hub owns these fields: whatever the broker sent is replaced.
	sent := time.Now()
	req.BrokerID = brokerID
	req.NotAfterUnixMS = sent.Add(agentcontrol.DeliverNotAfter).UnixMilli()
	if err := req.Validate(); err != nil {
		writeDeliverAnswer(w, http.StatusBadRequest, agentcontrol.DeliverResponse{Outcome: agentcontrol.OutcomeInvalid, Field: agentcontrol.FieldBrokerID})
		return
	}

	log := s.messageLog.With("broker_id", brokerID, "agent_id", agent.ID, "project_id", agent.ProjectID,
		"delivery_id", req.DeliveryID, "messages", len(req.MessageIDs), "op", req.Op, "bytes", deliverTextBytes(req))

	bind, err := s.store.BindMessageDelivery(ctx, agent.ID, req.DeliveryID, req.MessageIDs, sent)
	switch {
	case errors.Is(err, store.ErrNotFound):
		log.Warn("message delivery refused: a message row is missing or addressed to another agent")
		writeDeliverAnswer(w, http.StatusBadRequest, agentcontrol.DeliverResponse{Outcome: agentcontrol.OutcomeInvalid, Field: agentcontrol.FieldMessageIDs})
		return
	case err != nil:
		writeErrorFromErr(w, err, "")
		return
	}
	switch bind.Result {
	case store.DeliveryConflict:
		log.Warn("message delivery refused: rows bound to another delivery or past delivering")
		writeDeliverAnswer(w, http.StatusConflict, agentcontrol.DeliverResponse{Outcome: agentcontrol.OutcomeBound})
		return
	case store.DeliveryReplay:
		resp, derr := agentcontrol.DecodeDeliverResponse([]byte(bind.Outcome))
		if derr != nil {
			resp = agentcontrol.DeliverResponse{Outcome: agentcontrol.OutcomePartial, Stage: "unknown"}
		}
		resp.Replayed = true
		log.Info("message delivery replayed from the stored outcome", "outcome", resp.Outcome)
		writeDeliverAnswer(w, http.StatusOK, resp)
		return
	}

	payload, err := json.Marshal(req)
	if err != nil {
		s.unbindDelivery(r, req)
		writeErrorFromErr(w, err, "")
		return
	}
	status, respBody, err := s.callAgentControlJSON(ctx, agent.ID, agentcontrol.RouteDeliverMessage, payload, hubDeliverTimeout)
	latency := time.Since(sent)
	if err != nil {
		if errors.Is(err, router.ErrNoSession) || errors.Is(err, ErrAgentControlNotAdvertised) {
			s.unbindDelivery(r, req)
			log.Info("message delivery not reached", "latency_ms", latency.Milliseconds(), "advertised", !errors.Is(err, ErrAgentControlNotAdvertised))
			writeDeliverAnswer(w, http.StatusServiceUnavailable, agentcontrol.DeliverResponse{Outcome: deliverOutcomeNotReached})
			return
		}
		// A timeout or a drop after send: the paste may have happened.
		s.finishDelivery(r, req, store.MessageDispatchPartial, staleDeliveryOutcome)
		log.Warn("message delivery outcome unknown", "latency_ms", latency.Milliseconds(), "error", err)
		writeDeliverAnswer(w, http.StatusGatewayTimeout, agentcontrol.DeliverResponse{Outcome: deliverOutcomeUnknown})
		return
	}
	resp, derr := agentcontrol.DecodeDeliverResponse(respBody)
	switch {
	case status == http.StatusOK && derr == nil && resp.Outcome == agentcontrol.OutcomeDelivered:
		s.finishDelivery(r, req, store.MessageDispatchDelivered, storedOutcome(resp))
	case status == http.StatusOK && derr == nil && resp.Outcome == agentcontrol.OutcomePartial,
		status == http.StatusConflict && resp.Outcome == agentcontrol.OutcomeInProgress:
		s.finishDelivery(r, req, store.MessageDispatchPartial, storedOutcome(agentcontrol.DeliverResponse{Outcome: agentcontrol.OutcomePartial, Stage: resp.Stage}))
	case status == http.StatusConflict && resp.Outcome == agentcontrol.OutcomeBusy,
		status == http.StatusBadRequest, status == http.StatusNotFound, status == http.StatusRequestTimeout,
		status == http.StatusRequestEntityTooLarge, status == http.StatusUnprocessableEntity,
		status == http.StatusTooManyRequests, status == http.StatusNotImplemented, status == http.StatusServiceUnavailable:
		// Nothing was pasted: the broker may retry or use exec.
		s.unbindDelivery(r, req)
	default:
		// Anything else, including a 200 that is not a valid answer, may
		// have pasted: never read as success, never re-driven.
		s.finishDelivery(r, req, store.MessageDispatchPartial, staleDeliveryOutcome)
		log.Warn("message delivery answered unexpectedly", "status", status, "latency_ms", latency.Milliseconds())
		writeDeliverAnswer(w, http.StatusBadGateway, agentcontrol.DeliverResponse{Outcome: deliverOutcomeUnknown})
		return
	}
	log.Info("message delivery over conduit", "status", status, "outcome", resp.Outcome, "stage", resp.Stage,
		"replayed", resp.Replayed, "latency_ms", latency.Milliseconds())
	if derr != nil {
		// A not-pasted status without a valid body (a session-level 429,
		// 501 or 503): answer the status with an outcome the broker reads.
		resp = agentcontrol.DeliverResponse{Outcome: deliverOutcomeNotReached}
	}
	writeDeliverAnswer(w, status, resp)
}

func deliverTextBytes(req *agentcontrol.DeliverRequest) int {
	if req.Text == nil {
		return 0
	}
	return len(*req.Text)
}

// storedOutcome renders the delivery_outcome stored on the rows: the
// outcome and stage only, never text.
func storedOutcome(resp agentcontrol.DeliverResponse) string {
	b, _ := json.Marshal(agentcontrol.DeliverResponse{Outcome: resp.Outcome, Stage: resp.Stage})
	return string(b)
}

// unbindDelivery returns the rows to dispatched after the hub's call proved
// nothing was pasted, on a context detached from the request.
func (s *Server) unbindDelivery(r *http.Request, req *agentcontrol.DeliverRequest) {
	ctx, cancel := finalizationContext(r.Context())
	defer cancel()
	if err := s.store.UnbindMessageDelivery(ctx, req.DeliveryID, req.MessageIDs); err != nil {
		s.messageLog.Error("message delivery: unbind failed", "delivery_id", req.DeliveryID, "error", err)
	}
}

// finishDelivery records the delivery's final state on the rows.
func (s *Server) finishDelivery(r *http.Request, req *agentcontrol.DeliverRequest, state, outcome string) {
	ctx, cancel := finalizationContext(r.Context())
	defer cancel()
	if _, err := s.store.FinishMessageDelivery(ctx, req.DeliveryID, req.MessageIDs, state, outcome); err != nil {
		s.messageLog.Error("message delivery: recording the outcome failed", "delivery_id", req.DeliveryID, "error", err)
	}
}
