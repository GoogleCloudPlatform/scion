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
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ScheduledExecOutputResponse is the response body of
// GET /api/v1/projects/{projectId}/scheduled-events/{id}/output.
type ScheduledExecOutputResponse struct {
	CaptureMode  string `json:"captureMode"`
	OutputStatus string `json:"outputStatus,omitempty"`
	Output       any    `json:"output,omitempty"`
}

// getScheduledEventOutput handles
// GET /api/v1/projects/{projectId}/scheduled-events/{id}/output.
//
// Reading captured output requires scheduled_event.read on the event AND live
// agent.attach on the run's target agent via execRevealer (no creator
// exemption, SEC-P5). This file is the sole allow-listed caller of
// GetScheduledExecRunOutput (guarded by TestExecOutputReaders).
func (s *Server) getScheduledEventOutput(w http.ResponseWriter, r *http.Request, projectID, eventID string) {
	evt, ok := s.loadScheduledEventForProject(w, r, projectID, eventID)
	if !ok {
		return
	}
	if !s.authorizeScheduledEventRow(w, r, projectID, ActionRead, scheduledEventRowOf(evt)) {
		return
	}
	if evt.EventType != scheduledEventTypeExec {
		NotFound(w, "Scheduled event output")
		return
	}
	run, err := s.store.GetScheduledExecRun(r.Context(), eventID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Scheduled event output")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}
	if run == nil || run.CaptureMode == "" {
		NotFound(w, "Scheduled event output")
		return
	}
	if !s.newExecRevealer(r.Context()).reveals(run.TargetAgentID) {
		Forbidden(w)
		return
	}

	resp := ScheduledExecOutputResponse{
		CaptureMode:  run.CaptureMode,
		OutputStatus: run.OutputStatus,
	}
	if run.OutputStatus == agentcontrol.ExecOutputCaptured || run.OutputStatus == agentcontrol.ExecOutputTruncated {
		content, err := s.store.GetScheduledExecRunOutput(r.Context(), run.EventID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			writeErrorFromErr(w, err, "")
			return
		}
		if len(content) > 0 {
			switch run.CaptureMode {
			case agentcontrol.ExecCaptureJSON:
				resp.Output = json.RawMessage(content)
			case agentcontrol.ExecCaptureTail:
				resp.Output = string(content)
			}
		}
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, resp)
}
