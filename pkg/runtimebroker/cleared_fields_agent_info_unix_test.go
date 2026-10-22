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

package runtimebroker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// clearedFieldsInheritance reads the agent's agent-info.json confined to the
// agent home: a link there (to a record outside the home) or a FIFO is not
// read, so the outside record's template hash is never used.
func TestClearedFieldsInheritance_AgentInfoLinkAndFIFONotRead(t *testing.T) {
	base := t.TempDir()
	projectDir := filepath.Join(base, "proj", config.DotScion)
	agentName := "cf-agent"
	home := config.GetAgentHomePath(projectDir, agentName)
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside.json")
	// A template hash that would make the lookup ask the (absent) template
	// cache, so using this record shows up as a "template cache" error.
	if err := os.WriteFile(outside, []byte(`{"template":"x","templateHash":"sha256:`+strings.Repeat("a", 64)+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(outside)
	infoPath := filepath.Join(home, "agent-info.json")
	s := &Server{}

	// Control: the same record as a regular file in the home is used.
	if err := os.WriteFile(infoPath, before, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.clearedFieldsInheritance(context.Background(), filepath.Dir(projectDir), projectDir, agentName); err == nil || !strings.Contains(err.Error(), "template cache") {
		t.Fatalf("control: err = %v, want the record's template hash used", err)
	}

	_ = os.Remove(infoPath)
	if err := os.Symlink(outside, infoPath); err != nil {
		t.Fatal(err)
	}
	if _, err := s.clearedFieldsInheritance(context.Background(), filepath.Dir(projectDir), projectDir, agentName); err != nil && strings.Contains(err.Error(), "template cache") {
		t.Fatalf("link: err = %v; the record outside the home was read", err)
	}

	_ = os.Remove(infoPath)
	if err := syscall.Mkfifo(infoPath, 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_, _ = s.clearedFieldsInheritance(context.Background(), filepath.Dir(projectDir), projectDir, agentName)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("FIFO at agent-info.json blocked clearedFieldsInheritance")
	}
	if after, _ := os.ReadFile(outside); string(after) != string(before) {
		t.Fatal("outside record changed")
	}
}
