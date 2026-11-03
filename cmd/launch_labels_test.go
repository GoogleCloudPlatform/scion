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

package cmd

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for the launch step and code labels the CLI shows while waiting for
// a Hub launch and when it fails (design wave 3 §3 E-e, E-T5; D-T8;
// ptone/scion#4670).

// apiLaunchVocabulary reads the launch step and code values from the pkg/api
// sources (LaunchStep* and LaunchCode* constants), so a step or code added
// there without a CLI label fails the label tests. The tests run with the
// package directory as the working directory.
func apiLaunchVocabulary(t *testing.T) (steps, codes []string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "pkg", "api", "*.go"))
	require.NoError(t, err)
	values := map[string]string{} // every string constant in pkg/api
	idents := map[string]string{} // constants defined as another constant
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		require.NoError(t, err)
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					switch v := vs.Values[i].(type) {
					case *ast.BasicLit:
						if v.Kind == token.STRING {
							s, err := strconv.Unquote(v.Value)
							require.NoError(t, err)
							values[name.Name] = s
						}
					case *ast.Ident:
						idents[name.Name] = v.Name
					}
				}
			}
		}
	}
	for name, ref := range idents {
		if s, ok := values[ref]; ok {
			values[name] = s
		}
	}
	for name, v := range values {
		switch {
		case strings.HasPrefix(name, "LaunchStep"):
			steps = append(steps, v)
		case strings.HasPrefix(name, "LaunchCode"):
			codes = append(codes, v)
		}
	}
	sort.Strings(steps)
	sort.Strings(codes)
	require.NotEmpty(t, steps, "no LaunchStep constants found in pkg/api")
	require.NotEmpty(t, codes, "no LaunchCode constants found in pkg/api")
	return steps, codes
}

// TestLaunchProgress_EveryStepHasALabel: every pkg/api launch step prints a
// label in the wait's progress line, never the raw step or its fallback
// (except "launching", whose label is its name).
func TestLaunchProgress_EveryStepHasALabel(t *testing.T) {
	steps, _ := apiLaunchVocabulary(t)
	assert.Contains(t, steps, "awaiting_capacity")
	for _, step := range steps {
		var out bytes.Buffer
		var p launchProgress
		p.report(&out, "a1", &hubclient.Agent{Phase: "provisioning", Launch: activeLaunch(step, 2, nil)})
		line := out.String()
		require.True(t, strings.HasPrefix(line, "  a1: provisioning (") && strings.HasSuffix(line, ")\n"), "step %q: line %q", step, line)
		label := strings.TrimSuffix(strings.TrimPrefix(line, "  a1: provisioning ("), ")\n")
		if step == "launching" {
			// The shared table labels the generic step with its own name.
			assert.Equal(t, "launching", label)
			continue
		}
		assert.NotEqual(t, step, label, "step %q has no label", step)
		assert.NotEqual(t, strings.ReplaceAll(step, "_", " "), label, "step %q has no label (fallback shown)", step)
	}
}

// TestLaunchFailedError_EveryCodeHasALabel: every pkg/api launch code but
// launch_stopped, and the broker's skill_resolution_failed and
// storage_migration_required, is followed by a
// label in the failure text, never only the raw code or its fallback.
func TestLaunchFailedError_EveryCodeHasALabel(t *testing.T) {
	_, codes := apiLaunchVocabulary(t)
	assert.Contains(t, codes, "image_pull_failed")
	codes = append(codes, "skill_resolution_failed", "storage_migration_required")
	for _, code := range codes {
		if code == "launch_stopped" {
			continue // a stopped launch has its own neutral text (TestWaitForAgentLaunch_FailureNamesCodeAndStepLabels)
		}
		e := newLaunchFailedError("a1", &hubclient.Agent{Phase: "error",
			Launch: &hubclient.AgentLaunch{ID: "l1", State: "ended", Kind: "start", Error: code}})
		prefix := "agent 'a1' did not start (" + code + ": "
		require.True(t, strings.HasPrefix(e.Reason, prefix) && strings.HasSuffix(e.Reason, ")"), "code %q: reason %q", code, e.Reason)
		label := strings.TrimSuffix(strings.TrimPrefix(e.Reason, prefix), ")")
		assert.NotEmpty(t, label, "code %q", code)
		assert.NotEqual(t, strings.ReplaceAll(code, "_", " "), label, "code %q has no label (fallback shown)", code)
	}
}

