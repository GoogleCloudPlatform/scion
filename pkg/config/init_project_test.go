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
	"testing"
)

func TestInitProject_CreatesClaudeTemplate(t *testing.T) {
	// Create a temporary directory for the project
	tempDir, err := os.MkdirTemp("", "scion-init-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Run InitProject
	err = InitProject(tempDir, GetMockHarnesses())
	if err != nil {
		t.Fatalf("InitProject failed: %v", err)
	}

	// Verify that templates/claude exists
	claudeDir := filepath.Join(tempDir, "templates", "claude")
	if _, err := os.Stat(claudeDir); os.IsNotExist(err) {
		t.Errorf("Expected templates/claude to be created, but it was not")
	}

	// Verify a file inside templates/claude exists to be sure (now YAML)
	claudeSettings := filepath.Join(claudeDir, "scion-agent.yaml")
	if _, err := os.Stat(claudeSettings); os.IsNotExist(err) {
		t.Errorf("Expected templates/claude/scion-agent.yaml to be created, but it was not")
	}
}
