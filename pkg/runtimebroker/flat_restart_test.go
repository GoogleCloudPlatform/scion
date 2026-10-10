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
	"net/http"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Restart-handler coverage for flat Runtime Broker instances
// (.design/flat-runtime-brokers-contract.md sections 9 and 10). Restart is a
// stop leg followed by a start, so every refusal must precede the stop.

const flatRestartPath = "/api/v1/agents/test-agent-1/restart"

func mgrCalls(f *flatInstanceFixture) (stops, starts int) {
	f.mgr.mu.Lock()
	defer f.mgr.mu.Unlock()
	return f.mgr.stopCalls, f.mgr.startCalls
}

func TestFlatInstanceRestart_ExpectedTargetMismatchBeforeStopLeg(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	w := serveFlat(f.srv, http.MethodPost, flatRestartPath, `{"runId":"run-r","expectedRuntimeTargetId":"another-target"}`)
	e := expectFlatRefusal(t, w, http.StatusConflict, ErrCodeRuntimeTargetMismatch)
	if e.Details["actualRuntimeTargetId"] != f.identity.RuntimeTarget.ID || e.Details["expectedRuntimeTargetId"] != "another-target" ||
		e.Details["runtimeBrokerId"] != f.identity.RuntimeBrokerID {
		t.Fatalf("details = %v", e.Details)
	}
	if stops, starts := mgrCalls(f); stops != 0 || starts != 0 {
		t.Fatalf("a refused restart must not stop or start: stops=%d starts=%d", stops, starts)
	}
}

func TestFlatInstanceRestart_UndecodableBodyRejected(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	for _, body := range []string{`{"resolvedEnv": `, `{"expectedRuntimeTargetId": 42}`, `{"resolvedEnv": "not-a-map"}`} {
		w := serveFlat(f.srv, http.MethodPost, flatRestartPath, body)
		expectFlatRefusal(t, w, http.StatusBadRequest, ErrCodeInvalidRequest)
	}
	if stops, starts := mgrCalls(f); stops != 0 || starts != 0 {
		t.Fatalf("an undecodable restart must not stop or start: stops=%d starts=%d", stops, starts)
	}
	// Unknown keys stay ignored (start/restart version-skew contract).
	w := serveFlat(f.srv, http.MethodPost, flatRestartPath,
		`{"someFutureKey": true, "expectedRuntimeTargetId": "`+f.identity.RuntimeTarget.ID+`"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("unknown keys must be accepted: %d %s", w.Code, w.Body.String())
	}
}

func TestFlatInstanceRestart_IgnoresSavedProfile(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true, activeProfile: "batch"})
	projectDir, err := config.GetResolvedProjectDir("")
	if err != nil {
		t.Fatal(err)
	}
	writeFlatFixtureFile(t, filepath.Join(config.GetAgentHomePath(projectDir, "test-agent-1"), "agent-info.json"),
		`{"name":"test-agent-1","profile":"batch"}`)
	w := serveFlat(f.srv, http.MethodPost, flatRestartPath,
		`{"runId":"run-restart","expectedRuntimeTargetId":"`+f.identity.RuntimeTarget.ID+`"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", w.Code, w.Body.String())
	}
	f.mgr.mu.Lock()
	defer f.mgr.mu.Unlock()
	if f.mgr.startCalls != 1 {
		t.Fatalf("startCalls = %d, want 1", f.mgr.startCalls)
	}
	if f.mgr.lastStartOpts.Profile != "" {
		t.Fatalf("saved profile was used on restart: %q", f.mgr.lastStartOpts.Profile)
	}
	if f.mgr.lastStartOpts.RunID != "run-restart" {
		t.Fatalf("run ID = %q, want run-restart", f.mgr.lastStartOpts.RunID)
	}
}

// TestLegacyInstanceRestart_NonEmptyExpectedTargetRejected: a current-binary
// legacy Runtime Broker never ignores a non-empty expectedRuntimeTargetId on
// restart either.
func TestLegacyInstanceRestart_NonEmptyExpectedTargetRejected(t *testing.T) {
	srv := newTestServer(t)
	mgr := srv.manager.(*mockManager)
	w := serveFlat(srv, http.MethodPost, flatRestartPath, `{"expectedRuntimeTargetId":"some-target"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
	}
	e := decodeFlatError(t, w)
	if e.Code != ErrCodeRuntimeTargetMismatch {
		t.Fatalf("code = %q", e.Code)
	}
	if actual, ok := e.Details["actualRuntimeTargetId"]; !ok || actual != "" {
		t.Fatalf("a legacy Runtime Broker reports an empty actual target, got %v", e.Details)
	}
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if mgr.stopCalls != 0 || mgr.startCalls != 0 {
		t.Fatalf("a refused restart must not stop or start: stops=%d starts=%d", mgr.stopCalls, mgr.startCalls)
	}
}
