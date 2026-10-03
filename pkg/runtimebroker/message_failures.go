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
	"context"
	"log/slog"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// messageFailureReportTimeout bounds each report call to a hub.
const messageFailureReportTimeout = 10 * time.Second

// reportMessageFailure tells the hub that a message it believes was
// dispatched was never delivered (#1820). The report goes to the hub
// connection the message arrived on (X-Scion-Hub-Connection, set by the
// control channel). When that is unknown — for example a direct HTTP
// dispatch — it goes to every connection with a hub client. Each hub
// ignores message IDs it does not own, and checks that the recipient agent
// is assigned to the reporting broker.
func (s *Server) reportMessageFailure(connName string, failure hubclient.MessageFailure) {
	var targets []*HubConnection
	s.hubMu.RLock()
	if conn, ok := s.hubConnections[connName]; connName != "" && ok && conn.HubClient != nil {
		targets = append(targets, conn)
	} else {
		for _, conn := range s.hubConnections {
			if conn.HubClient != nil {
				targets = append(targets, conn)
			}
		}
	}
	s.hubMu.RUnlock()

	if len(targets) == 0 {
		s.messageLogger().Warn("buffered message delivery failed; no hub connection to report it to",
			"message_id", failure.MessageID, "agent_id", failure.AgentID, "reason", failure.Reason)
		return
	}

	report := &hubclient.MessageFailuresReport{Failures: []hubclient.MessageFailure{failure}}
	for _, conn := range targets {
		ctx, cancel := context.WithTimeout(context.Background(), messageFailureReportTimeout)
		err := conn.HubClient.RuntimeBrokers().ReportMessageFailures(ctx, conn.BrokerID, report)
		cancel()
		if err != nil {
			s.messageLogger().Warn("failed to report buffered message delivery failure to hub",
				"connection", conn.Name, "message_id", failure.MessageID,
				"agent_id", failure.AgentID, "error", err)
			continue
		}
		s.messageLogger().Info("reported buffered message delivery failure to hub",
			"connection", conn.Name, "message_id", failure.MessageID, "agent_id", failure.AgentID)
	}
}

// messageLogger returns the dedicated message log when configured, falling
// back to the general message logger.
func (s *Server) messageLogger() *slog.Logger {
	if s.dedicatedMessageLog != nil {
		return s.dedicatedMessageLog
	}
	return s.messageLog
}
