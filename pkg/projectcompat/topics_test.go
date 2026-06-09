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

package projectcompat

import "testing"

func TestTopicBuildersUseCanonicalProjectPrefix(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"agent", AgentTopic("p1", "coder"), "scion.project.p1.agent.coder.messages"},
		{"user", UserTopic("p1", "alice"), "scion.project.p1.user.alice.messages"},
		{"broadcast", BroadcastTopic("p1"), "scion.project.p1.broadcast"},
		{"all agents", AllAgentTopic("p1"), "scion.project.p1.agent.*.messages"},
		{"all users", AllUserTopic("p1"), "scion.project.p1.user.*.messages"},
		{"project pattern", ProjectPattern("p1"), "scion.project.p1.>"},
		{"all projects pattern", AllProjectsPattern(), "scion.project.>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Fatalf("got %q, want %q", tt.got, tt.want)
			}
		})
	}
}

func TestParseTopicAcceptsCanonicalAndLegacy(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want Topic
	}{
		{
			name: "canonical agent",
			in:   "scion.project.p1.agent.coder.messages",
			want: Topic{ProjectID: "p1", Kind: TopicKindAgent, Actor: "coder"},
		},
		{
			name: "legacy agent",
			in:   "scion.grove.p1.agent.coder.messages",
			want: Topic{ProjectID: "p1", Kind: TopicKindAgent, Actor: "coder", Legacy: true},
		},
		{
			name: "canonical user wildcard",
			in:   "scion.project.p1.user.*.messages",
			want: Topic{ProjectID: "p1", Kind: TopicKindUser, Actor: "*"},
		},
		{
			name: "legacy broadcast",
			in:   "scion.grove.p1.broadcast",
			want: Topic{ProjectID: "p1", Kind: TopicKindBroadcast, Legacy: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseTopic(tt.in)
			if err != nil {
				t.Fatalf("ParseTopic(%q) error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("ParseTopic(%q) = %#v, want %#v", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseTopicRejectsMalformedTopics(t *testing.T) {
	for _, topic := range []string{
		"",
		"scion.global.broadcast",
		"scion.project",
		"scion.project..broadcast",
		"scion.project.p1.agent.coder",
		"scion.project.p1.agent.coder.messages.extra",
		"scion.project.p1.user..messages",
		"scion.project.p1.unknown",
	} {
		t.Run(topic, func(t *testing.T) {
			if _, err := ParseTopic(topic); err == nil {
				t.Fatalf("ParseTopic(%q) succeeded, want error", topic)
			}
		})
	}
}
