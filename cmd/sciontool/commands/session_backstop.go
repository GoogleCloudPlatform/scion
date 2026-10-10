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

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/handlers"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
)

// RunInit calls the startup clear and the shutdown backstop through these
// seams, so a test can pin where in the startup and shutdown sequence they
// run.
var (
	runClearSessionTombstoneAtStartup = clearSessionTombstoneAtStartup
	runReportOpenSessionAtShutdown    = reportOpenSessionAtShutdown
	runWireSessionUsage               = wireSessionUsage
)

// reportOpenSessionAtShutdown is the init daemon's backstop for session
// metrics. Hook processes report a session when the harness's session-end
// event arrives; when it never does (the agent was stopped, or the harness
// has no session-end hook) the session's counts are still in the agent's
// state file. This finalizes such a session, marks it
// closed, and reports it once. It also sends any report whose sender (a
// session-end hook, or this daemon at an earlier shutdown) died before
// confirming it. The session's status comes from the
// harness's exit outcome: "error" for a crash, otherwise "completed".
//
// It is best-effort: failures are logged and dropped, and the Hub call is
// bounded by shutdownSessionReportTimeout (defined in hook.go, next to the
// hook processes' hookHubBudget). Nothing is read or changed when
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
	summaries, err := store.CloseOpenSessionAndClaimPending(errMsg)
	if err != nil {
		log.Error("Session metrics: shutdown check of %s failed, nothing reported: %v", store.Path, err)
		return
	}
	if len(summaries) == 0 {
		log.Debug("Session metrics: no open session or unsent report at shutdown")
		return
	}

	// One deadline covers every report, so the backstop's bound does not
	// grow with the number of unsent reports.
	ctx, cancel := context.WithTimeout(context.Background(), shutdownSessionReportTimeout)
	defer cancel()
	attempted := make([]string, 0, len(summaries))
	for _, summary := range summaries {
		attempted = append(attempted, summary.SessionID)
		if err := client.ReportMetrics(ctx, hub.SummaryToMetricsPayload(summary)); err != nil {
			log.Error("Session metrics: failed to report session %s at shutdown: %v", summary.SessionID, err)
			continue
		}
		log.Info("Session metrics reported to hub at shutdown for session %s (status %s, %d turns)",
			summary.SessionID, summary.Status, summary.TurnCount)
	}
	// Each send was attempted; if this fails, the reports stay pending and
	// a hook of the next run sends them again (the Hub keeps one row per
	// session).
	if err := store.CompleteReportsNoFollow(attempted...); err != nil {
		log.Error("Session metrics: cannot clear the pending reports in %s: %v", store.Path, err)
	}
}

// clearSessionTombstoneAtStartup removes the closed-session tombstone that
// reportOpenSessionAtShutdown left during the previous shutdown. It must run
// before the harness starts: the tombstone exists only to stop hooks that
// are still running during that shutdown from reporting the session again,
// and a leftover one would make the hooks ignore a resumed session that
// reuses the ID. Failures are logged; startup continues.
func clearSessionTombstoneAtStartup(agentHome string) {
	if agentHome == "" {
		return
	}
	store := handlers.NewFileSessionState(agentHome)
	cleared, err := store.ClearSessionTombstone()
	if err != nil {
		log.Error("Session metrics: cannot clear the previous shutdown's tombstone in %s: %v", store.Path, err)
		return
	}
	if cleared {
		log.Debug("Session metrics: cleared the previous shutdown's tombstone")
	}
}

// sessionUsageRecorder returns the sink that adds the telemetry pipeline's
// natively derived usage to the open session in the agent's session metrics
// state file (handlers.FileSessionState.AddUsage). For harnesses whose usage
// source is native, this is the only way model calls and tokens reach the
// session report: their hook events carry no usage. It returns nil when no
// agent home is known. Failures are logged and dropped.
func sessionUsageRecorder(agentHome string) telemetry.SessionUsageSink {
	if agentHome == "" {
		return nil
	}
	store := handlers.NewFileSessionState(agentHome)
	return func(u telemetry.SessionUsage) {
		added, err := store.AddUsage(u)
		if err != nil {
			log.Error("Session metrics: cannot add native usage to %s: %v", store.Path, err)
			return
		}
		if !added {
			log.Debug("Session metrics: no open session for native usage (%d calls), not added", u.Calls)
		}
	}
}

// sessionUsageSinkSetter is the part of *telemetry.Pipeline that
// wireSessionUsage needs; a test substitutes a fake.
type sessionUsageSinkSetter interface {
	SetSessionUsageSink(telemetry.SessionUsageSink)
}

// wireSessionUsage installs sessionUsageRecorder(agentHome) as the
// pipeline's session usage sink, so natively derived usage reaches the
// session metrics state. RunInit calls it (through runWireSessionUsage)
// once the telemetry pipeline has started, before the harness starts. A nil
// pipeline (telemetry disabled or failed to start) is a no-op.
func wireSessionUsage(p sessionUsageSinkSetter, agentHome string) {
	if p == nil {
		return
	}
	if pipeline, ok := p.(*telemetry.Pipeline); ok && pipeline == nil {
		return
	}
	p.SetSessionUsageSink(sessionUsageRecorder(agentHome))
}
