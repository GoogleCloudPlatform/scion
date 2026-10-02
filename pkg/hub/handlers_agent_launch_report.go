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
	"net/http"
	"time"
	"unicode"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// AgentLaunchReport is the broker->Hub launch report wire type (design
// §3.2), POSTed to
// /api/v1/runtime-brokers/{brokerId}/agents/{agentId}/launch. `claim` and
// `checkpoint` are sent synchronously and gate the launch (§3.8.2); `progress`
// covers step changes and the keepalive. Agent is present only on a
// `succeeded` report; only the subset of it store.LaunchReport documents
// (Runtime) is applied here — the full echo (exposed ports, applied config,
// etc.) is not, since no broker sends this report yet.
type AgentLaunchReport struct {
	LaunchID   string           `json:"launchId"`
	InstanceID string           `json:"instanceId"`
	Seq        int64            `json:"seq"`
	State      string           `json:"state"` // claim | checkpoint | progress | succeeded | failed
	Phase      string           `json:"phase,omitempty"`
	Step       string           `json:"step,omitempty"`
	Message    string           `json:"message,omitempty"`
	ErrorCode  string           `json:"errorCode,omitempty"`
	Agent      *RemoteAgentInfo `json:"agent,omitempty"` // succeeded only
	At         time.Time        `json:"at,omitzero"`
}

// agentLaunchReportAppliedResponse is the 200 body (design §3.2).
type agentLaunchReportAppliedResponse struct {
	Result string `json:"result"` // applied | duplicate | completed
}

// agentLaunchReportErrorResponse is the 404/409 body (design §3.2). A 403
// uses the standard API error body, not this shape.
type agentLaunchReportErrorResponse struct {
	Code   string `json:"code"`
	Reason string `json:"reason,omitempty"`
}

// maxInstanceIDBytes and maxPhaseBytes bound the two identifier fields that
// are matched exactly downstream (InstanceID against the stored launch_owner,
// ApplyLaunchReport in pkg/store/entadapter; Phase against a fixed ordinal
// set) rather than stored or displayed as free text. Unlike Step/Message/
// ErrorCode, these must not be rewritten or truncated: silently normalizing
// them could make two distinct instance IDs collapse to the same owner, or
// turn a garbled phase into a valid one. A report with either field too long
// or containing a control character is rejected outright.
const (
	maxInstanceIDBytes = 256
	maxPhaseBytes      = 64
)

// hasControlCharacter reports whether s contains any Unicode control
// character (Cc, including all ASCII control codes).
func hasControlCharacter(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// handleAgentLaunchReport implements the broker->Hub launch report endpoint
// (design §3.2, §3.7). Authenticated by the broker's HMAC identity only,
// routed next to message-failures.
func (s *Server) handleAgentLaunchReport(w http.ResponseWriter, r *http.Request, brokerID, agentID string) {
	ctx := r.Context()

	brokerIdent := GetBrokerIdentityFromContext(ctx)
	if brokerIdent == nil || brokerIdent.BrokerID() != brokerID {
		logAuthzDenial(r, GetIdentityFromContext(ctx), Resource{Type: "runtime_broker", ID: brokerID}, ActionUpdate,
			"a launch report may only be sent by the broker itself")
		Forbidden(w)
		return
	}

	// Bound the request body like the sibling message-failures endpoint's
	// stored/echoed string does, so an authenticated-but-misbehaving broker
	// cannot make the Hub buffer an unbounded JSON body.
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)

	var req AgentLaunchReport
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// InstanceID and Phase are identifiers matched exactly downstream (see
	// maxInstanceIDBytes/maxPhaseBytes above), like LaunchID and State: bad
	// input is rejected, not silently rewritten.
	if len(req.InstanceID) > maxInstanceIDBytes || hasControlCharacter(req.InstanceID) {
		BadRequest(w, "instanceId is too long or contains a control character")
		return
	}
	if len(req.Phase) > maxPhaseBytes || hasControlCharacter(req.Phase) {
		BadRequest(w, "phase is too long or contains a control character")
		return
	}

	// sanitizeFailureReason (message_delivery_failures.go) is reused for the
	// remaining broker-supplied free-text fields, which are stored and (for
	// Message) republished over SSE: the request body's only other bound is
	// the 64 KiB MaxBytesReader above, which would let any one of these
	// through at up to that full size otherwise.
	sr := store.LaunchReport{
		LaunchID:   req.LaunchID,
		InstanceID: req.InstanceID,
		Seq:        req.Seq,
		State:      req.State,
		Phase:      req.Phase,
		Step:       sanitizeFailureReason(req.Step),
		Message:    sanitizeFailureReason(req.Message),
		ErrorCode:  sanitizeFailureReason(req.ErrorCode),
	}
	if req.Agent != nil {
		sr.Runtime = req.Agent.Runtime
	}

	answer, updated, err := s.store.ApplyLaunchReport(ctx, agentID, brokerID, sr)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// A "failed" report the store actually applied (HTTPStatus==0, not a
	// conflict/stale-launch rejection, and Result==Applied rather than
	// Completed) means this launch ended in a broker-confirmed failure before
	// ever reaching running — see ApplyLaunchReportPreRunning/
	// applyLaunchReportActive's Failed branches and the timed_out/lost+
	// phase=error refine case in entadapter/launch_report.go. Revoke the
	// credential the Hub minted for this create (ptone/scion#1956). This
	// condition is deliberately narrower than "State==failed": a stray
	// failed report once the agent is already Running returns
	// Result=Completed (the agent did start; nothing to revoke), and a
	// failed report arriving during a stop/suspend returns a Conflict
	// HTTPStatus — in both of those cases the agent may still be relying on
	// its current credential.
	if sr.State == store.LaunchReportStateFailed && answer.HTTPStatus == 0 && answer.Result == store.LaunchReportResultApplied {
		revokeAgentCredentialsBestEffort(ctx, s.store, agentID, agentCredentialRevokeReasonCreateFailed)
	}

	if answer.HTTPStatus != 0 {
		if answer.HTTPStatus == http.StatusForbidden {
			Forbidden(w)
		} else {
			writeJSON(w, answer.HTTPStatus, agentLaunchReportErrorResponse{Code: answer.Code, Reason: answer.Reason})
		}
	} else {
		writeJSON(w, http.StatusOK, agentLaunchReportAppliedResponse{Result: answer.Result})
	}

	// design §3.7 step 7: publish after every applied change to step, phase,
	// message or launch state. Keepalive-only writes (Changed == false) are
	// not published.
	if answer.Changed {
		s.events.PublishAgentStatus(ctx, &updated)
	}
}
