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

package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	gcplog "cloud.google.com/go/logging"
)

func TestNewArtifactLogger_StdoutFallbackWithoutCloud(t *testing.T) {
	t.Setenv("K_SERVICE", "")
	logger, cleanup, err := NewArtifactLogger(ArtifactLoggerConfig{Component: "test-hub", Level: slog.LevelInfo})
	if err != nil {
		t.Fatalf("NewArtifactLogger() error = %v", err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	if logger == nil {
		t.Fatal("NewArtifactLogger() returned nil logger")
	}
	rh, ok := logger.Handler().(*requestIDHandler)
	if !ok {
		t.Fatalf("handler = %T, want *requestIDHandler", logger.Handler())
	}
	if _, ok := rh.inner.(*slog.JSONHandler); !ok {
		t.Errorf("without Cloud Logging the inner handler = %T, want the stdout JSON handler", rh.inner)
	}
}

func TestRequestIDHandler_AddsRequestIDFromContext(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(&requestIDHandler{inner: slog.NewJSONHandler(&buf, nil)})
	ctx := ContextWithRequestMeta(context.Background(), &RequestMeta{RequestID: "req-123"})

	logger.InfoContext(ctx, "artifact decision", AttrArtifactID, "a1")
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got[AttrRequestID] != "req-123" {
		t.Errorf("request_id = %v, want req-123", got[AttrRequestID])
	}

	// An explicit request_id is kept, never duplicated.
	buf.Reset()
	logger.InfoContext(ctx, "artifact decision", AttrRequestID, "explicit")
	if n := bytes.Count(buf.Bytes(), []byte(`"request_id"`)); n != 1 {
		t.Errorf("request_id appears %d times, want 1: %s", n, buf.String())
	}

	// Outside a request nothing is added.
	buf.Reset()
	logger.Info("artifact decision")
	if bytes.Contains(buf.Bytes(), []byte(AttrRequestID)) {
		t.Errorf("request_id added outside a request: %s", buf.String())
	}
}

func TestArtifactCloudHandler_PromotesLabels(t *testing.T) {
	var entries []gcplog.Entry
	h := &artifactCloudHandler{CloudHandler: CloudHandler{
		level: slog.LevelDebug, component: "test-hub", hubID: "hub-1",
		logHook: func(e gcplog.Entry) { entries = append(entries, e) },
	}}
	logger := slog.New(h).With(AttrPrincipalKind, "user")
	logger.Warn("artifact refused",
		AttrArtifactID, "a1", AttrProjectID, "p1", AttrArtifactAction, "grant_put",
		AttrArtifactResult, "refused", AttrArtifactReason, "not_administrator",
		AttrPrincipalID, "u1", AttrRequestID, "req-1", "subject_ref", "user:u2")
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.Severity != gcplog.Warning {
		t.Errorf("severity = %v, want Warning", e.Severity)
	}
	want := map[string]string{
		AttrArtifactID: "a1", AttrProjectID: "p1", AttrArtifactAction: "grant_put",
		AttrArtifactResult: "refused", AttrArtifactReason: "not_administrator",
		AttrPrincipalKind: "user", AttrPrincipalID: "u1", "hub_id": "hub-1", "component": "test-hub",
	}
	for k, v := range want {
		if e.Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, e.Labels[k], v)
		}
	}
	for _, k := range []string{AttrRequestID, "subject_ref"} {
		if _, ok := e.Labels[k]; ok {
			t.Errorf("label %s promoted; it stays in the payload only", k)
		}
	}
	payload := e.Payload.(map[string]any)
	if payload["subject_ref"] != "user:u2" || payload[AttrRequestID] != "req-1" || payload[AttrPrincipalKind] != "user" {
		t.Errorf("payload missing fields: %v", payload)
	}
}

func TestArtifactCloudHandler_WithGroupKeepsType(t *testing.T) {
	h := &artifactCloudHandler{CloudHandler: CloudHandler{component: "c", hubID: "h", logHook: func(gcplog.Entry) {}}}
	if _, ok := h.WithGroup("g").(*artifactCloudHandler); !ok {
		t.Error("WithGroup should return an *artifactCloudHandler")
	}
	if _, ok := h.WithAttrs([]slog.Attr{slog.String("k", "v")}).(*artifactCloudHandler); !ok {
		t.Error("WithAttrs should return an *artifactCloudHandler")
	}
}
