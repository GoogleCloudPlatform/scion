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

//go:build unix

package commands

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/trajectory"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/wakesock"
)

func TestStartTrajectoryRunner_Gating(t *testing.T) {
	home := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	configuredClient := func() *hub.Client {
		return hub.NewClientWithConfig(srv.URL, "tok-1", "agent-1")
	}

	t.Run("unconfigured client returns nil", func(t *testing.T) {
		t.Setenv("SCION_HARNESS", "claude")
		t.Setenv(hub.EnvLaunchID, "run-1")
		if r := startTrajectoryRunner(context.Background(), home, 0, 0, func() *hub.Client { return nil }); r != nil {
			r.PreStop(context.Background())
			t.Fatalf("expected nil runner for nil hub client")
		}
	})

	t.Run("missing SCION_HARNESS returns nil", func(t *testing.T) {
		t.Setenv("SCION_HARNESS", "")
		t.Setenv(hub.EnvLaunchID, "run-1")
		if r := startTrajectoryRunner(context.Background(), home, 0, 0, configuredClient); r != nil {
			r.PreStop(context.Background())
			t.Fatalf("expected nil runner when SCION_HARNESS is unset")
		}
	})

	t.Run("unsupported SCION_HARNESS returns nil", func(t *testing.T) {
		t.Setenv("SCION_HARNESS", "gemini")
		t.Setenv(hub.EnvLaunchID, "run-1")
		if r := startTrajectoryRunner(context.Background(), home, 0, 0, configuredClient); r != nil {
			r.PreStop(context.Background())
			t.Fatalf("expected nil runner for unsupported harness")
		}
	})

	t.Run("missing run ID returns nil", func(t *testing.T) {
		t.Setenv("SCION_HARNESS", "claude")
		t.Setenv(hub.EnvLaunchID, "")
		if r := startTrajectoryRunner(context.Background(), home, 0, 0, configuredClient); r != nil {
			r.PreStop(context.Background())
			t.Fatalf("expected nil runner when no run_id is available")
		}
	})

	t.Run("valid claude config starts runner", func(t *testing.T) {
		t.Setenv("SCION_HARNESS", "claude")
		t.Setenv(hub.EnvLaunchID, "run-1")
		r := startTrajectoryRunner(context.Background(), home, 0, 0, configuredClient)
		if r == nil {
			t.Fatalf("expected non-nil runner for valid claude config")
		}
		r.PreStop(context.Background())
	})
}

func TestProcessHookData_TrajectoryWakeDispatch(t *testing.T) {
	// Short temp dir so unixgram sun_path stays under 108 bytes.
	home, err := os.MkdirTemp("", "trjw-")
	if err != nil {
		t.Fatalf("mkdirtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	t.Setenv("SCION_TELEMETRY_ENABLED", "false")

	origStdout := hookStdout
	var outBuf bytes.Buffer
	hookStdout = &outBuf
	t.Cleanup(func() { hookStdout = origStdout })

	origDialect := hookDialect
	t.Cleanup(func() { hookDialect = origDialect })

	wakes := make(chan byte, 8)
	closer, err := wakesock.Listen(trajectory.WakeSocketPath(home), wakesock.Options{}, func(b byte) {
		wakes <- b
	})
	if err != nil {
		t.Fatalf("wakesock.Listen: %v", err)
	}
	defer func() { _ = closer.Close() }()

	// 1. Claude SessionEnd -> WakeSessionEnd ('s').
	hookDialect = "claude"
	if err := processHookData([]byte(`{"hook_event_name":"SessionEnd","session_id":"s1"}`)); err != nil {
		t.Fatalf("processHookData SessionEnd: %v", err)
	}
	select {
	case got := <-wakes:
		if got != trajectory.WakeSessionEnd {
			t.Fatalf("SessionEnd wake = %q, want %q", got, trajectory.WakeSessionEnd)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for Claude SessionEnd wake")
	}

	// 2. Claude Stop -> WakeModelEnd ('m').
	hookDialect = "claude"
	if err := processHookData([]byte(`{"hook_event_name":"Stop","session_id":"s1"}`)); err != nil {
		t.Fatalf("processHookData Stop: %v", err)
	}
	select {
	case got := <-wakes:
		if got != trajectory.WakeModelEnd {
			t.Fatalf("Stop wake = %q, want %q", got, trajectory.WakeModelEnd)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for Claude Stop wake")
	}

	// 3. Non-Claude dialect (gemini) -> no wake.
	hookDialect = "gemini"
	if err := processHookData([]byte(`{"hook_event_name":"SessionEnd","session_id":"s2"}`)); err != nil {
		t.Fatalf("processHookData gemini SessionEnd: %v", err)
	}
	select {
	case got := <-wakes:
		t.Fatalf("expected no wake for gemini dialect, got %q", got)
	case <-time.After(100 * time.Millisecond):
	}
}
