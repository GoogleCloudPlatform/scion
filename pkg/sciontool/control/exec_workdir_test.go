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

package control

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
)

func TestExecEnabled_CommandRequests_ShellOnly(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{
			name: "shell harness with command_requests enabled",
			env: map[string]string{
				EnvHubExperiments: "hub.conduit," + ExperimentCommandRequests,
				"SCION_HARNESS":   state.ShellHarnessName,
			},
			want: true,
		},
		{
			name: "LLM harness with command_requests alone never gets exec routes (AC-S2)",
			env: map[string]string{
				EnvHubExperiments: "hub.conduit," + ExperimentCommandRequests,
				"SCION_HARNESS":   "claude",
			},
			want: false,
		},
		{
			name: "unset harness with command_requests alone never gets exec routes (AC-S2)",
			env: map[string]string{
				EnvHubExperiments: "hub.conduit," + ExperimentCommandRequests,
			},
			want: false,
		},
		{
			name: "LLM harness with exec_schedules still gets exec routes",
			env: map[string]string{
				EnvHubExperiments: "hub.conduit," + ExperimentExecSchedules,
				"SCION_HARNESS":   "claude",
			},
			want: true,
		},
		{
			name: "shell harness without either exec experiment gets no exec routes",
			env: map[string]string{
				EnvHubExperiments: "hub.conduit",
				"SCION_HARNESS":   state.ShellHarnessName,
			},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExecEnabled(func(k string) string { return tc.env[k] })
			if got != tc.want {
				t.Fatalf("ExecEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestExecHandler_AdvertisesWorkdirFeature(t *testing.T) {
	f := newExecFixture(t)
	if !slices.Contains(f.h.Features(), agentcontrol.FeatureExecWorkdir) {
		t.Fatalf("Handler.Features() = %v, want %q", f.h.Features(), agentcontrol.FeatureExecWorkdir)
	}
	noExec := New(Options{KickTokenRefresh: func() {}})
	if slices.Contains(noExec.Features(), agentcontrol.FeatureExecWorkdir) {
		t.Fatalf("Handler without Exec must not advertise %q", agentcontrol.FeatureExecWorkdir)
	}
}

func TestExecRoute_Workdir(t *testing.T) {
	workspaceDir := t.TempDir()
	homeDir := t.TempDir()
	explicitDir := t.TempDir()
	symlinkDir := filepath.Join(t.TempDir(), "link-to-dir")
	if err := os.Symlink(explicitDir, symlinkDir); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	notADir := filepath.Join(t.TempDir(), "regular-file")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	missingDir := filepath.Join(t.TempDir(), "does-not-exist")

	resolvedHome, err := filepath.EvalSymlinks(homeDir)
	if err != nil {
		t.Fatalf("EvalSymlinks(homeDir): %v", err)
	}
	resolvedExplicit, err := filepath.EvalSymlinks(explicitDir)
	if err != nil {
		t.Fatalf("EvalSymlinks(explicitDir): %v", err)
	}

	f := newExecFixture(t, func(o *ExecOptions) {
		o.Workdir = workspaceDir
		o.Env = func() ([]string, error) {
			return []string{"PATH=/usr/bin:/bin", "HOME=" + homeDir}, nil
		}
	})

	// 1. "" -> legacy workspace behavior.
	reqLegacy := f.startReq(1, "legacy")
	status, resp := f.start(t, reqLegacy)
	if status != http.StatusAccepted || resp.State != agentcontrol.ExecStateRunning {
		t.Fatalf("legacy start = %d %+v", status, resp)
	}
	f.fx.mu.Lock()
	gotDir := f.fx.procs["legacy"].specs.dir
	f.fx.mu.Unlock()
	if gotDir != workspaceDir {
		t.Fatalf("legacy workdir = %q, want %q", gotDir, workspaceDir)
	}
	f.fx.end(t, "legacy", 0)
	<-f.finishedCh

	// 2. "~" -> run env HOME.
	reqHome := f.startReq(2, "home")
	reqHome.Workdir = "~"
	status, resp = f.start(t, reqHome)
	if status != http.StatusAccepted || resp.State != agentcontrol.ExecStateRunning {
		t.Fatalf("tilde start = %d %+v", status, resp)
	}
	f.fx.mu.Lock()
	gotDir = f.fx.procs["home"].specs.dir
	gotPWD := envValue(f.fx.procs["home"].specs.env, "PWD")
	f.fx.mu.Unlock()
	if gotDir != resolvedHome || gotPWD != resolvedHome {
		t.Fatalf("tilde workdir = (%q, PWD=%q), want %q", gotDir, gotPWD, resolvedHome)
	}
	f.fx.end(t, "home", 0)
	<-f.finishedCh

	// 3. Explicit symlink to directory -> EvalSymlinks target.
	reqLink := f.startReq(3, "link")
	reqLink.Workdir = symlinkDir
	status, resp = f.start(t, reqLink)
	if status != http.StatusAccepted || resp.State != agentcontrol.ExecStateRunning {
		t.Fatalf("symlink start = %d %+v", status, resp)
	}
	f.fx.mu.Lock()
	gotDir = f.fx.procs["link"].specs.dir
	f.fx.mu.Unlock()
	if gotDir != resolvedExplicit {
		t.Fatalf("symlink workdir = %q, want %q", gotDir, resolvedExplicit)
	}
	f.fx.end(t, "link", 0)
	<-f.finishedCh

	// 4. Replay of run 3 with a different workdir -> 409 mismatch (AC-S6).
	reqReplayDiff := reqLink
	reqReplayDiff.Workdir = "~"
	status, resp = f.start(t, reqReplayDiff)
	if status != http.StatusConflict || resp.Outcome != agentcontrol.ExecOutcomeMismatch {
		t.Fatalf("replay with different workdir = %d %+v, want 409 mismatch", status, resp)
	}

	// 5. Missing explicit directory -> start_failed, never falls back (AC-S6).
	reqMissing := f.startReq(4, "missing")
	reqMissing.Workdir = missingDir
	status, resp = f.start(t, reqMissing)
	if status != http.StatusAccepted || resp.State != agentcontrol.ExecStateFinished || resp.Outcome != agentcontrol.ExecOutcomeStartFailed {
		t.Fatalf("missing workdir = %d %+v, want 202 finished/start_failed", status, resp)
	}
	<-f.finishedCh

	// 6. Regular file as workdir -> start_failed, never falls back.
	reqFile := f.startReq(5, "file")
	reqFile.Workdir = notADir
	status, resp = f.start(t, reqFile)
	if status != http.StatusAccepted || resp.State != agentcontrol.ExecStateFinished || resp.Outcome != agentcontrol.ExecOutcomeStartFailed {
		t.Fatalf("file workdir = %d %+v, want 202 finished/start_failed", status, resp)
	}
	<-f.finishedCh
}
