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

package supervisor

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// TestChildEnv pins the harness child's environment assembly: user
// identity when dropping privileges, SCION_EXTRA_PATH prepended to PATH
// and removed, the overlay merged without overriding the runtime env,
// and secret overrides replacing entries.
func TestChildEnv(t *testing.T) {
	base := []string{"PATH=/usr/bin", "SCION_EXTRA_PATH=/opt/x", "HOME=/root", "KEEP=runtime", "API_KEY="}
	cfg := Config{
		UID: 1000, GID: 1000, Username: "scion",
		EnvOverlay:      map[string]string{"KEEP": "overlay", "NEW": "n"},
		SecretOverrides: map[string]string{"API_KEY": "fetched"},
	}
	got, err := ChildEnv(cfg, base)
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range []string{"PATH=/opt/x:/usr/bin", "HOME=/home/scion", "USER=scion", "LOGNAME=scion", "KEEP=runtime", "NEW=n", "API_KEY=fetched"} {
		if !slices.Contains(got, kv) {
			t.Errorf("ChildEnv lacks the expected value of %s; keys: %v", envKey(kv), envKeys(got))
		}
	}
	for _, kv := range got {
		if kv == "SCION_EXTRA_PATH=/opt/x" || kv == "HOME=/root" || kv == "API_KEY=" {
			t.Errorf("ChildEnv keeps the base value of %s", envKey(kv))
		}
	}
	if base[0] != "PATH=/usr/bin" {
		t.Error("ChildEnv modified its base")
	}

	// No privilege drop and not rootless: the identity is left alone.
	got, err = ChildEnv(Config{Username: "scion"}, []string{"HOME=/root"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"HOME=/root"}) {
		t.Errorf("ChildEnv without a drop changed the environment; keys: %v", envKeys(got))
	}
}

// TestSSHSessionEnvWithoutSecretOverrides: SSH sessions do not receive
// secret overrides; other environment matches the harness child.
func TestSSHSessionEnvWithoutSecretOverrides(t *testing.T) {
	base := []string{"PATH=/usr/bin", "SCION_EXTRA_PATH=/opt/x", "API_KEY="}
	cfg := Config{
		UID: 1000, GID: 1000, Username: "scion",
		EnvOverlay:      map[string]string{"NEW": "n"},
		SecretOverrides: map[string]string{"API_KEY": "fetched", "ADDED_SECRET": "a"},
	}
	harness, err := ChildEnv(cfg, base)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(harness, "API_KEY=fetched") || !slices.Contains(harness, "ADDED_SECRET=a") {
		t.Fatalf("harness env lacks the secret overrides; keys: %v", envKeys(harness))
	}
	ssh, err := SSHSessionEnv(cfg, base)
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range ssh {
		if kv == "API_KEY=fetched" || strings.HasPrefix(kv, "ADDED_SECRET=") {
			t.Errorf("SSH session env has secret override %s", envKey(kv))
		}
	}
	noSecrets := cfg
	noSecrets.SecretOverrides = nil
	want, err := ChildEnv(noSecrets, base)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ssh, want) {
		t.Fatalf("SSH session env differs from the harness env without secret overrides; keys %v, want keys %v", envKeys(ssh), envKeys(want))
	}
}

// TestSSHSessionEnvWritesNoDebugLines: ChildEnv writes its debug lines;
// SSHSessionEnv, which runs for every SSH session, writes none.
func TestSSHSessionEnvWritesNoDebugLines(t *testing.T) {
	// Keep this test's log file out of the default agent.log. Setting HOME
	// would not do it: the default path is /home/scion/agent.log whenever
	// /home/scion exists, and a chosen path is kept.
	log.SetLogPath(filepath.Join(t.TempDir(), "agent.log"))
	// Initialize the log package before SetDebug: initialization sets the
	// debug switch from the shared level, which would undo SetDebug.
	log.Init()
	log.SetDebug(true)
	t.Cleanup(func() {
		// Back to the default log path and the shared level
		// (SCION_LOG_LEVEL) for later tests.
		log.SetLogPath("")
		log.Init()
	})
	base := []string{"PATH=/usr/bin", "SCION_EXTRA_PATH=/opt/x"}
	cfg := Config{UID: 1000, GID: 1000, Username: "scion", EnvOverlay: map[string]string{"NEW": "n"}}
	harness := captureStderr(t, func() {
		if _, err := ChildEnv(cfg, base); err != nil {
			t.Error(err)
		}
	})
	for _, want := range []string{"Child env: HOME=", "Applied SCION_EXTRA_PATH: PATH=", "Applied harness env overlay"} {
		if !strings.Contains(harness, want) {
			t.Errorf("ChildEnv debug output lacks %q: %q", want, harness)
		}
	}
	ssh := captureStderr(t, func() {
		if _, err := SSHSessionEnv(cfg, base); err != nil {
			t.Error(err)
		}
	})
	if strings.TrimSpace(ssh) != "" {
		t.Errorf("SSHSessionEnv wrote debug output: %q", ssh)
	}
}

// envKey is the key of a KEY=VALUE entry. Failure messages name keys
// only, never values.
func envKey(kv string) string {
	if i := strings.IndexByte(kv, '='); i >= 0 {
		return kv[:i]
	}
	return kv
}

// envKeys lists the keys of env, in order.
func envKeys(env []string) []string {
	keys := make([]string, len(env))
	for i, kv := range env {
		keys[i] = envKey(kv)
	}
	return keys
}
