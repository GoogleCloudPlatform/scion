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

package services

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

func TestManager_WorkloadIsolationStripsScionEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SCION_AGENT_ID", "agent-123")
	t.Setenv("SCION_HUB_ENDPOINT", "https://hub.example.test")
	t.Setenv("SCION_AUTH_TOKEN", "secret-agent-token")
	t.Setenv("SCION_TRANSPORT_TOKEN", "secret-transport-token")
	t.Setenv("SCION_CLI_MODE", "agent")
	t.Setenv("USER_CUSTOM_VAR", "keep-me")

	outPath := filepath.Join(home, "isolated-env.txt")
	mgr := New(2 * time.Second)
	mgr.SetWorkloadIsolation(true)

	specs := []api.ServiceSpec{
		{
			Name:    "envdump",
			Command: []string{"sh", "-c", "env > " + outPath},
			Env: map[string]string{
				"SVC_VAR":           "svc-val",
				"SCION_INJECTED_BY": "spec-should-be-stripped",
			},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := mgr.Start(ctx, specs, 0, 0, "", false); err != nil {
		t.Fatalf("Manager.Start failed: %v", err)
	}
	t.Cleanup(func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer shutCancel()
		_ = mgr.Shutdown(shutCtx)
	})

	var data []byte
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		data, err = os.ReadFile(outPath)
		if err == nil && len(data) > 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if len(data) == 0 {
		t.Fatalf("sidecar did not write env output to %s", outPath)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	seen := make(map[string]string, len(lines))
	for _, line := range lines {
		if k, v, ok := strings.Cut(line, "="); ok {
			seen[k] = v
		}
	}

	for k := range seen {
		if strings.HasPrefix(k, "SCION_") {
			t.Errorf("isolated sidecar env leaked SCION_* variable %q", k)
		}
	}
	if got := seen["USER_CUSTOM_VAR"]; got != "keep-me" {
		t.Errorf("USER_CUSTOM_VAR = %q, want %q", got, "keep-me")
	}
	if got := seen["SVC_VAR"]; got != "svc-val" {
		t.Errorf("SVC_VAR = %q, want %q", got, "svc-val")
	}
	if got := seen["HOME"]; got != home {
		t.Errorf("HOME = %q, want %q", got, home)
	}
}

func TestManager_NoWorkloadIsolationPreservesScionEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SCION_AGENT_ID", "agent-non-shell")

	outPath := filepath.Join(home, "non-isolated-env.txt")
	mgr := New(2 * time.Second)

	specs := []api.ServiceSpec{
		{
			Name:    "envdump",
			Command: []string{"sh", "-c", "env > " + outPath},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := mgr.Start(ctx, specs, 0, 0, "", false); err != nil {
		t.Fatalf("Manager.Start failed: %v", err)
	}
	t.Cleanup(func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer shutCancel()
		_ = mgr.Shutdown(shutCtx)
	})

	var data []byte
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		data, err = os.ReadFile(outPath)
		if err == nil && len(data) > 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if len(data) == 0 {
		t.Fatalf("sidecar did not write env output to %s", outPath)
	}

	if !strings.Contains(string(data), "SCION_AGENT_ID=agent-non-shell") {
		t.Errorf("non-isolated sidecar env missing SCION_AGENT_ID=agent-non-shell; got:\n%s", string(data))
	}
}
