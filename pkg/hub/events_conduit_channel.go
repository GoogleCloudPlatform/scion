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
	"strings"
)

// The conduit channel (design §3.9 "Conduit channel"). A separate relay
// LISTENs only on pgConduitChannel, never on the global channel. The hub
// publishes there, in the same statement as the global notice (so both are
// delivered on commit or neither is), every subject a relay acts on: the
// revocation subjects that trigger a re-check and the settings change
// subject. A conduit notice carries the subject and identifiers only: no
// entity body, credential or token.

// pgConduitChannel is the dedicated conduit channel.
const pgConduitChannel = "scion_ev_conduit"

// conduitChannelSubjects are the subject patterns published on the conduit
// channel: what a separate relay acts on.
func conduitChannelSubjects() []string {
	return append(append(append([]string(nil), conduitAuthzEventPatterns...), conduitBrokerChangedSubject), settingsUpdatedSubject)
}

// conduitNotice is a conduit channel payload.
type conduitNotice struct {
	Subject   string `json:"subject"`
	AgentID   string `json:"agentId,omitempty"`
	ProjectID string `json:"projectId,omitempty"`
	UserID    string `json:"userId,omitempty"`
	BrokerID  string `json:"brokerId,omitempty"`
	Section   string `json:"section,omitempty"`
}

// conduitNoticeFor returns the conduit channel payload of an event, or ""
// when the subject is not published there. Identifiers come from the
// subject and from the conduit and settings event types only; any other
// payload contributes nothing.
func conduitNoticeFor(subject string, event interface{}) string {
	if !conduitChannelSubject(subject) {
		return ""
	}
	n := conduitNotice{Subject: subject}
	parts := strings.Split(subject, ".")
	if len(parts) == 3 {
		switch parts[0] {
		case "agent":
			n.AgentID = parts[1]
		case "project":
			n.ProjectID = parts[1]
		}
	}
	switch e := event.(type) {
	case conduitAuthzMatch:
		n.UserID, n.ProjectID, n.AgentID = e.UserID, e.ProjectID, e.AgentID
	case conduitBrokerChange:
		n.BrokerID = e.BrokerID
	case SettingsUpdatedEvent:
		n.Section = e.Section
	}
	b, err := json.Marshal(n)
	if err != nil {
		return ""
	}
	return string(b)
}

// conduitChannelSubject reports whether subject is published on the conduit
// channel.
func conduitChannelSubject(subject string) bool {
	for _, p := range conduitChannelSubjects() {
		if subjectMatchesPattern(p, subject) {
			return true
		}
	}
	return false
}
