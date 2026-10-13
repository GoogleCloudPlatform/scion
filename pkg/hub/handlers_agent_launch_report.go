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
// `succeeded` report; of its fields the hub applies the broker-resolved
// config (template, harness config and auth, image, profile, runtime: see
// store.ApplyBrokerAgentConfig), WorkspacePlacement and RunID (see
// handleAgentLaunchReport). The rest of the echo (phase, activity, container
// status, etc.) is not applied.
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
		// The broker-resolved config is applied in the store's launch
		// transaction with the same rules (and the same conversion) as a
		// synchronous dispatch (ptone/scion#4818), so a report the launch
		// fencing rejects writes none of it.
		sr.AgentConfig = brokerAgentConfigOf(req.Agent)
	}

	answer, updated, err := s.store.ApplyLaunchReport(ctx, agentID, brokerID, sr)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// A succeeded report carries the launch's start echo: record where it
	// placed the agent's workspace (narrow write, see
	// recordWorkspacePlacement), only when the store applied the report.
	if sr.State == store.LaunchReportStateSucceeded && answer.HTTPStatus == 0 && answer.Result == store.LaunchReportResultApplied &&
		req.Agent != nil && validWorkspacePlacementReport(req.Agent.WorkspacePlacement) {
		if err := s.store.SetAgentWorkspacePlacement(ctx, agentID, req.Agent.WorkspacePlacement); err != nil {
			s.agentLifecycleLog.Warn("launch report: failed to record the agent's workspace placement",
				"agent_id", agentID, "error", err)
		} else {
			updated.WorkspacePlacement = req.Agent.WorkspacePlacement
		}
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

	// An applied succeeded report settles the run the launch started
	// (ptone/scion#3176), as a synchronous dispatch that lands does
	// (adoptBrokerRunID): the broker's Start removed every earlier entry of
	// the name that was not running, so the previous runs are cleared. A
	// start that found the agent running keeps that entry and reports its
	// run, which the hub then adopts (ptone/scion#2965), so a later delete
	// names the run the runtime holds. A broker that reports no run ID gets
	// no clear; a later delete then repeats the previous runs as run-scoped
	// 404s. Only an applied report settles: a completed answer (a repeated
	// terminal report, or the hub had already resolved the launch) is
	// deliberately not settled; the conservative cost is extra 404s on a
	// later delete.
	if sr.State == store.LaunchReportStateSucceeded && answer.HTTPStatus == 0 && answer.Result == store.LaunchReportResultApplied {
		s.settleLaunchedRun(ctx, &updated, req.Agent)
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

// settleLaunchedRun settles the run of a launch whose succeeded report the
// store applied (see the caller). row is the agent as that report's
// transaction left it, so row.RunID is the run the hub recorded for the
// launch when the report applied. The write is one guarded swap from
// row.RunID to the run the broker reports (store.AdoptAgentRunID), which
// also clears the previous runs:
//   - the same run: a same-value swap that only clears the previous runs;
//   - another run: the broker kept a running entry of the name and reports
//     its run (agent.AgentManager.Start returns a running entry when the
//     start has no task), and the hub adopts it (ptone/scion#2965).
//
// The swap is checked in the same statement as the write. It never
// overwrites a run recorded after the report applied (keyed on row.RunID,
// and on row.RunIDUpdatedAt, so a run that changed and changed back is not
// matched either), and it writes nothing while the row is soft-deleted or a
// delete holds it, so a delete's claim snapshot never misses the settled
// run. A row with no RunIDUpdatedAt (its run has not changed since the
// column was added, so no dispatch recorded this launch's run) and a report
// with no run ID change nothing. Best-effort: a failed write is logged and
// leaves the row as it was, which costs a later delete only a run-scoped
// refusal or extra 404s. On a swap, row's RunID and PreviousRunIDs are
// updated to match.
func (s *Server) settleLaunchedRun(ctx context.Context, row *store.Agent, info *RemoteAgentInfo) {
	if info == nil || info.RunID == "" {
		return
	}
	if row.RunIDUpdatedAt == nil {
		s.agentLifecycleLog.Debug("Launch report: the agent's run has no recorded change time; not settling the launched run",
			"agent_id", row.ID, "run_id", row.RunID, "broker_run_id", info.RunID)
		return
	}
	swapped, err := s.store.AdoptAgentRunID(ctx, row.ID, row.RunID, info.RunID, *row.RunIDUpdatedAt)
	if err != nil {
		s.agentLifecycleLog.Warn("Launch report: failed to settle the launched run",
			"agent_id", row.ID, "run_id", row.RunID, "broker_run_id", info.RunID, "error", err)
		return
	}
	if !swapped {
		s.agentLifecycleLog.Debug("Launch report: another run is recorded or a delete holds the agent; not settling the launched run",
			"agent_id", row.ID, "run_id", row.RunID, "broker_run_id", info.RunID)
		return
	}
	minted := row.RunID
	row.RunID = info.RunID
	row.PreviousRunIDs = nil
	if info.RunID != minted {
		s.agentLifecycleLog.Info("Launch report: adopted the broker's run ID",
			"agent_id", row.ID, "agent", row.Slug, "minted_run_id", minted, "run_id", info.RunID)
	}
}
