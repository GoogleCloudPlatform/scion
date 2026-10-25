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
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// startupWarnings runs validateBrokerAuthStartup for a loopback server
// built from cfg and returns what it logged.
func startupWarnings(t *testing.T, cfg ServerConfig) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	cfg.Host = "127.0.0.1"
	cfg.BrokerID = "broker-1"
	cfg.HeartbeatEnabled = false
	cfg.ControlChannelEnabled = false
	cfg.StateDir = t.TempDir()
	srv := New(cfg, &mockManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }}, config.RuntimeBuildRecord{})

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	if err := srv.validateBrokerAuthStartup(); err != nil {
		t.Fatalf("validateBrokerAuthStartup: %v", err)
	}
	return logs.String()
}

const corsWildcardWarningText = "Runtime Broker CORS allows every origin"

func TestStartupWarning_CORSWildcard(t *testing.T) {
	for _, tc := range []struct {
		name     string
		enabled  bool
		origins  []string
		hub      bool
		wantWarn bool
	}{
		{name: "explicit wildcard", enabled: true, origins: []string{"*"}, wantWarn: true},
		{name: "explicit wildcard, pending registration", enabled: true, origins: []string{"*"}, hub: true, wantWarn: true},
		{name: "listed origin", enabled: true, origins: []string{"https://ui.example.test"}},
		{name: "wildcard listed but CORS off", enabled: false, origins: []string{"*"}},
		{name: "defaults", enabled: false, origins: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultServerConfig()
			cfg.CORSEnabled = tc.enabled
			cfg.CORSAllowedOrigins = tc.origins
			cfg.HubEnabled = tc.hub
			logs := startupWarnings(t, cfg)
			if got := strings.Contains(logs, corsWildcardWarningText); got != tc.wantWarn {
				t.Errorf("wildcard warning logged = %v, want %v; logs:\n%s", got, tc.wantWarn, logs)
			}
		})
	}
}

// A loopback broker with auth enabled and no keys says that only health
// endpoints are served, not that it runs without strict auth.
func TestStartupWarning_NoKeysSaysHealthOnly(t *testing.T) {
	logs := startupWarnings(t, DefaultServerConfig())
	if !strings.Contains(logs, "only health endpoints are served") {
		t.Errorf("no health-only warning for a broker without keys; logs:\n%s", logs)
	}
	if strings.Contains(logs, "running without strict broker auth") {
		t.Errorf("broker without keys reported as running without strict auth; logs:\n%s", logs)
	}
}

// With broker auth disabled (code-only), the loopback warning still says
// the broker runs without strict auth.
func TestStartupWarning_AuthDisabledSaysNoStrictAuth(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.BrokerAuthEnabled = false
	logs := startupWarnings(t, cfg)
	if !strings.Contains(logs, "running without strict broker auth") {
		t.Errorf("no loopback warning for a broker with auth disabled; logs:\n%s", logs)
	}
}
