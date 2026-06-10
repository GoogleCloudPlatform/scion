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

// Package projectcompat centralizes bounded compatibility for legacy grove
// strings. New code should use project terminology and call these helpers at
// explicit adapter points when old clients, config, labels, topics, or routes
// may still provide grove names.
package projectcompat

import (
	"fmt"
	"strings"
)

const (
	CanonicalTopicPrefix = "scion.project"
	LegacyTopicPrefix    = "scion.grove"

	LabelProjectID   = "scion.project_id"
	LabelGroveID     = "scion.grove_id"
	LabelProject     = "scion.project"
	LabelGrove       = "scion.grove"
	LabelProjectPath = "scion.project_path"
	LabelGrovePath   = "scion.grove_path"
)

type TopicKind string

const (
	TopicKindAgent     TopicKind = "agent"
	TopicKindUser      TopicKind = "user"
	TopicKindBroadcast TopicKind = "broadcast"
)

type Topic struct {
	ProjectID string
	Kind      TopicKind
	Actor     string
	Legacy    bool
}

func AgentTopic(projectID, agentSlug string) string {
	return CanonicalTopicPrefix + "." + projectID + ".agent." + agentSlug + ".messages"
}

func UserTopic(projectID, userID string) string {
	return CanonicalTopicPrefix + "." + projectID + ".user." + userID + ".messages"
}

func BroadcastTopic(projectID string) string {
	return CanonicalTopicPrefix + "." + projectID + ".broadcast"
}

func AllAgentTopic(projectID string) string {
	return CanonicalTopicPrefix + "." + projectID + ".agent.*.messages"
}

func AllUserTopic(projectID string) string {
	return CanonicalTopicPrefix + "." + projectID + ".user.*.messages"
}

func ProjectPattern(projectID string) string {
	return CanonicalTopicPrefix + "." + projectID + ".>"
}

func AllProjectsPattern() string {
	return CanonicalTopicPrefix + ".>"
}

func LegacyUserTopic(projectID, userID string) string {
	return LegacyTopicPrefix + "." + projectID + ".user." + userID + ".messages"
}

func ParseTopic(topic string) (Topic, error) {
	parts := strings.Split(topic, ".")
	if len(parts) < 4 || parts[0] != "scion" {
		return Topic{}, fmt.Errorf("malformed topic %q", topic)
	}

	var legacy bool
	switch parts[1] {
	case "project":
	case "grove":
		legacy = true
	default:
		return Topic{}, fmt.Errorf("expected project or legacy grove topic, got %q", topic)
	}

	t := Topic{ProjectID: parts[2], Legacy: legacy}
	if t.ProjectID == "" {
		return Topic{}, fmt.Errorf("missing project id in topic %q", topic)
	}

	switch parts[3] {
	case "agent":
		if len(parts) != 6 || parts[5] != "messages" || parts[4] == "" {
			return Topic{}, fmt.Errorf("expected %s.<projectId>.agent.<agentSlug>.messages", CanonicalTopicPrefix)
		}
		t.Kind = TopicKindAgent
		t.Actor = parts[4]
	case "user":
		if len(parts) != 6 || parts[5] != "messages" || parts[4] == "" {
			return Topic{}, fmt.Errorf("expected %s.<projectId>.user.<userId>.messages", CanonicalTopicPrefix)
		}
		t.Kind = TopicKindUser
		t.Actor = parts[4]
	case "broadcast":
		if len(parts) != 4 {
			return Topic{}, fmt.Errorf("expected %s.<projectId>.broadcast", CanonicalTopicPrefix)
		}
		t.Kind = TopicKindBroadcast
	default:
		return Topic{}, fmt.Errorf("unknown topic kind %q", parts[3])
	}
	return t, nil
}
