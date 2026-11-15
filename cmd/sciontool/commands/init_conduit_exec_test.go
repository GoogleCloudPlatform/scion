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

//go:build linux

package commands

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	core "github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/control"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/supervisor"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

func execEnvWith(experiments string) func(string) string {
	return func(k string) string {
		switch k {
		case control.EnvHubExperiments:
			return experiments
		case conduit.EnvLaunchID:
			return "launch-7"
		}
		return ""
	}
}

// TestConduitExecOptions: the exec routes are registered only for an agent
// launched with hub.exec_schedules, and run as the PTY user with its
// privilege-drop rule.
func TestConduitExecOptions(t *testing.T) {
	u := conduit.PTYUser{UID: 1000, GID: 1001, Username: "scion", RequirePrivilegeDrop: true}
	env := func() ([]string, error) { return nil, nil }
	if conduitExecOptions(execEnvWith("hub.conduit,hub.conduit_deliver"), u, env) != nil {
		t.Fatal("registered without the launch flag")
	}
	if conduitExecOptions(execEnvWith(""), u, env) != nil {
		t.Fatal("registered with no launch experiments")
	}
	o := conduitExecOptions(execEnvWith("hub.conduit,hub.exec_schedules"), u, env)
	if o == nil {
		t.Fatal("not registered with the launch flag")
	}
	if o.UID != 1000 || o.GID != 1001 || o.Username != "scion" || !o.RequirePrivilegeDrop || o.LaunchID != "launch-7" || o.Env == nil {
		t.Fatalf("options = %+v", o)
	}
}