// TestWaitForAgentLaunch_ProgressPrintsStepLabels: the progress lines show
// the step labels, and a step this CLI does not know is shown with "_" as a
// space.
func TestWaitForAgentLaunch_ProgressPrintsStepLabels(t *testing.T) {
	shortenLaunchWaitTimings(t)
	seq := &agentSequence{results: []func() (*hubclient.Agent, error){
		agentResult(&hubclient.Agent{Phase: "provisioning", Launch: activeLaunch("scheduling", 2, nil)}),
		agentResult(&hubclient.Agent{Phase: "provisioning", Launch: activeLaunch("awaiting_capacity", 2, nil)}),
		agentResult(&hubclient.Agent{Phase: "provisioning", Launch: activeLaunch("container_create", 2, nil)}),
		agentResult(&hubclient.Agent{Phase: "starting", Launch: activeLaunch("future_step", 2, nil)}),
		agentResult(&hubclient.Agent{Phase: "running"}),
	}}
	var out bytes.Buffer
	_, err := waitForAgentLaunch(context.Background(), launchWaitOptions{
		AgentName: "a1", Get: seq.get, PollInterval: time.Millisecond, Timeout: 3 * time.Second, Progress: &out,
	})
	require.NoError(t, err)
	assert.Equal(t, "  a1: provisioning (waiting to be scheduled)\n"+
		"  a1: provisioning (waiting for node capacity)\n"+
		"  a1: provisioning (preparing the container (image pull, volume mounts))\n"+
		"  a1: starting (future step)\n"+
		"  a1: running\n", out.String())
}

// TestWaitForAgentLaunch_FailureNamesCodeAndStepLabels: the failure text
// keeps the raw code first, adds its label (once) and the label of the step
// the launch was in. A stopped launch gets a neutral text instead.
func TestWaitForAgentLaunch_FailureNamesCodeAndStepLabels(t *testing.T) {
	shortenLaunchWaitTimings(t)
	for _, tc := range []struct {
		name   string
		launch hubclient.AgentLaunch
		msg    string
		want   string
	}{
		{
			name:   "code, step and message",
			launch: hubclient.AgentLaunch{Kind: "start", Error: "launch_timeout", Step: "awaiting_capacity"},
			msg:    "awaiting_capacity: timed out waiting for node capacity",
			want:   "agent 'a1' did not start (launch_timeout: the launch timed out) while waiting for node capacity: awaiting_capacity: timed out waiting for node capacity",
		},
		{
			name:   "typed failure: the message already carries the label",
			launch: hubclient.AgentLaunch{Kind: "start", Error: "image_pull_failed", Step: "container_create"},
			msg:    "container_create: the agent image could not be pulled (ImagePullBackOff)",
			want:   "agent 'a1' did not start (image_pull_failed) while preparing the container (image pull, volume mounts): container_create: the agent image could not be pulled (ImagePullBackOff)",
		},
		{
			name:   "preempted pod: the label stays a prefix of the message, printed once",
			launch: hubclient.AgentLaunch{Kind: "start", Error: "pod_disrupted", Step: "container_create"},
			msg:    "container_create: the agent pod was preempted, evicted or deleted while starting: it was preempted (PreemptionByScheduler)",
			want:   "agent 'a1' did not start (pod_disrupted) while preparing the container (image pull, volume mounts): container_create: the agent pod was preempted, evicted or deleted while starting: it was preempted (PreemptionByScheduler)",
		},
		{
			name:   "evicted pod: the label is printed once",
			launch: hubclient.AgentLaunch{Kind: "start", Error: "pod_disrupted", Step: "scheduling"},
			msg:    "scheduling: the agent pod was preempted, evicted or deleted while starting: it was evicted (Evicted)",
			want:   "agent 'a1' did not start (pod_disrupted) while waiting to be scheduled: scheduling: the agent pod was preempted, evicted or deleted while starting: it was evicted (Evicted)",
		},
		{
			name:   "runtime cause",
			launch: hubclient.AgentLaunch{Kind: "start", Error: "runtime_error", Cause: "runtime_forbidden", Step: "launching"},
			msg:    "launching: Failed to create agent",
			want:   "agent 'a1' did not start (runtime_error: the runtime could not start the agent, cause runtime_forbidden) while launching: launching: Failed to create agent",
		},
		{
			name:   "no step",
			launch: hubclient.AgentLaunch{Kind: "start", Error: "broker_lost"},
			want:   "agent 'a1' did not start (broker_lost: the runtime broker stopped responding)",
		},
		{
			name:   "unknown code and step",
			launch: hubclient.AgentLaunch{Kind: "start", Error: "future_code", Step: "future_step"},
			want:   "agent 'a1' did not start (future_code: future code) while future step",
		},
		{
			name:   "stopped start: neutral text",
			launch: hubclient.AgentLaunch{Kind: "start", Error: "launch_stopped", Step: "pod_create"},
			want:   "agent 'a1' did not start: the start was stopped (launch_stopped)",
		},
		{
			name:   "create",
			launch: hubclient.AgentLaunch{Kind: "create", Error: "crash_loop", Step: "container_start"},
			want:   "agent 'a1' create did not complete (crash_loop: the agent container keeps crashing on start) while starting the container",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := tc.launch
			l.ID, l.State = "l1", "ended"
			seq := &agentSequence{results: []func() (*hubclient.Agent, error){
				agentResult(&hubclient.Agent{Phase: "error", Message: tc.msg, Launch: &l}),
			}}
			_, err := waitForAgentLaunch(context.Background(), launchWaitOptions{
				AgentName: "a1", Get: seq.get, PollInterval: time.Millisecond, Timeout: 3 * time.Second,
			})
			var failed *launchFailedError
			require.ErrorAs(t, err, &failed)
			assert.Equal(t, tc.want, failed.Reason)
			if label := launchCodeText(tc.launch.Error); label != "" {
				assert.Equal(t, 1, strings.Count(failed.Reason, label), "the code label appears once: %q", failed.Reason)
			}
		})
	}
}

