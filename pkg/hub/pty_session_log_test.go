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
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPTYSessionLog_CarriesRoutedBrokerID pins that the "PTY session
// started"/"ended" lines carry the broker the stream is actually routed to
// under its own field, distinct from the process-wide "broker_id" attr a
// combo-mode server attaches to every log line (which names the locally
// co-located broker, not necessarily the one enforcing this attach). Without
// this field, an attach refusal in the log points at the wrong broker.
func TestPTYSessionLog_CarriesRoutedBrokerID(t *testing.T) {
	buf := captureAuditLogs(t)

	logPTYSessionStarted("agent-1", "some-agent", "routed-broker-7", "user-9")
	logPTYSessionEnded("agent-1", "some-agent", "routed-broker-7", 4501, "attach_unsupported")

	started := auditRecordWithMsg(t, buf, "PTY session started")
	require.NotNil(t, started, "expected a \"PTY session started\" log line")
	require.Equal(t, "routed-broker-7", started["routed_broker_id"])

	ended := auditRecordWithMsg(t, buf, "PTY session ended")
	require.NotNil(t, ended, "expected a \"PTY session ended\" log line")
	require.Equal(t, "routed-broker-7", ended["routed_broker_id"])
	require.EqualValues(t, 4501, ended["close_code"])
	require.Equal(t, "attach_unsupported", ended["close_reason"])
}
