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

package agent

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// stubHostNetworkBlockPolicy pins the broker block policy setting and the
// cloud-host detection result for the rest of the test, so tests that reach
// the block branch do not depend on the machine they run on.
func stubHostNetworkBlockPolicy(t *testing.T, policy string, host runtime.CloudHost) {
	t.Helper()
	t.Setenv(runtime.HostNetworkBlockPolicyEnvVar, policy)
	orig := detectCloudHost
	detectCloudHost = func() runtime.CloudHost { return host }
	t.Cleanup(func() { detectCloudHost = orig })
}

func TestHostNetworkBlockPolicy(t *testing.T) {
	const baseWarning = "metadata block mode is not enforced under host networking"
	cloud := runtime.CloudHost{Detected: true, Source: "dmi", Provider: "gce"}
	none := runtime.CloudHost{}

	tests := []struct {
		name         string
		policy       string
		host         runtime.CloudHost
		wantErr      []string // substrings of the error; nil means no error
		wantWarning  string   // exact warning when no error
		wantDetected bool     // whether detection must have been consulted
	}{
		{name: "default on cloud refuses", policy: "", host: cloud,
			wantErr: []string{"detected cloud host", "bridge-reachable hub URL", runtime.HostNetworkBlockPolicyEnvVar + "=allow"}, wantDetected: true},
		{name: "default off cloud warns", policy: "", host: none, wantWarning: baseWarning, wantDetected: true},
		{name: "auto off cloud warns", policy: "auto", host: none, wantWarning: baseWarning, wantDetected: true},
		{name: "strict refuses without detection", policy: "strict", host: none,
			wantErr: []string{runtime.HostNetworkBlockPolicyEnvVar + " refuses", "bridge-reachable hub URL"}},
		{name: "allow on cloud starts with acknowledgement", policy: "allow", host: cloud,
			wantWarning:  baseWarning + "; this broker runs on a detected cloud host and the start was allowed by the broker setting " + runtime.HostNetworkBlockPolicyEnvVar,
			wantDetected: true},
		{name: "allow off cloud warns", policy: "allow", host: none, wantWarning: baseWarning, wantDetected: true},
		{name: "unrecognized value is strict", policy: "permissive", host: none,
			wantErr: []string{runtime.HostNetworkBlockPolicyEnvVar + " refuses"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(runtime.HostNetworkBlockPolicyEnvVar, tt.policy)
			consulted := false
			orig := detectCloudHost
			detectCloudHost = func() runtime.CloudHost { consulted = true; return tt.host }
			t.Cleanup(func() { detectCloudHost = orig })

			warning, err := hostNetworkBlockPolicy("a1", baseWarning)
			if consulted != tt.wantDetected {
				t.Errorf("detection consulted = %v, want %v", consulted, tt.wantDetected)
			}
			if tt.wantErr != nil {
				if err == nil {
					t.Fatalf("expected error, got warning %q", warning)
				}
				for _, s := range tt.wantErr {
					if !strings.Contains(err.Error(), s) {
						t.Errorf("error %q does not contain %q", err, s)
					}
				}
				if warning != "" {
					t.Errorf("warning = %q on refusal, want empty", warning)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if warning != tt.wantWarning {
				t.Errorf("warning = %q, want %q", warning, tt.wantWarning)
			}
		})
	}
}

func TestStart_HostNetworkBlockRefusedOnCloudHostBeforeRun(t *testing.T) {
	stubHostNetworkBlockPolicy(t, "", runtime.CloudHost{Detected: true, Source: "dmi", Provider: "gce"})
	_, cfg, err := startHostNetFixture(t, "http://localhost:8080", "block")
	if err == nil {
		t.Fatal("Start succeeded, want a refusal for block under host networking on a detected cloud host")
	}
	for _, want := range []string{`"block"`, "host networking", "detected cloud host", "bridge-reachable hub URL", runtime.HostNetworkBlockPolicyEnvVar + "=allow"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if cfg != nil {
		t.Error("the runtime's Run was called; a refused start must not create a container")
	}
}

func TestStart_HostNetworkBlockStrictRefusedOffCloud(t *testing.T) {
	stubHostNetworkBlockPolicy(t, "strict", runtime.CloudHost{})
	_, cfg, err := startHostNetFixture(t, "http://localhost:8080", "block")
	if err == nil {
		t.Fatal("Start succeeded, want a refusal under the strict policy")
	}
	if !strings.Contains(err.Error(), runtime.HostNetworkBlockPolicyEnvVar+" refuses") {
		t.Errorf("error = %q, want it to name the setting", err)
	}
	if cfg != nil {
		t.Error("the runtime's Run was called; a refused start must not create a container")
	}
}

func TestStart_HostNetworkBlockAllowedOnCloudHostRelaysAcknowledgement(t *testing.T) {
	stubHostNetworkBlockPolicy(t, "allow", runtime.CloudHost{Detected: true, Source: "cloud-init", Provider: "aws"})
	info, cfg, err := startHostNetFixture(t, "http://localhost:8080", "block")
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if cfg == nil || cfg.NetworkMode != "host" {
		t.Fatalf("RunConfig = %+v, want a host-network run", cfg)
	}
	if cfg.MetadataInterception {
		t.Error("MetadataInterception = true under host networking, want false (no NET_ADMIN)")
	}
	want := hostNetworkMetadataWarning + "; this broker runs on a detected cloud host and the start was allowed by the broker setting " + runtime.HostNetworkBlockPolicyEnvVar
	if !slices.Contains(info.RelayedWarnings, want) {
		t.Errorf("RelayedWarnings = %v, want the acknowledged warning", info.RelayedWarnings)
	}
}

func TestStart_BridgeBlockIgnoresCloudPolicy(t *testing.T) {
	stubHostNetworkBlockPolicy(t, "strict", runtime.CloudHost{Detected: true, Source: "dmi", Provider: "gce"})
	_, cfg, err := startHostNetFixture(t, "https://hub.example.com", "block")
	if err != nil {
		t.Fatalf("Start failed on bridge networking: %v", err)
	}
	if cfg == nil || cfg.NetworkMode != "" || !cfg.MetadataInterception {
		t.Fatalf("RunConfig = %+v, want bridge with interception", cfg)
	}
}

// captureHostNetSlog routes the default slog logger into a buffer for the rest of
// the test.
func captureHostNetSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(orig) })
	return &buf
}

