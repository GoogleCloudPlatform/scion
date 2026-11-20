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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func stubDeleteOwnership(t *testing.T, brokerUID, brokerGID int, owners map[string][2]int) {
	t.Helper()
	origOwner := deletePathOwner
	origUID := deleteProcessUID
	origGID := deleteProcessGID
	deleteProcessUID = func() int { return brokerUID }
	deleteProcessGID = func() int { return brokerGID }
	deletePathOwner = func(path string) (int, int, bool) {
		if pair, ok := owners[filepath.Clean(path)]; ok {
			return pair[0], pair[1], true
		}
		return 0, 0, false
	}
	t.Cleanup(func() {
		deletePathOwner = origOwner
		deleteProcessUID = origUID
		deleteProcessGID = origGID
	})
}

func TestDetectDeleteOwnershipMismatch(t *testing.T) {
	rootDir := "/home/scion/.scion/project-configs"
	homeDir := filepath.Join(rootDir, "proj__0e844da0", ".scion", "agents", "test", "home")
	bashrc := filepath.Join(homeDir, ".bashrc")
	relBashrc := filepath.Join("proj__0e844da0", ".scion", "agents", "test", "home", ".bashrc")

	t.Run("foreign-owned parent with absolute path", func(t *testing.T) {
		stubDeleteOwnership(t, 1002, 1002, map[string][2]int{
			homeDir: {1000, 1000},
		})
		err := fmt.Errorf("failed to remove agent directory: %w", &fs.PathError{
			Op: "remove", Path: bashrc, Err: fs.ErrPermission,
		})
		m, ok := detectDeleteOwnershipMismatch(err, "")
		if !ok {
			t.Fatal("expected ownership mismatch to be detected")
		}
		if m.Path != bashrc || m.OwnerPath != homeDir {
			t.Errorf("got path=%q ownerPath=%q, want path=%q ownerPath=%q", m.Path, m.OwnerPath, bashrc, homeDir)
		}
		if m.OwnerUID != 1000 || m.OwnerGID != 1000 || m.BrokerUID != 1002 || m.BrokerGID != 1002 {
			t.Errorf("unexpected ids: %+v", m)
		}
	})

	t.Run("foreign-owned parent with os.Root-relative path", func(t *testing.T) {
		stubDeleteOwnership(t, 1002, 1002, map[string][2]int{
			homeDir: {1000, 1000},
		})
		err := &fs.PathError{Op: "remove", Path: relBashrc, Err: fs.ErrPermission}
		m, ok := detectDeleteOwnershipMismatch(err, rootDir)
		if !ok {
			t.Fatal("expected ownership mismatch to be detected for relative os.Root path")
		}
		if m.Path != bashrc || m.OwnerPath != homeDir {
			t.Errorf("got path=%q ownerPath=%q, want path=%q ownerPath=%q", m.Path, m.OwnerPath, bashrc, homeDir)
		}
	})

	t.Run("foreign-owned target directory itself", func(t *testing.T) {
		stubDeleteOwnership(t, 1002, 1002, map[string][2]int{
			filepath.Dir(homeDir): {1002, 1002},
			homeDir:               {1000, 1000},
		})
		err := &fs.PathError{Op: "open", Path: homeDir, Err: fs.ErrPermission}
		m, ok := detectDeleteOwnershipMismatch(err, "")
		if !ok {
			t.Fatal("expected ownership mismatch on unreadable foreign-owned dir")
		}
		if m.OwnerPath != homeDir || m.OwnerUID != 1000 {
			t.Errorf("got %+v, want ownerPath=%q ownerUID=1000", m, homeDir)
		}
	})

	t.Run("same-UID permission error is not misclassified", func(t *testing.T) {
		stubDeleteOwnership(t, 1002, 1002, map[string][2]int{
			homeDir: {1002, 1002},
			bashrc:  {1002, 1002},
		})
		err := &fs.PathError{Op: "remove", Path: bashrc, Err: fs.ErrPermission}
		if m, ok := detectDeleteOwnershipMismatch(err, ""); ok {
			t.Fatalf("expected no mismatch for same-UID permission error, got %+v", m)
		}
	})

	t.Run("non-permission error is ignored", func(t *testing.T) {
		stubDeleteOwnership(t, 1002, 1002, map[string][2]int{
			homeDir: {1000, 1000},
		})
		err := &fs.PathError{Op: "remove", Path: bashrc, Err: errors.New("i/o error")}
		if m, ok := detectDeleteOwnershipMismatch(err, ""); ok {
			t.Fatalf("expected no mismatch for non-permission error, got %+v", m)
		}
	})
}

