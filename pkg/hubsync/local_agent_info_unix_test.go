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

package hubsync

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// getLocalAgentInfo reads the agent's agent-info.json confined to the agent
// home: a link there (to a record outside the home) or a FIFO is not read;
// the legacy scion-agent.json fallback applies instead.
func TestGetLocalAgentInfo_AgentInfoLinkAndFIFONotRead(t *testing.T) {
	base := t.TempDir()
	agentDir := filepath.Join(base, "agents", "myagent")
	home := filepath.Join(agentDir, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{"harness_config":"local-hc"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside.json")
	if err := os.WriteFile(outside, []byte(`{"template":"outside-tpl","harnessConfig":"outside-hc"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	infoPath := filepath.Join(home, "agent-info.json")
	if err := os.Symlink(outside, infoPath); err != nil {
		t.Fatal(err)
	}
	if got := getLocalAgentInfo(base, "myagent"); got != nil && (got.Template == "outside-tpl" || got.HarnessConfig == "outside-hc") {
		t.Fatalf("link: getLocalAgentInfo = %+v; the record outside the home was read", got)
	}

	_ = os.Remove(infoPath)
	if err := syscall.Mkfifo(infoPath, 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = getLocalAgentInfo(base, "myagent"); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("FIFO at agent-info.json blocked getLocalAgentInfo")
	}
}
