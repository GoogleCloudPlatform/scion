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

package opsettings

import (
	"encoding/json"
	"testing"
)

func TestParseChatRetentionDoc(t *testing.T) {
	def := DefaultChatRetentionConfig()
	tests := []struct {
		name    string
		doc     string
		want    ChatRetentionConfig
		wantErr bool
	}{
		{name: "empty", doc: `{}`, want: def},
		{name: "enabled no window", doc: `{"enabled":true}`, want: func() ChatRetentionConfig { c := def; c.Enabled = true; return c }()},
		{name: "window", doc: `{"enabled":true,"messages_retention_days":30}`,
			want: func() ChatRetentionConfig { c := def; c.Enabled = true; c.MessagesRetentionDays = 30; return c }()},
		{name: "minimum window", doc: `{"messages_retention_days":7}`,
			want: func() ChatRetentionConfig { c := def; c.MessagesRetentionDays = 7; return c }()},
		{name: "window below minimum", doc: `{"messages_retention_days":6}`, wantErr: true},
		{name: "zero window", doc: `{"messages_retention_days":0}`, wantErr: true},
		{name: "sweep tuning", doc: `{"sweep":{"chunk_size":25,"pause_ms":0,"tick_budget_seconds":5,"tick_interval_minutes":1}}`,
			want: ChatRetentionConfig{ChunkSize: 25, PauseMS: 0, TickBudgetSeconds: 5, TickIntervalMinutes: 1}},
		{name: "partial sweep", doc: `{"sweep":{"chunk_size":1000}}`,
			want: func() ChatRetentionConfig { c := def; c.ChunkSize = 1000; return c }()},
		{name: "chunk below floor", doc: `{"sweep":{"chunk_size":24}}`, wantErr: true},
		{name: "chunk above cap", doc: `{"sweep":{"chunk_size":1001}}`, wantErr: true},
		{name: "negative pause", doc: `{"sweep":{"pause_ms":-1}}`, wantErr: true},
		{name: "zero budget", doc: `{"sweep":{"tick_budget_seconds":0}}`, wantErr: true},
		{name: "largest budget and pause", doc: `{"sweep":{"tick_budget_seconds":40,"pause_ms":10000}}`,
			want: func() ChatRetentionConfig { c := def; c.TickBudgetSeconds = 40; c.PauseMS = 10000; return c }()},
		{name: "budget over the scheduler limit", doc: `{"sweep":{"tick_budget_seconds":41}}`, wantErr: true},
		{name: "pause over the limit", doc: `{"sweep":{"pause_ms":10001}}`, wantErr: true},
		{name: "zero interval", doc: `{"sweep":{"tick_interval_minutes":0}}`, wantErr: true},
		{name: "unknown field", doc: `{"deleted_conversation_grace_days":30}`, wantErr: true},
		{name: "unknown sweep field", doc: `{"sweep":{"foo":1}}`, wantErr: true},
		{name: "malformed", doc: `{`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseChatRetentionDoc(json.RawMessage(tt.doc))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseChatRetentionDoc(%s) = %+v, want error", tt.doc, got)
				}
				// The schema must refuse every document Resolve refuses
				// (malformed JSON is refused before validation).
				if tt.name != "malformed" {
					if errs := Validate(ChatRetentionSectionName, json.RawMessage(tt.doc)); len(errs) == 0 {
						t.Errorf("Validate(%s) accepted a document Resolve refuses", tt.doc)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseChatRetentionDoc(%s): %v", tt.doc, err)
			}
			if got != tt.want {
				t.Errorf("ParseChatRetentionDoc(%s) = %+v, want %+v", tt.doc, got, tt.want)
			}
			if errs := Validate(ChatRetentionSectionName, json.RawMessage(tt.doc)); len(errs) != 0 {
				t.Errorf("Validate(%s) = %v, want none", tt.doc, errs)
			}
		})
	}
}

// Settings round-trips through ParseChatRetentionDoc, and a config without a
// window stores no messages_retention_days.
func TestChatRetentionConfigSettingsRoundTrip(t *testing.T) {
	for _, c := range []ChatRetentionConfig{
		DefaultChatRetentionConfig(),
		{Enabled: true, MessagesRetentionDays: 30, ChunkSize: 100, PauseMS: 10, TickBudgetSeconds: 5, TickIntervalMinutes: 2},
	} {
		doc, err := json.Marshal(c.Settings())
		if err != nil {
			t.Fatal(err)
		}
		got, err := ParseChatRetentionDoc(doc)
		if err != nil {
			t.Fatalf("parse %s: %v", doc, err)
		}
		if got != c {
			t.Errorf("round trip of %+v = %+v (doc %s)", c, got, doc)
		}
		if !c.HasWindow() {
			var m map[string]any
			_ = json.Unmarshal(doc, &m)
			if _, ok := m["messages_retention_days"]; ok {
				t.Errorf("doc %s stores messages_retention_days without a window", doc)
			}
		}
	}
}