// launchCodeText is the label the failure tests expect for the codes they
// use (kept here, not read from the table under test).
func launchCodeText(code string) string {
	return map[string]string{
		"image_pull_failed": "the agent image could not be pulled",
		"launch_timeout":    "the launch timed out",
		"pod_disrupted":     "the agent pod was preempted, evicted or deleted while starting",
	}[code]
}

// TestWaitForAgentLaunch_EndedLaunchErrorEndsWaitInAnyPhase (D-T8, E-AC4):
// a launch that ended with an error ends the wait whatever phase the failed
// start left the agent in, at the first read, instead of waiting out the
// budget.
func TestWaitForAgentLaunch_EndedLaunchErrorEndsWaitInAnyPhase(t *testing.T) {
	shortenLaunchWaitTimings(t)
	for _, phase := range []string{"suspended", "starting", "created", "provisioning"} {
		t.Run(phase, func(t *testing.T) {
			seq := &agentSequence{results: []func() (*hubclient.Agent, error){
				agentResult(&hubclient.Agent{Phase: phase, Message: "container_start: the agent container keeps crashing on start (CrashLoopBackOff)",
					Launch: &hubclient.AgentLaunch{ID: "l2", State: "ended", Kind: "start", Error: "crash_loop", Step: "container_start", EndReason: "failed"}}),
			}}
			_, err := waitForAgentLaunch(context.Background(), launchWaitOptions{
				AgentName: "a1", Get: seq.get, PollInterval: time.Millisecond, Timeout: 300 * time.Millisecond,
			})
			var failed *launchFailedError
			require.ErrorAs(t, err, &failed, "the ended launch ends the wait (got %v)", err)
			assert.False(t, failed.Incomplete)
			assert.Equal(t, phase, failed.Phase)
			assert.Contains(t, err.Error(), "did not start (crash_loop) while starting the container")
			assert.Equal(t, 1, seq.calls, "ended at the first read")
		})
	}
}