// TestHarnessShellEnvParity runs a real command through the exec route
// with the environment sciontool init wires (harnessShellEnv of the
// harness supervisor config) and checks that the run sees the same
// environment keys as the harness child (supervisor.ChildEnv), plus PWD:
// secret overrides and resolved auth included, unlike SSH sessions and the
// PTY client.
func TestHarnessShellEnvParity(t *testing.T) {
	t.Setenv("SCION_EXTRA_PATH", "/opt/extra")
	t.Setenv("PATH", "/usr/bin:/bin")
	cfg := harnessSupervisorConfig(InitRunOptions{}, time.Second, 0, 0, false,
		map[string]string{"OVERLAY_ONLY": "o"}, "", map[string]string{"SECRET_OVERRIDE": "s"})
	cfg.AuthEnv = map[string]string{"AUTH_ONLY": "a"}

	want, err := supervisor.ChildEnv(cfg, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	opts := conduitExecOptions(execEnvWith("hub.exec_schedules"), conduit.PTYUser{}, harnessShellEnv(cfg))
	dir := t.TempDir()
	opts.Workdir = dir
	h := control.New(control.Options{KickTokenRefresh: func() {}, Exec: opts})

	out := filepath.Join(dir, "env.out")
	req := agentcontrol.ExecStartRequest{
		V: 1, RunID: "00000000-0000-4000-8000-0000000000e1",
		Argv: []string{"sh", "-c", "env -0 > " + out}, TimeoutS: 10,
		NotAfterUnixMS: time.Now().Add(time.Minute).UnixMilli(),
	}
	if code := postControl(t, h, control.RouteExecStart, req); code != http.StatusAccepted {
		t.Fatalf("exec-start = %d", code)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		code := postControl(t, h, control.RouteExecStatus, agentcontrol.ExecStatusRequest{V: 1, RunID: req.RunID})
		if code != http.StatusOK {
			t.Fatalf("exec-status = %d", code)
		}
		if b, err := os.ReadFile(out); err == nil && len(b) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("run never wrote its environment")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // let env finish writing
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, kv := range strings.Split(strings.TrimRight(string(b), "\x00"), "\x00") {
		k, _, _ := strings.Cut(kv, "=")
		got[k] = true
	}
	wantKeys := map[string]bool{"PWD": true}
	for _, kv := range want {
		k, _, _ := strings.Cut(kv, "=")
		wantKeys[k] = true
	}
	// sh adds a few of its own (SHLVL, _, OLDPWD); ignore exactly those.
	for _, k := range []string{"SHLVL", "_", "OLDPWD"} {
		if !wantKeys[k] {
			delete(got, k)
		}
	}
	var missing, extra []string
	for k := range wantKeys {
		if !got[k] {
			missing = append(missing, k)
		}
	}
	for k := range got {
		if !wantKeys[k] {
			extra = append(extra, k)
		}
	}
	slices.Sort(missing)
	slices.Sort(extra)
	if len(missing)+len(extra) > 0 {
		t.Fatalf("run env keys differ from the harness env: missing %v, extra %v", missing, extra)
	}
	for _, k := range []string{"SECRET_OVERRIDE", "AUTH_ONLY", "OVERLAY_ONLY"} {
		if !got[k] {
			t.Errorf("run env lacks %s", k)
		}
	}
	if got["SCION_EXTRA_PATH"] {
		t.Error("SCION_EXTRA_PATH must be folded into PATH, as for the harness")
	}
}

func postControl(t *testing.T, h http.Handler, route string, body any) int {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, control.Prefix+route, strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// TestPortForwardingConduitExec: with the launch flag, the Hello advertises
// exec-start and exec-status and a hub RpcRequest on the agent's own
// session runs a command; without it, neither route is advertised or
// reachable.
func TestPortForwardingConduitExec(t *testing.T) {
	for _, withFlag := range []bool{true, false} {
		name := map[bool]string{true: "flag", false: "no flag"}[withFlag]
		t.Run(name, func(t *testing.T) {
			t.Cleanup(hub.SetTokenHome(t.TempDir()))
			h := newPFHub(t, true)
			client := hub.NewClientWithConfig(h.srv.URL, "token-1", "agent-1")
			exps := conduit.ExperimentConduit
			if withFlag {
				exps += "," + control.ExperimentExecSchedules
			}
			env := map[string]string{
				conduit.EnvHubExperiments: exps,
				projectkeys.EnvProjectID:  "project-1",
				conduit.EnvLaunchID:       "launch-1",
			}
			getenv := func(k string) string { return env[k] }
			execOpts := conduitExecOptions(getenv, conduit.PTYUser{}, func() ([]string, error) {
				return []string{"PATH=/usr/bin:/bin"}, nil
			})
			if withFlag != (execOpts != nil) {
				t.Fatalf("exec options = %v with flag %t", execOpts, withFlag)
			}
			ctx, cancel := context.WithCancel(context.Background())
			p := newPortForwarding(client, false, conduit.PTYUser{}, nil, nil, execOpts, getenv)
			done := make(chan struct{})
			go func() { p.run(ctx); close(done) }()
			t.Cleanup(func() {
				cancel()
				<-done
			})
			var hello *conduitv1.Hello
			select {
			case hello = <-h.hellos:
			case <-time.After(pfWait):
				t.Fatal("no conduit Hello")
			}
			want := []string{control.RouteRotateToken}
			if withFlag {
				want = append(want, control.RouteExecStart, control.RouteExecStatus)
			}
			if got := hello.GetCapabilities().GetRpc(); !slices.Equal(got, want) {
				t.Fatalf("Hello rpc = %v, want %v", got, want)
			}
			var s core.Session
			select {
			case s = <-h.sessions:
			case <-time.After(pfWait):
				t.Fatal("no conduit session")
			}
			body, _ := json.Marshal(agentcontrol.ExecStartRequest{
				V: 1, RunID: "00000000-0000-4000-8000-0000000000e2", Argv: []string{"true"}, TimeoutS: 5,
				NotAfterUnixMS: time.Now().Add(time.Minute).UnixMilli(),
			})
			resp, err := s.Call(ctx, &conduitv1.RpcRequest{
				Method: "POST", Path: "/v1/control/exec-start", Body: body,
				Headers: map[string]string{"Content-Type": "application/json"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if !withFlag {
				if resp.GetStatus() != http.StatusNotFound {
					t.Fatalf("exec-start without the flag = %d, want 404", resp.GetStatus())
				}
				return
			}
			res, err := agentcontrol.ParseExecStartAnswer(int(resp.GetStatus()), resp.GetBody())
			if err != nil || res.Status != http.StatusAccepted {
				t.Fatalf("exec-start = %d %q (%v)", resp.GetStatus(), resp.GetBody(), err)
			}
		})
	}
}
