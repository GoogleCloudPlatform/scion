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

package hub

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestReportSSHHostKey: the host key goes to the agent's status endpoint
// as sshHostKey, alone, with the run-id header.
func TestReportSSHHostKey(t *testing.T) {
	t.Cleanup(SetTokenHome(t.TempDir()))
	var gotPath, gotRunID string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotRunID = r.Header.Get(RunIDHeader)
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := NewClientWithConfig(srv.URL, "tok", "agent-1")
	c.runID = "run-1"
	c.maxRetries = 0

	const key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOq1Ofh2a4Rp9yUdu8sUgQhj3eQ8k2jzYhZzF9h4Xk3N"
	if err := c.ReportSSHHostKey(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/v1/agents/agent-1/status" {
		t.Errorf("path %q", gotPath)
	}
	if gotRunID != "run-1" {
		t.Errorf("run-id header %q, want run-1", gotRunID)
	}
	if len(gotBody) != 1 || gotBody["sshHostKey"] != key {
		t.Errorf("body %v, want only sshHostKey", gotBody)
	}
}