// TestWaitForAgentLaunch_FailedEndReasonEndsWait: a launch that ended
// failed, timed_out or lost ends the wait even without an error code
// (as pkg/hubclient's wait does). not_launched does not: the Hub also ends a
// launch not_launched when a broker without async launch started the agent
// synchronously, and that start goes on. superseded does not either.
func TestWaitForAgentLaunch_FailedEndReasonEndsWait(t *testing.T) {
	shortenLaunchWaitTimings(t)
	for _, tc := range []struct {
		endReason string
		ends      bool
	}{
		{"failed", true},
		{"timed_out", true},
		{"lost", true},
		{"not_launched", false},
		{"superseded", false},
	} {
		t.Run(tc.endReason, func(t *testing.T) {
			ended := &hubclient.AgentLaunch{ID: "l2", State: "ended", Kind: "start", EndReason: tc.endReason}
			seq := &agentSequence{results: []func() (*hubclient.Agent, error){
				agentResult(&hubclient.Agent{Phase: "starting", Launch: ended}),
				agentResult(&hubclient.Agent{ID: "id-1", Phase: "running"}),
			}}
			_, err := waitForAgentLaunch(context.Background(), launchWaitOptions{
				AgentName: "a1", Get: seq.get, PollInterval: time.Millisecond, Timeout: 3 * time.Second,
			})
			if !tc.ends {
				require.NoError(t, err, "end reason %s keeps the wait going", tc.endReason)
				assert.Equal(t, 2, seq.calls)
				return
			}
			var failed *launchFailedError
			require.ErrorAs(t, err, &failed, "end reason %s ends the wait (got %v)", tc.endReason, err)
			assert.Equal(t, "agent 'a1' failed to start (phase: starting)", failed.Reason)
			assert.Equal(t, 1, seq.calls, "ended at the first read")
		})
	}
}

// TestStartAgentViaHub_AsyncResumeOutput (D-T8): `scion resume` through a
// Hub that answers with an accepted start launch prints the step labels
// while waiting, and on success the resumed line; a failed start launch
// fails with the code and step labels and the logs hint, not the recreate
// hint of an incomplete create.
func TestStartAgentViaHub_AsyncResumeOutput(t *testing.T) {
	startLaunch := func(step string) *hubclient.AgentLaunch {
		l := activeLaunch(step, 2, nil)
		l.ID, l.Kind = "launch-s", "start"
		return l
	}
	created := hubclient.CreateAgentResponse{Agent: &hubclient.Agent{ID: "id-1", Slug: "a1", Name: "a1", Phase: "starting", Launch: startLaunch("")}}

	t.Run("success", func(t *testing.T) {
		hub := &launchMockHub{t: t, createStatus: http.StatusOK, createBody: created,
			afterCreate: []interface{}{
				hubclient.Agent{ID: "id-1", Slug: "a1", Phase: "starting", Launch: startLaunch("container_create")},
				hubclient.Agent{ID: "id-1", Slug: "a1", Phase: "running", Activity: "working"},
			}}
		hubCtx := setupLaunchStartTest(t, hub)
		var err error
		var stdout string
		stderr := captureStderr(t, func() {
			stdout = captureStdout(t, func() { err = startAgentViaHub(nil, hubCtx, "a1", "", true, nil) })
		})
		require.NoError(t, err)
		require.Len(t, hub.createReqs, 1)
		assert.Equal(t, true, hub.createReqs[0]["resume"])
		assert.Contains(t, stdout, "Resuming agent 'a1'...")
		assert.Contains(t, stderr, "  a1: starting (preparing the container (image pull, volume mounts))\n")
		assert.Contains(t, stderr, "Agent 'a1' resumed via Hub.")
	})

	t.Run("failure", func(t *testing.T) {
		hub := &launchMockHub{t: t, createStatus: http.StatusOK, createBody: created,
			afterCreate: []interface{}{
				hubclient.Agent{ID: "id-1", Slug: "a1", Phase: "starting", Launch: startLaunch("container_start")},
				hubclient.Agent{ID: "id-1", Slug: "a1", Phase: "error", Template: "claude",
					Message: "container_start: the agent container keeps crashing on start (CrashLoopBackOff)",
					Launch:  &hubclient.AgentLaunch{ID: "launch-s", State: "ended", Kind: "start", Error: "crash_loop", Step: "container_start", EndReason: "failed"}},
			}}
		hubCtx := setupLaunchStartTest(t, hub)
		var err error
		stderr := captureStderr(t, func() {
			_ = captureStdout(t, func() { err = startAgentViaHub(nil, hubCtx, "a1", "", true, nil) })
		})
		var failed *launchFailedError
		require.ErrorAs(t, err, &failed)
		assert.False(t, failed.Incomplete)
		assert.Contains(t, stderr, "  a1: starting (starting the container)\n")
		assert.Equal(t, "agent 'a1' did not start (crash_loop) while starting the container: "+
			"container_start: the agent container keeps crashing on start (CrashLoopBackOff)\n\n"+
			"Check the agent logs with: scion logs a1", err.Error())
	})
}
