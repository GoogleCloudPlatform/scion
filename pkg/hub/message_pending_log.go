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
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Creation paths recorded on the "message entered pending" log line
// (ptone/scion#4827). Each constant names one hub call site that persists a
// message row in dispatch_state "pending", so a row reported by the
// stuck-pending sweep can be traced back to the code that wrote it. The
// store-level default (a writer that leaves DispatchState empty) is logged
// separately by the store, with creation path store.MessagePendingPathStoreDefault.
const (
	// pendingPathAgentDM is ExecuteAgentDM step 8: the row is persisted
	// "pending" as its own dispatch intent and moves to dispatched or failed
	// once the dispatch outcome is known (#1689).
	pendingPathAgentDM = "agent_dm"
)

// logMessageEnteredPending records that msg was persisted in dispatch_state
// "pending" by the call site named by path. It must be called after the row
// is persisted, so msg.DispatchState holds the stored state. It logs only
// identifiers and routing fields, with the type mapped through
// messages.LogType: never the body, metadata or attachments.
// A row in any other state is not logged.
func (s *Server) logMessageEnteredPending(path string, msg *store.Message) {
	if msg == nil || msg.DispatchState != store.MessageDispatchPending {
		return
	}
	log := s.messageLog
	if log == nil {
		log = slog.Default()
	}
	log.Info(store.MessageEnteredPendingLogMsg,
		"message_id", msg.ID,
		"sender", msg.Sender,
		"recipient", msg.Recipient,
		"type", messages.LogType(msg.Type),
		"creation_path", path,
	)
}
