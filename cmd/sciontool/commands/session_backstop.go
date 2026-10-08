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

package commands

import (
	"context"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/handlers"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// shutdownSessionReportTimeout bounds the Hub call that reports a still-open
// session at shutdown. Together with the state lock wait (2s) it keeps the
// backstop well inside the runtime's default 10s stop grace period.
var shutdownSessionReportTimeout = 3 * time.Second

// reportOpenSessionAtShutdown is the init daemon's backstop for session
// metrics. Hook processes report a session when the harness's session-end
// event arrives; when it never does (the agent was stopped and the harness
// killed, or the harness has no session-end hook) the session's counts are
// still in the agent's state file. This finalizes such a session, marks it
// closed, and reports it once. The session's status comes from the
// harness's exit outcome: "error" for a crash, otherwise "completed".
//
// It is best-effort: failures are logged and dropped, and the Hub call is
// bounded by shutdownSessionReportTimeout. Nothing is read or changed when
// the Hub is not configured.
func reportOpenSessionAtShutdown(agentHome string, outcome exitOutcome, newClient func() *hub.Client) {
	if agentHome == "" {
		return
	}
	client := newClient()
	if client == nil || !client.IsConfigured() {
		return
	}

	errMsg := ""
	if outcome.isCrash {
		errMsg = outcome.message
	}
	store := handlers.NewFileSessionState(agentHome)
	summary, ok, err := store.CloseOpenSession(errMsg)
	if err != nil {
		log.Error("Session metrics: shutdown check of %s failed, nothing reported: %v", store.Path, err)
		return
	}
	if !ok {
		log.Debug("Session metrics: no open session at shutdown")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), shutdownSessionReportTimeout)
	defer cancel()
	if err := client.ReportMetrics(ctx, hub.SummaryToMetricsPayload(summary)); err != nil {
		log.Error("Session metrics: failed to report open session %s at shutdown: %v", summary.SessionID, err)
		return
	}
	log.Info("Session metrics reported to hub at shutdown for session %s (status %s, %d turns)",
		summary.SessionID, summary.Status, summary.TurnCount)
}
