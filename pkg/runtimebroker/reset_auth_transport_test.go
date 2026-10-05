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
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

type stdinExec struct {
	cmd   string
	stdin string
}

type resetAuthRecorder struct {
	mu        sync.Mutex
	withStdin []stdinExec
	plain     [][]string
	failOn    string // fail an ExecWithStdin whose script contains this
}

func (r *resetAuthRecorder) runtime() *runtime.MockRuntime {
	return &runtime.MockRuntime{
		NameFunc: func() string { return "docker" },
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{{
				Name:        "dev",
				ContainerID: "c-dev",
				Labels:      map[string]string{"scion.name": "dev", projectkeys.LabelProjectID: stopLookupProjectID},
			}}, nil
		},
		ExecWithStdinFunc: func(_ context.Context, _ string, cmd []string, stdin io.Reader) (string, error) {
			data, _ := io.ReadAll(stdin)
			script := strings.Join(cmd, " ")
			r.mu.Lock()
			defer r.mu.Unlock()
			r.withStdin = append(r.withStdin, stdinExec{cmd: script, stdin: string(data)})
			if r.failOn != "" && strings.Contains(script, r.failOn) {
				return "", errors.New("exec failed")
			}
			return "", nil
		},
		ExecFunc: func(_ context.Context, _ string, cmd []string) (string, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.plain = append(r.plain, cmd)
			return "", nil
		},
	}
}

func doTransportResetAuth(t *testing.T, rec *resetAuthRecorder, body string) *httptest.ResponseRecorder {
	t.Helper()
	rt := rec.runtime()
	mgr := agent.NewManager(rt)
	t.Cleanup(mgr.Close)
	srv := New(DefaultServerConfig(), mgr, rt)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/dev/reset-auth", strings.NewReader(body))
	srv.resetAuth(w, req, "dev", stopLookupProjectID)
	return w
}

func TestResetAuth_WritesTransportTokenWhenProvided(t *testing.T) {
	rec := &resetAuthRecorder{}
	w := doTransportResetAuth(t, rec, `{"token":"app-value","transportToken":"transport-value"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if len(rec.withStdin) != 2 {
		t.Fatalf("ExecWithStdin calls = %d, want 2 (agent token, transport token)", len(rec.withStdin))
	}
	app, tr := rec.withStdin[0], rec.withStdin[1]
	if app.stdin != "app-value" || !strings.Contains(app.cmd, "scion-token") {
		t.Errorf("first write = %+v, want agent token to scion-token", app)
	}
	if tr.stdin != "transport-value" || !strings.Contains(tr.cmd, "/transport-token\"") {
		t.Errorf("second write = %+v, want transport token to transport-token", tr)
	}
	if !strings.Contains(tr.cmd, "umask 077") {
		t.Errorf("transport write must create the file 0600: %q", tr.cmd)
	}
	if strings.Contains(tr.cmd, "transport-value") {
		t.Errorf("token value must travel on stdin, not in the command")
	}
	// The PID 1 signal comes after both writes.
	if len(rec.plain) != 1 || strings.Join(rec.plain[0], " ") != "kill -USR2 1" {
		t.Errorf("signal calls = %v", rec.plain)
	}
	if !strings.Contains(w.Body.String(), "transport token written") {
		t.Errorf("response should report the transport write: %s", w.Body.String())
	}
}

func TestResetAuth_NoTransportTokenSingleWrite(t *testing.T) {
	rec := &resetAuthRecorder{}
	w := doTransportResetAuth(t, rec, `{"token":"app-value"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if len(rec.withStdin) != 1 {
		t.Fatalf("ExecWithStdin calls = %d, want 1", len(rec.withStdin))
	}
	if strings.Contains(w.Body.String(), "transport") {
		t.Errorf("response should not mention transport: %s", w.Body.String())
	}
}

func TestResetAuth_TransportWriteFailureDoesNotFailReset(t *testing.T) {
	rec := &resetAuthRecorder{failOn: "transport-token"}
	w := doTransportResetAuth(t, rec, `{"token":"app-value","transportToken":"transport-value"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if len(rec.plain) != 1 {
		t.Errorf("init must still be signalled, got %v", rec.plain)
	}
	if !strings.Contains(w.Body.String(), "transport token write failed") {
		t.Errorf("response should report the failed transport write: %s", w.Body.String())
	}
}

// TestScionTokenDirScript runs the shared TOKEN_DIR prelude used by both
// reset-auth writes: it uses the scion user's home from getent, and falls
// back to /home/scion when getent is missing or returns nothing.
func TestScionTokenDirScript(t *testing.T) {
	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	cutPath, err := exec.LookPath("cut")
	if err != nil {
		t.Skip("cut not available")
	}
	run := func(t *testing.T, getent string) string {
		t.Helper()
		bin := t.TempDir()
		if err := os.Symlink(cutPath, filepath.Join(bin, "cut")); err != nil {
			t.Fatal(err)
		}
		if getent != "" {
			if err := os.WriteFile(filepath.Join(bin, "getent"), []byte("#!"+shPath+"\n"+getent+"\n"), 0755); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command(shPath, "-c", scionTokenDirScript+` && printf %s "$TOKEN_DIR"`)
		cmd.Env = []string{"PATH=" + bin}
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("script failed: %v", err)
		}
		return string(out)
	}

	if got := run(t, ""); got != "/home/scion/.scion" {
		t.Errorf("without getent: TOKEN_DIR=%q, want /home/scion/.scion", got)
	}
	if got := run(t, "exit 2"); got != "/home/scion/.scion" {
		t.Errorf("no getent entry: TOKEN_DIR=%q, want /home/scion/.scion", got)
	}
	if got := run(t, "echo 'scion:x:1000:1000::/srv/scion-home:/bin/sh'"); got != "/srv/scion-home/.scion" {
		t.Errorf("with getent: TOKEN_DIR=%q, want /srv/scion-home/.scion", got)
	}
	for _, c := range [][]string{scionTokenWriteCmd(), transportTokenWriteCmd()} {
		if !strings.Contains(c[2], scionTokenDirScript) {
			t.Errorf("write command does not use the shared TOKEN_DIR prelude: %q", c[2])
		}
	}
}
