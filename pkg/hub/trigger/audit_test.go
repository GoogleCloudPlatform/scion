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

package trigger

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSlogAuditSink_RecordTemplateRefusal(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	sink := NewSlogAuditSink(SlogAuditConfig{Logger: logger})

	ctx := logging.ContextWithRequestMeta(context.Background(), &logging.RequestMeta{RequestID: "req-42"})
	sink.RecordTemplateRefusal(ctx, TemplateRefusalAuditRecord{
		ProjectID: "proj-1",
		TriggerID: "trig-1",
		EventID:   "ev-1",
		FireID:    "fire-1",
		Slot:      "name",
		Path:      "event.payload.branch",
		Reason:    "template value rejected",
	})

	var entry map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &entry))
	assert.Equal(t, "WARN", entry["level"])
	assert.Equal(t, AuditFireTemplateRefused, entry["msg"])
	assert.Equal(t, AuditFireTemplateRefused, entry["event_type"])
	assert.Equal(t, "proj-1", entry["project_id"])
	assert.Equal(t, "trig-1", entry["trigger_id"])
	assert.Equal(t, "ev-1", entry["event_id"])
	assert.Equal(t, "fire-1", entry["fire_id"])
	assert.Equal(t, "name", entry["slot"])
	assert.Equal(t, "event.payload.branch", entry["path"])
	assert.Equal(t, "template value rejected", entry["reason"])
	assert.Equal(t, "req-42", entry[logging.AttrRequestID])
	// Nil receiver is a safe no-op.
	var nilSink *SlogAuditSink
	nilSink.RecordTemplateRefusal(ctx, TemplateRefusalAuditRecord{Slot: "task"})
}
