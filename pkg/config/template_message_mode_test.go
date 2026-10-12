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

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadConfigMessageMode: scion-agent.yaml's message_mode (a job key the
// hub reads from the template snapshot, ptone/scion#4222) parses, and
// LoadConfig refuses a value that is not a message mode.
func TestLoadConfigMessageMode(t *testing.T) {
	load := func(t *testing.T, body string) (string, error) {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "scion-agent.yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := (&Template{Path: dir}).LoadConfig()
		if err != nil {
			return "", err
		}
		return cfg.MessageMode, nil
	}

	for _, mode := range []string{"none", "lineage", "branch", "project", "hub"} {
		got, err := load(t, "message_mode: "+mode+"\n")
		if err != nil {
			t.Fatalf("message_mode %q: unexpected error: %v", mode, err)
		}
		if got != mode {
			t.Errorf("message_mode %q parsed as %q", mode, got)
		}
	}
	if got, err := load(t, "message-mode: lineage\n"); err != nil || got != "lineage" {
		t.Errorf("hyphenated key: got %q, %v", got, err)
	}
	if got, err := load(t, "model: m\n"); err != nil || got != "" {
		t.Errorf("unset: got %q, %v", got, err)
	}
	if _, err := load(t, "message_mode: everyone\n"); err == nil || !strings.Contains(err.Error(), "message_mode") {
		t.Errorf("invalid mode: want a message_mode error, got %v", err)
	}
}