func TestDeleteProject_ForeignOwnedHomeReturnsClearErrorAndLogsMismatch(t *testing.T) {
	srv, home := newScopeTestServer(t, &filteringMockManager{})
	var logBuf bytes.Buffer
	srv.agentLifecycleLog = slog.New(slog.NewJSONHandler(&logBuf, nil))

	makeHubMarkerProject(t, home, "proj-a", scopeProjA, "dev")
	dirA := seedProjectConfigsEntry(t, home, "proj-a", scopeProjA)

	agentHome := filepath.Join(dirA, config.DotScion, "agents", "test", "home")
	bashrc := filepath.Join(agentHome, ".bashrc")
	rootDir := filepath.Join(home, config.DotScion, config.ProjectConfigsDir)
	relBashrc, err := filepath.Rel(rootDir, bashrc)
	if err != nil {
		t.Fatal(err)
	}

	stubDeleteOwnership(t, 1002, 1002, map[string][2]int{
		agentHome: {1000, 1000},
	})

	srv.mu.Lock()
	srv.projectConfigsRemove = func(string, string) error {
		return &fs.PathError{Op: "remove", Path: relBashrc, Err: fs.ErrPermission}
	}
	srv.mu.Unlock()

	rec := doDeleteProject(t, srv, "proj-a", scopeProjA)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", rec.Code, rec.Body.String())
	}

	var resp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal error response: %v (body=%s)", err, rec.Body.String())
	}
	if resp.Error.Code != ErrCodeRuntimeError {
		t.Errorf("code = %q, want %q", resp.Error.Code, ErrCodeRuntimeError)
	}
	wantMsg := deleteOwnershipMismatchMessage(opRemoveProjectConfigDir)
	if resp.Error.Message != wantMsg {
		t.Errorf("message = %q, want %q", resp.Error.Message, wantMsg)
	}
	for _, forbidden := range []string{home, ".bashrc", "project-configs", "agents/test"} {
		if strings.Contains(rec.Body.String(), forbidden) {
			t.Errorf("response leaked host path fragment %q: %s", forbidden, rec.Body.String())
		}
	}

	logOut := logBuf.String()
	for _, wantLog := range []string{
		`"reason":"file_ownership_mismatch"`,
		bashrc,
		agentHome,
		`"owner_uid":1000`,
		`"owner_gid":1000`,
		`"broker_uid":1002`,
		`"broker_gid":1002`,
	} {
		if !strings.Contains(logOut, wantLog) {
			t.Errorf("server log missing %q:\n%s", wantLog, logOut)
		}
	}
}

type deleteErrMockManager struct {
	*filteringMockManager
	deleteErr error
}

func (m *deleteErrMockManager) DeleteTarget(_ context.Context, _, _ string, _ runtime.RunRef, _ bool, _ string, _ bool) (bool, error) {
	return false, m.deleteErr
}

func TestDeleteAgent_ForeignOwnedHomeReturnsClearErrorAndLogsMismatch(t *testing.T) {
	baseMgr := &filteringMockManager{}
	srv, home := newScopeTestServer(t, baseMgr)
	var logBuf bytes.Buffer
	srv.agentLifecycleLog = slog.New(slog.NewJSONHandler(&logBuf, nil))

	projDir := filepath.Join(home, config.DotScion, "projects", "proj-a")
	agentHome := filepath.Join(home, config.DotScion, config.ProjectConfigsDir, "proj-a__11111111", config.DotScion, "agents", "test", "home")
	bashrc := filepath.Join(agentHome, ".bashrc")

	baseMgr.agents = []api.AgentInfo{
		labelled("test", "cid-test", scopeProjA, filepath.Join(projDir, config.DotScion)),
	}
	errMgr := &deleteErrMockManager{
		filteringMockManager: baseMgr,
		deleteErr: fmt.Errorf("%w: delete: remove external agent state dir: %w",
			agent.ErrAgentFilesDelete,
			&fs.PathError{Op: "remove", Path: bashrc, Err: fs.ErrPermission},
		),
	}
	srv.mu.Lock()
	srv.manager = errMgr
	srv.mu.Unlock()

	stubDeleteOwnership(t, 1002, 1002, map[string][2]int{
		agentHome: {1000, 1000},
	})

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/test?projectId="+scopeProjA+"&deleteFiles=true", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", rec.Code, rec.Body.String())
	}
	var resp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal error response: %v (body=%s)", err, rec.Body.String())
	}
	wantMsg := deleteOwnershipMismatchMessage("delete agent")
	if resp.Error.Message != wantMsg {
		t.Errorf("message = %q, want %q", resp.Error.Message, wantMsg)
	}
	for _, forbidden := range []string{home, ".bashrc", "project-configs"} {
		if strings.Contains(rec.Body.String(), forbidden) {
			t.Errorf("response leaked host path fragment %q: %s", forbidden, rec.Body.String())
		}
	}

	logOut := logBuf.String()
	for _, wantLog := range []string{
		`"reason":"file_ownership_mismatch"`,
		bashrc,
		agentHome,
		`"owner_uid":1000`,
		`"broker_uid":1002`,
	} {
		if !strings.Contains(logOut, wantLog) {
			t.Errorf("server log missing %q:\n%s", wantLog, logOut)
		}
	}
}
