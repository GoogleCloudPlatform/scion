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

import "strings"

// DiagnosticLogEntry extends CloudLogEntry with source classification.
type DiagnosticLogEntry struct {
	CloudLogEntry
	Source string `json:"source"` // "hub", "broker", "agent", "messages", "server"
}

// DiagnosticsLogResponse is the batch query response.
type DiagnosticsLogResponse struct {
	Entries      []DiagnosticLogEntry `json:"entries"`
	HasMore      bool                 `json:"hasMore"`
	GCPProjectID string               `json:"gcpProjectId,omitempty"`
}

// classifySource determines the source category of a log entry based on
// its log name and structured metadata. The classification rules follow
// this priority order:
//  1. logName contains "scion-messages" -> "messages"
//  2. logName contains "scion-agents"   -> "agent"
//  3. jsonPayload.subsystem starts with "hub."    -> "hub"
//  4. jsonPayload.subsystem starts with "broker." -> "broker"
//  5. labels.component = "scion-hub"    -> "hub"
//  6. labels.component = "scion-broker" -> "broker"
//  7. Fallback -> "server"
func classifySource(entry CloudLogEntry) string {
	logName := entry.LogName

	// Check log name first (highest priority)
	if strings.Contains(logName, "scion-messages") {
		return "messages"
	}
	if strings.Contains(logName, "scion-agents") {
		return "agent"
	}

	// Check subsystem in JSON payload
	if sub, ok := entry.JSONPayload["subsystem"].(string); ok {
		if strings.HasPrefix(sub, "hub.") {
			return "hub"
		}
		if strings.HasPrefix(sub, "broker.") {
			return "broker"
		}
	}

	// Check component label
	if comp, ok := entry.Labels["component"]; ok {
		if comp == "scion-hub" {
			return "hub"
		}
		if comp == "scion-broker" {
			return "broker"
		}
	}

	return "server"
}

// toDiagnosticEntry converts a CloudLogEntry to a DiagnosticLogEntry
// by classifying its source.
func toDiagnosticEntry(entry CloudLogEntry) DiagnosticLogEntry {
	return DiagnosticLogEntry{
		CloudLogEntry: entry,
		Source:        classifySource(entry),
	}
}