func TestHostNetworkBlockPolicy_AllowOnCloudLogsAcknowledgement(t *testing.T) {
	stubHostNetworkBlockPolicy(t, "allow", runtime.CloudHost{Detected: true, Source: "dmi", Provider: "gce"})
	buf := captureHostNetSlog(t)
	if _, err := hostNetworkBlockPolicy("a1", "w"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var ack string
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, "allowed by broker policy") {
			ack = line
		}
	}
	if ack == "" {
		t.Fatalf("no Warn acknowledgement logged; log was:\n%s", buf.String())
	}
	for _, want := range []string{"agent=a1", "setting=" + runtime.HostNetworkBlockPolicyEnvVar, "detection=dmi"} {
		if !strings.Contains(ack, want) {
			t.Errorf("acknowledgement %q does not contain %q", ack, want)
		}
	}
}

func TestHostNetworkBlockPolicy_NoAcknowledgementOffCloud(t *testing.T) {
	stubHostNetworkBlockPolicy(t, "allow", runtime.CloudHost{})
	buf := captureHostNetSlog(t)
	if _, err := hostNetworkBlockPolicy("a1", "w"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(buf.String(), "allowed by broker policy") {
		t.Errorf("acknowledgement logged off a cloud host:\n%s", buf.String())
	}
}

func TestHostNetworkBlockPolicy_UnknownPolicyLogsError(t *testing.T) {
	stubHostNetworkBlockPolicy(t, "permissive", runtime.CloudHost{})
	buf := captureHostNetSlog(t)
	if _, err := hostNetworkBlockPolicy("a1", "w"); err == nil {
		t.Fatal("expected a refusal for an unrecognized policy value")
	}
	out := buf.String()
	if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "setting="+runtime.HostNetworkBlockPolicyEnvVar) {
		t.Errorf("no Error naming the setting logged; log was:\n%s", out)
	}
	if strings.Contains(out, "permissive") {
		t.Errorf("log names the setting value; log was:\n%s", out)
	}
}

// refusalLogLine returns the Start refusal log line from a captured log.
func refusalLogLine(log string) string {
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, "refusing agent start") {
			return line
		}
	}
	return ""
}

func TestStart_HostNetworkRefusalLogsReason(t *testing.T) {
	cloud := runtime.CloudHost{Detected: true, Source: "dmi", Provider: "gce"}
	tests := []struct {
		name       string
		policy     string
		host       runtime.CloudHost
		mode       string
		wantReason string
	}{
		{name: "assign", policy: "", host: runtime.CloudHost{}, mode: "assign", wantReason: "assign_unsupported"},
		{name: "block on cloud host", policy: "", host: cloud, mode: "block", wantReason: "cloud_host"},
		{name: "block under strict", policy: "strict", host: runtime.CloudHost{}, mode: "block", wantReason: "strict"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubHostNetworkBlockPolicy(t, tt.policy, tt.host)
			buf := captureHostNetSlog(t)
			if _, _, err := startHostNetFixture(t, "http://localhost:8080", tt.mode); err == nil {
				t.Fatal("Start succeeded, want a refusal")
			}
			line := refusalLogLine(buf.String())
			if line == "" {
				t.Fatalf("no refusal log line; log was:\n%s", buf.String())
			}
			for _, want := range []string{"level=WARN", "GCP metadata mode under host networking", "reason=" + tt.wantReason, "agent=tz-agent"} {
				if !strings.Contains(line, want) {
					t.Errorf("refusal log %q does not contain %q", line, want)
				}
			}
			if strings.Contains(line, "not supported") {
				t.Errorf("refusal log %q still describes every refusal as unsupported", line)
			}
		})
	}
}

func TestHostNetworkRefusalReason(t *testing.T) {
	if got := hostNetworkRefusalReason(errors.New("other")); got != "unknown" {
		t.Errorf("reason of a plain error = %q, want unknown", got)
	}
	wrapped := fmt.Errorf("wrapped: %w", &hostNetworkRefusal{reason: hostNetworkRefusalStrict, msg: "m"})
	if got := hostNetworkRefusalReason(wrapped); got != "strict" {
		t.Errorf("reason of a wrapped refusal = %q, want strict", got)
	}
}
