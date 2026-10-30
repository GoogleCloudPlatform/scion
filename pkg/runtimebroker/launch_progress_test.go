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

// Tests for the async launch progress hook and the typed failure
// classification (design wave 3 §3 E-b, E-d; ptone/scion#4670 phase E1b).
package runtimebroker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// stepReports returns the progress reports that carry a step (the progress
// poster's posts), in order. Keepalives are progress reports without one.
func stepReports(rtb *mockRuntimeBrokerService) []*hubclient.AgentLaunchReport {
	var out []*hubclient.AgentLaunchReport
	for _, r := range rtb.getLaunchReports() {
		if r.Report.State == hubclient.AgentLaunchReportStateProgress && r.Report.Step != "" {
			out = append(out, r.Report)
		}
	}
	return out
}

// keepaliveReports returns the progress reports without a step.
func keepaliveReports(rtb *mockRuntimeBrokerService) []*hubclient.AgentLaunchReport {
	var out []*hubclient.AgentLaunchReport
	for _, r := range rtb.getLaunchReports() {
		if r.Report.State == hubclient.AgentLaunchReportStateProgress && r.Report.Step == "" {
			out = append(out, r.Report)
		}
	}
	return out
}

// terminalReport returns the first succeeded or failed report, or nil.
func terminalReport(rtb *mockRuntimeBrokerService) *hubclient.AgentLaunchReport {
	for _, r := range rtb.getLaunchReports() {
		if r.Report.State == hubclient.AgentLaunchReportStateSucceeded || r.Report.State == hubclient.AgentLaunchReportStateFailed {
			return r.Report
		}
	}
	return nil
}

// TestLaunchSenderProgress_ReturnsWhilePosterStuck: Progress only records the
// step and wakes the poster without blocking, so it returns at once even
// while the poster is stuck in a post the Hub never answers.
func TestLaunchSenderProgress_ReturnsWhilePosterStuck(t *testing.T) {
	release := make(chan struct{})
	rtb := &mockRuntimeBrokerService{launchReportFunc: func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		if req.Step != "" {
			<-release
		}
		return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
	}}
	s := newTestLaunchSender(t, rtb, time.Hour)
	s.timings.progressInterval = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartProgress(ctx)
	defer s.WaitProgressStopped()
	defer s.StopKeepalive()
	defer close(release)

	s.Progress(api.LaunchStepScheduling, "waiting to be scheduled")
	if !waitUntil(t, 2*time.Second, func() bool { return len(stepReports(rtb)) == 1 }) {
		t.Fatal("expected the poster to start posting the first step")
	}

	// The poster is now stuck in that post.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			s.Progress(fmt.Sprintf("step-%d", i), "")
		}
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Progress blocked while the progress poster was stuck in a post")
	}
	if got := s.LastStep(); got != "step-49" {
		t.Fatalf("LastStep() = %q, want step-49", got)
	}
}

// TestLaunchSenderProgress_CoalescesLatestWins: steps recorded inside the
// progress interval are coalesced; only the latest is posted, one interval
// after the previous post, with a higher seq.
func TestLaunchSenderProgress_CoalescesLatestWins(t *testing.T) {
	const interval = 300 * time.Millisecond
	rtb := &mockRuntimeBrokerService{}
	s := newTestLaunchSender(t, rtb, time.Hour)
	s.timings.progressInterval = interval
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartProgress(ctx)
	defer s.WaitProgressStopped()
	defer s.StopKeepalive()

	s.Progress(api.LaunchStepScheduling, "waiting to be scheduled")
	if !waitUntil(t, 2*time.Second, func() bool { return len(stepReports(rtb)) == 1 }) {
		t.Fatal("expected the first step to be posted")
	}
	s.Progress(api.LaunchStepInitContainers, "running init containers")
	s.Progress(api.LaunchStepContainerCreate, "preparing the container (image pull, volume mounts)")
	if !waitUntil(t, 3*time.Second, func() bool { return len(stepReports(rtb)) >= 2 }) {
		t.Fatal("expected a second progress post")
	}
	time.Sleep(2 * interval) // nothing further may be posted

	got := stepReports(rtb)
	if len(got) != 2 || got[0].Step != api.LaunchStepScheduling || got[1].Step != api.LaunchStepContainerCreate {
		var steps []string
		for _, r := range got {
			steps = append(steps, r.Step)
		}
		t.Fatalf("posted steps = %v, want [scheduling container_create] (init_containers coalesced away)", steps)
	}
	if got[1].Message != "preparing the container (image pull, volume mounts)" {
		t.Fatalf("second post message = %q, want the latest step's message", got[1].Message)
	}
	if got[1].Seq <= got[0].Seq {
		t.Fatalf("seq did not advance: %d then %d", got[0].Seq, got[1].Seq)
	}
	if gap := got[1].At.Sub(got[0].At); gap < interval {
		t.Fatalf("second post %v after the first, want at least the %v interval", gap, interval)
	}
}

// TestLaunchSenderProgress_SameStepNotReposted: a step that has been posted
// is never posted again, however often the poster is woken.
func TestLaunchSenderProgress_SameStepNotReposted(t *testing.T) {
	rtb := &mockRuntimeBrokerService{}
	s := newTestLaunchSender(t, rtb, time.Hour)
	s.timings.progressInterval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartProgress(ctx)
	defer s.WaitProgressStopped()
	defer s.StopKeepalive()

	s.Progress(api.LaunchStepScheduling, "")
	if !waitUntil(t, 2*time.Second, func() bool { return len(stepReports(rtb)) == 1 }) {
		t.Fatal("expected the step to be posted")
	}
	for i := 0; i < 3; i++ {
		s.Progress(api.LaunchStepScheduling, "")
		s.signalProgress()
		time.Sleep(30 * time.Millisecond)
	}
	if n := len(stepReports(rtb)); n != 1 {
		t.Fatalf("got %d progress posts for one step, want 1", n)
	}
}

// TestLaunchSenderProgress_ConflictAnswerAborts: a progress post's answer
// goes through the keepalive's gate table, so a 409 ends the launch.
func TestLaunchSenderProgress_ConflictAnswerAborts(t *testing.T) {
	for _, c := range []struct {
		reason string
		want   gateAction
	}{
		{hubclient.AgentLaunchReportReasonStopped, gateAbortCleanup},
		{hubclient.AgentLaunchReportReasonSuperseded, gateAbortNoCleanup},
	} {
		t.Run(c.reason, func(t *testing.T) {
			rtb := &mockRuntimeBrokerService{launchReportFunc: func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
				if req.Step != "" {
					return &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Code: hubclient.AgentLaunchReportCodeStaleLaunch, Reason: c.reason}, nil
				}
				return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
			}}
			s := newTestLaunchSender(t, rtb, time.Hour)
			s.timings.progressInterval = time.Millisecond
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s.StartProgress(ctx)
			defer s.WaitProgressStopped()
			defer s.StopKeepalive()

			s.Progress(api.LaunchStepScheduling, "")
			if !waitUntil(t, 2*time.Second, s.IsAborted) {
				t.Fatalf("a 409 %s answer to a progress post did not end the launch", c.reason)
			}
			if out := s.LastAbortOutcome(); out == nil || out.action != c.want {
				t.Fatalf("abort outcome = %+v, want action %v", out, c.want)
			}
		})
	}
}

// TestLaunchSenderProgress_LandedPostResetsKeepalive: a progress post that
// lands restarts the keepalive interval from that post.
func TestLaunchSenderProgress_LandedPostResetsKeepalive(t *testing.T) {
	const keepalive = 400 * time.Millisecond
	rtb := &mockRuntimeBrokerService{}
	s := newTestLaunchSender(t, rtb, keepalive)
	s.timings.progressInterval = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	s.StartKeepalive(ctx)
	s.StartProgress(ctx)
	defer s.WaitKeepaliveStopped()
	defer s.WaitProgressStopped()
	defer s.StopKeepalive()

	time.Sleep(250 * time.Millisecond)
	s.Progress(api.LaunchStepScheduling, "")
	if !waitUntil(t, time.Second, func() bool { return len(stepReports(rtb)) == 1 }) {
		t.Fatal("expected the step to be posted")
	}
	// Without the reset the first keepalive goes at ~400ms; with it, at
	// ~650ms (400ms after the post landed at ~250ms).
	time.Sleep(time.Until(start.Add(550 * time.Millisecond)))
	if n := len(keepaliveReports(rtb)); n != 0 {
		t.Fatalf("got %d keepalive(s) within 550ms although a progress post landed at ~250ms", n)
	}
	if !waitUntil(t, 2*time.Second, func() bool { return len(keepaliveReports(rtb)) >= 1 }) {
		t.Fatal("the keepalive never resumed after the progress post")
	}
}

// startTestPoster starts s's progress poster with interval and registers
// its stop and wait as test cleanup.
func startTestPoster(t *testing.T, s *launchSender, interval time.Duration) {
	t.Helper()
	s.timings.progressInterval = interval
	ctx, cancel := context.WithCancel(context.Background())
	s.StartProgress(ctx)
	t.Cleanup(func() {
		s.StopKeepalive()
		s.WaitProgressStopped()
		cancel()
	})
}

// TestLaunchSenderProgress_DuplicateAnswerReposts: a progress post answered
// duplicate (a keepalive sent with the current seq reached the Hub first, so
// the step was not applied) is posted again with a higher seq.
func TestLaunchSenderProgress_DuplicateAnswerReposts(t *testing.T) {
	var mu sync.Mutex
	stepPosts := 0
	rtb := &mockRuntimeBrokerService{launchReportFunc: func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		mu.Lock()
		defer mu.Unlock()
		if req.Step != "" {
			stepPosts++
			if stepPosts == 1 {
				return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultDuplicate}, nil
			}
		}
		return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
	}}
	s := newTestLaunchSender(t, rtb, time.Hour)
	startTestPoster(t, s, 20*time.Millisecond)

	s.Progress(api.LaunchStepContainerCreate, "")
	if !waitUntil(t, 2*time.Second, func() bool { return len(stepReports(rtb)) >= 2 }) {
		t.Fatalf("a step answered duplicate was not posted again (posts: %d)", len(stepReports(rtb)))
	}
	time.Sleep(100 * time.Millisecond) // the applied repost is final
	got := stepReports(rtb)
	if len(got) != 2 || got[1].Step != api.LaunchStepContainerCreate || got[1].Seq <= got[0].Seq {
		t.Fatalf("posts = %+v, want container_create twice with a higher second seq", got)
	}
}

// TestLaunchSenderProgress_GateAnswers covers the rest of the gate table for
// a progress post's answer: 403 and 404 agent_launch_unknown end the launch
// with cleanup, 400 is logged and the launch continues, completed stops
// further posts, and no answer leaves the step to be posted again.
func TestLaunchSenderProgress_GateAnswers(t *testing.T) {
	answerOnce := func(first *hubclient.AgentLaunchReportResult, firstErr error) func(*hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		var mu sync.Mutex
		n := 0
		return func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
			mu.Lock()
			defer mu.Unlock()
			if req.Step != "" {
				n++
				if n == 1 {
					return first, firstErr
				}
			}
			return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
		}
	}
	for _, c := range []struct {
		name   string
		result *hubclient.AgentLaunchReportResult
	}{
		{"403", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusForbidden}},
		{"404 unknown launch", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusNotFound, Code: hubclient.AgentLaunchReportCodeUnknownLaunch}},
	} {
		t.Run(c.name+" aborts with cleanup", func(t *testing.T) {
			rtb := &mockRuntimeBrokerService{launchReportFunc: answerOnce(c.result, nil)}
			s := newTestLaunchSender(t, rtb, time.Hour)
			startTestPoster(t, s, time.Millisecond)
			s.Progress(api.LaunchStepScheduling, "")
			if !waitUntil(t, 2*time.Second, s.IsAborted) {
				t.Fatalf("a %s answer to a progress post did not end the launch", c.name)
			}
			if out := s.LastAbortOutcome(); out == nil || out.action != gateAbortCleanup {
				t.Fatalf("abort outcome = %+v, want gateAbortCleanup", out)
			}
		})
	}

	t.Run("400 continues", func(t *testing.T) {
		rtb := &mockRuntimeBrokerService{launchReportFunc: answerOnce(&hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusBadRequest}, nil)}
		s := newTestLaunchSender(t, rtb, time.Hour)
		startTestPoster(t, s, time.Millisecond)
		s.Progress(api.LaunchStepScheduling, "")
		if !waitUntil(t, 2*time.Second, func() bool { return len(stepReports(rtb)) == 1 }) {
			t.Fatal("expected the step to be posted")
		}
		s.Progress(api.LaunchStepContainerCreate, "")
		if !waitUntil(t, 2*time.Second, func() bool { return len(stepReports(rtb)) == 2 }) {
			t.Fatal("the poster stopped posting after a 400 answer")
		}
		if s.IsAborted() || s.IsCompleted() {
			t.Fatalf("a 400 answer ended the launch (aborted %v, completed %v)", s.IsAborted(), s.IsCompleted())
		}
	})

	t.Run("completed stops posting", func(t *testing.T) {
		rtb := &mockRuntimeBrokerService{launchReportFunc: answerOnce(&hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultCompleted}, nil)}
		s := newTestLaunchSender(t, rtb, time.Hour)
		startTestPoster(t, s, time.Millisecond)
		s.Progress(api.LaunchStepScheduling, "")
		if !waitUntil(t, 2*time.Second, s.IsCompleted) {
			t.Fatal("a completed answer to a progress post was not recorded")
		}
		s.Progress(api.LaunchStepContainerCreate, "")
		time.Sleep(100 * time.Millisecond)
		if n := len(stepReports(rtb)); n != 1 {
			t.Fatalf("got %d progress posts, want 1: nothing is posted once the launch completed", n)
		}
		if s.IsAborted() {
			t.Fatal("a completed answer must not end the launch as aborted")
		}
	})

	t.Run("no answer is posted again", func(t *testing.T) {
		rtb := &mockRuntimeBrokerService{launchReportFunc: answerOnce(nil, errors.New("hub unreachable"))}
		s := newTestLaunchSender(t, rtb, time.Hour)
		startTestPoster(t, s, 20*time.Millisecond)
		s.Progress(api.LaunchStepScheduling, "")
		if !waitUntil(t, 2*time.Second, func() bool { return len(stepReports(rtb)) >= 2 }) {
			t.Fatalf("a step whose post got no answer was not posted again (posts: %d)", len(stepReports(rtb)))
		}
		got := stepReports(rtb)
		if got[1].Step != api.LaunchStepScheduling || got[1].Seq <= got[0].Seq {
			t.Fatalf("repost = %+v, want scheduling with a higher seq", got[1])
		}
	})
}

// TestLaunchSenderProgress_CheckpointSupersedesPendingStep: a checkpoint is
// a newer step than a coalesced progress step still waiting for its post, so
// that progress step is dropped instead of moving the Hub's step back.
func TestLaunchSenderProgress_CheckpointSupersedesPendingStep(t *testing.T) {
	rtb := &mockRuntimeBrokerService{}
	s := newTestLaunchSender(t, rtb, time.Hour)
	startTestPoster(t, s, 300*time.Millisecond)

	s.Progress(api.LaunchStepScheduling, "")
	if !waitUntil(t, 2*time.Second, func() bool { return len(stepReports(rtb)) == 1 }) {
		t.Fatal("expected the first step to be posted")
	}
	s.Progress(api.LaunchStepContainerCreate, "") // waits out the interval
	if err := s.Checkpoint(context.Background(), api.LaunchStepPodCreate); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	time.Sleep(600 * time.Millisecond)
	for _, r := range stepReports(rtb)[1:] {
		t.Fatalf("progress step %q was posted after the newer pod_create checkpoint", r.Step)
	}
	if got := s.LastStep(); got != api.LaunchStepPodCreate {
		t.Fatalf("LastStep() = %q, want pod_create", got)
	}
}

// TestRunLaunch_ProgressPosterStoppedBeforeReturn: runLaunch stops the
// progress poster, cancels a post still in flight, and waits for the poster
// to exit before it returns.
func TestRunLaunch_ProgressPosterStoppedBeforeReturn(t *testing.T) {
	var posterExited atomic.Bool
	postBlocked := make(chan struct{})
	start := time.Now()
	rtb, _ := runTestLaunch(t, "poster-stopped", func(ctx context.Context, opts api.StartOptions, env *launchTestEnv) (*api.AgentInfo, error) {
		var once sync.Once
		env.rtb.mu.Lock()
		env.rtb.ctxHook = func(postCtx context.Context) {
			reps := env.rtb.getLaunchReports()
			if last := reps[len(reps)-1].Report; last.State != hubclient.AgentLaunchReportStateProgress || last.Step == "" {
				return
			}
			once.Do(func() { close(postBlocked) })
			// The step post stays in flight until it is cancelled, and then
			// a little longer, so a runLaunch that does not wait for the
			// poster returns first.
			<-postCtx.Done()
			time.Sleep(300 * time.Millisecond)
			posterExited.Store(true)
		}
		env.rtb.mu.Unlock()
		opts.Progress(api.LaunchStepContainerCreate, "")
		select {
		case <-postBlocked:
		case <-time.After(2 * time.Second):
			return nil, errors.New("the progress post never started")
		}
		return &api.AgentInfo{ID: "c1", Name: opts.Name, Phase: "running"}, nil
	}, nil, nil)
	elapsed := time.Since(start)

	if term := terminalReport(rtb); term == nil || term.State != hubclient.AgentLaunchReportStateSucceeded {
		t.Fatalf("terminal = %+v, want succeeded", term)
	}
	if !posterExited.Load() {
		t.Fatal("runLaunch returned while its progress poster was still in a post")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("runLaunch took %v: a progress post in flight was not cancelled when the launch ended", elapsed)
	}
}

// launchFuncManager is an asyncManager whose Start runs start.
type launchFuncManager struct {
	*asyncManager
	start func(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error)
}

func (m *launchFuncManager) Start(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
	m.mu.Lock()
	m.startCalls++
	m.mu.Unlock()
	return m.start(ctx, opts)
}

// launchTestEnv is what a test Start can reach: the mock Hub, and stop,
// which cancels the launch as the broker's stop handler does
// (cancelLocalLaunch) before it deletes anything.
type launchTestEnv struct {
	rtb  *mockRuntimeBrokerService
	stop func()
}

// runTestLaunch runs one launch with mgr's Start to completion and returns
// the mock Hub. answer, when set, answers every report.
func runTestLaunch(t *testing.T, name string, start func(ctx context.Context, opts api.StartOptions, env *launchTestEnv) (*api.AgentInfo, error), answer func(*hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error), afterStartSelected func(*launchSender)) (*mockRuntimeBrokerService, *launchFuncManager) {
	t.Helper()
	mgr := &launchFuncManager{asyncManager: newAsyncManager()}
	srv, rtb := newAsyncTestServer(t, mgr)
	srv.launchTimingOverride = fastLaunchTimings(time.Hour)
	key := launchKey{Slug: "agent-" + name}
	env := &launchTestEnv{rtb: rtb, stop: func() { srv.cancelLocalLaunch(key, "agent-"+name) }}
	mgr.start = func(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
		return start(ctx, opts, env)
	}
	if answer == nil {
		answer = func(*hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
			return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
		}
	}
	rtb.launchReportFunc = answer

	ctx, cancel := context.WithCancel(context.Background())
	rec := newLaunchRecord("L-"+name, "agent-"+name, store.LaunchKindCreate, "", time.Now().Add(time.Hour), cancel)
	lc := launchCtx{
		opts:               api.StartOptions{Name: "agent-" + name},
		mgr:                mgr,
		key:                key,
		afterStartSelected: afterStartSelected,
	}
	srv.launchRegistry.Begin(lc.key, rec)
	done := make(chan struct{})
	go func() {
		srv.runLaunch(ctx, rec, lc)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runLaunch did not return")
	}
	return rtb, mgr
}

// podDisrupted is the error the Kubernetes runtime returns for a pod removed
// while the launch waited for it.
func podDisrupted() error {
	return &scionrt.LaunchFailure{Code: api.LaunchCodePodDisrupted, Step: api.LaunchStepContainerCreate, Reason: "",
		Err: errors.New(`pod "agent-x" was deleted while starting — if you did not stop it, start the agent again`)}
}

// TestRunLaunch_ProgressWiredIntoStartOptions: the runtime gets the
// sender's progress hook, and a step it reports reaches the Hub.
func TestRunLaunch_ProgressWiredIntoStartOptions(t *testing.T) {
	rtb, _ := runTestLaunch(t, "progress-wired", func(ctx context.Context, opts api.StartOptions, env *launchTestEnv) (*api.AgentInfo, error) {
		if opts.Progress == nil {
			return nil, errors.New("no progress hook")
		}
		opts.Progress(api.LaunchStepContainerCreate, "preparing the container (image pull, volume mounts)")
		waitUntil(t, 2*time.Second, func() bool { return len(stepReports(env.rtb)) >= 1 })
		return &api.AgentInfo{ID: "c1", Name: opts.Name, Phase: "running"}, nil
	}, nil, nil)

	got := stepReports(rtb)
	if len(got) != 1 || got[0].Step != api.LaunchStepContainerCreate {
		t.Fatalf("progress posts = %+v, want one container_create post", got)
	}
	if term := terminalReport(rtb); term == nil || term.State != hubclient.AgentLaunchReportStateSucceeded {
		t.Fatalf("terminal = %+v, want succeeded", term)
	}
}

// TestRunLaunch_FailureStepIsLastStep: an untyped Start failure is reported
// in the last step the launch reached, from Progress or Checkpoint.
func TestRunLaunch_FailureStepIsLastStep(t *testing.T) {
	t.Run("progress", func(t *testing.T) {
		rtb, _ := runTestLaunch(t, "fail-step-progress", func(ctx context.Context, opts api.StartOptions, _ *launchTestEnv) (*api.AgentInfo, error) {
			opts.Progress(api.LaunchStepContainerCreate, "")
			return nil, errors.New("boom")
		}, nil, nil)
		term := terminalReport(rtb)
		if term == nil || term.State != hubclient.AgentLaunchReportStateFailed {
			t.Fatalf("terminal = %+v, want failed", term)
		}
		if term.Step != api.LaunchStepContainerCreate || term.ErrorCode != api.LaunchCodeRuntimeError {
			t.Fatalf("failed report step/code = %q/%q, want container_create/runtime_error", term.Step, term.ErrorCode)
		}
	})
	t.Run("checkpoint", func(t *testing.T) {
		rtb, _ := runTestLaunch(t, "fail-step-checkpoint", func(ctx context.Context, opts api.StartOptions, _ *launchTestEnv) (*api.AgentInfo, error) {
			if err := opts.Checkpoint(ctx, api.LaunchStepPodCreate); err != nil {
				return nil, err
			}
			return nil, errors.New("boom")
		}, nil, nil)
		term := terminalReport(rtb)
		if term == nil || term.Step != api.LaunchStepPodCreate {
			t.Fatalf("terminal = %+v, want a failed report in step pod_create", term)
		}
	})
}

// TestRunLaunch_TypedFailureEndToEnd: a typed runtime failure is reported
// with its code, its step and fixed text naming only the Kubernetes reason.
func TestRunLaunch_TypedFailureEndToEnd(t *testing.T) {
	raw := errors.New(`pod "agent-secret-pod" in namespace "ns-secret" failed: ImagePullBackOff: Back-off pulling image "gcr.io/secret-proj/secret-image:v1"`)
	rtb, _ := runTestLaunch(t, "typed-failure", func(ctx context.Context, opts api.StartOptions, _ *launchTestEnv) (*api.AgentInfo, error) {
		opts.Progress(api.LaunchStepScheduling, "")
		return nil, &scionrt.LaunchFailure{Code: api.LaunchCodeImagePullFailed, Step: api.LaunchStepContainerCreate, Reason: "ImagePullBackOff", Err: raw}
	}, nil, nil)
	term := terminalReport(rtb)
	if term == nil || term.State != hubclient.AgentLaunchReportStateFailed {
		t.Fatalf("terminal = %+v, want failed", term)
	}
	if term.ErrorCode != api.LaunchCodeImagePullFailed || term.Step != api.LaunchStepContainerCreate {
		t.Fatalf("code/step = %q/%q, want image_pull_failed/container_create", term.ErrorCode, term.Step)
	}
	if want := "the agent image could not be pulled (ImagePullBackOff)"; term.Message != want {
		t.Fatalf("message = %q, want %q", term.Message, want)
	}
}

// TestRunLaunch_LocalStopBeatsPodDisrupted: a local stop cancels the launch
// before it deletes the pod, so the pod_disrupted error Start then returns
// is never reported.
func TestRunLaunch_LocalStopBeatsPodDisrupted(t *testing.T) {
	rtb, _ := runTestLaunch(t, "stop-beats-disrupted", func(ctx context.Context, opts api.StartOptions, env *launchTestEnv) (*api.AgentInfo, error) {
		opts.Progress(api.LaunchStepContainerCreate, "")
		env.stop() // the stop handler cancels the launch, then deletes the pod
		return nil, podDisrupted()
	}, nil, nil)
	assertNoTerminal(t, rtb, "a launch stopped locally whose pod was then deleted")
}

// TestRunLaunch_SupersedeBeatsPodDisrupted: when the Hub answers a progress
// post 409 superseded and the pod then disappears, the launch sends no
// terminal and cleans up nothing (the superseding launch owns the names).
func TestRunLaunch_SupersedeBeatsPodDisrupted(t *testing.T) {
	t.Run("answer to a progress post", func(t *testing.T) {
		rtb, mgr := runTestLaunch(t, "supersede-progress", func(ctx context.Context, opts api.StartOptions, _ *launchTestEnv) (*api.AgentInfo, error) {
			opts.Progress(api.LaunchStepContainerCreate, "")
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
			return nil, podDisrupted()
		}, func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
			if req.Step != "" && req.State == hubclient.AgentLaunchReportStateProgress {
				return &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Code: hubclient.AgentLaunchReportCodeStaleLaunch, Reason: hubclient.AgentLaunchReportReasonSuperseded}, nil
			}
			return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
		}, nil)
		assertNoTerminal(t, rtb, "a superseded launch whose pod was then deleted")
		if n := mgr.CleanupCallCount(); n != 0 {
			t.Fatalf("a superseded launch cleaned up %d time(s), want 0", n)
		}
	})
	t.Run("abort recorded as Start returns", func(t *testing.T) {
		rtb, mgr := runTestLaunch(t, "supersede-after-select", func(ctx context.Context, opts api.StartOptions, _ *launchTestEnv) (*api.AgentInfo, error) {
			return nil, podDisrupted()
		}, nil, func(sender *launchSender) {
			sender.recordKeepaliveAbort(gateAbortNoCleanup, &hubclient.AgentLaunchReportResult{
				HTTPStatus: http.StatusConflict, Code: hubclient.AgentLaunchReportCodeStaleLaunch, Reason: hubclient.AgentLaunchReportReasonSuperseded,
			})
		})
		assertNoTerminal(t, rtb, "a launch superseded as its Start returned pod_disrupted")
		if n := mgr.CleanupCallCount(); n != 0 {
			t.Fatalf("a superseded launch cleaned up %d time(s), want 0", n)
		}
	})
}

// TestClassifyStartError_LaunchFailure covers every launch code a runtime
// can type: the code, the step, and fixed text carrying only the reason
// enum, never the error's own text.
func TestClassifyStartError_LaunchFailure(t *testing.T) {
	raw := errors.New(`pod "agent-secret-pod" in namespace "ns-secret": image "gcr.io/secret-proj/secret-image:v1"`)
	for _, c := range []struct {
		code, step, reason, want string
	}{
		{api.LaunchCodeImagePullFailed, api.LaunchStepContainerCreate, "ErrImagePull", "the agent image could not be pulled (ErrImagePull)"},
		{api.LaunchCodeInvalidImage, api.LaunchStepContainerCreate, "InvalidImageName", "the agent image reference is not valid (InvalidImageName)"},
		{api.LaunchCodeContainerConfig, api.LaunchStepContainerCreate, "CreateContainerConfigError", "the agent container could not be configured (CreateContainerConfigError)"},
		{api.LaunchCodeCrashLoop, api.LaunchStepContainerStart, "CrashLoopBackOff", "the agent container keeps crashing on start (CrashLoopBackOff)"},
		{api.LaunchCodeInitContainerFailed, api.LaunchStepInitContainers, "Error", "an init container of the agent pod failed (Error)"},
		{api.LaunchCodeUnschedulable, api.LaunchStepScheduling, "Unschedulable", "the agent pod cannot be scheduled (Unschedulable)"},
		{api.LaunchCodePodDisrupted, api.LaunchStepScheduling, "PreemptionByScheduler", "the agent pod was preempted, evicted or deleted while starting: it was preempted (PreemptionByScheduler)"},
		{api.LaunchCodePodDisrupted, api.LaunchStepContainerCreate, "Evicted", "the agent pod was preempted, evicted or deleted while starting: it was evicted (Evicted)"},
		{api.LaunchCodePodDisrupted, api.LaunchStepContainerCreate, "TerminationByKubelet", "the agent pod was preempted, evicted or deleted while starting: it was evicted (TerminationByKubelet)"},
		{api.LaunchCodePodDisrupted, api.LaunchStepContainerCreate, "EvictionByEvictionAPI", "the agent pod was preempted, evicted or deleted while starting: it was evicted (EvictionByEvictionAPI)"},
		// A pod deleted without a disruption reason keeps the plain text.
		{api.LaunchCodePodDisrupted, api.LaunchStepContainerCreate, "", "the agent pod was preempted, evicted or deleted while starting"},
		// A non-enum reason is dropped whole, so no preempted/evicted suffix.
		{api.LaunchCodePodDisrupted, api.LaunchStepContainerCreate, "preempted by ns-secret/agent-secret-pod", "the agent pod was preempted, evicted or deleted while starting"},
		{api.LaunchCodeLaunchTimeout, api.LaunchStepAwaitingCapacity, "", "launch timed out waiting for the agent pod to be ready"},
		// A reason that is not an enum (free text) is never reported.
		{api.LaunchCodeCrashLoop, api.LaunchStepContainerStart, "back-off restarting ns-secret/agent-secret-pod", "the agent container keeps crashing on start"},
	} {
		t.Run(c.code+"/"+c.reason, func(t *testing.T) {
			err := fmt.Errorf("start agent: %w", &scionrt.LaunchFailure{Code: c.code, Step: c.step, Reason: c.reason, Err: raw})
			code, step, msg := classifyStartError(context.Background(), err, "", api.LaunchStepScheduling, store.LaunchKindCreate)
			if code != c.code || step != c.step || msg != c.want {
				t.Fatalf("got (%q, %q, %q), want (%q, %q, %q)", code, step, msg, c.code, c.step, c.want)
			}
			for _, leak := range []string{"secret-image", "agent-secret-pod", "ns-secret"} {
				if strings.Contains(msg, leak) {
					t.Fatalf("message %q leaks %q", msg, leak)
				}
			}
		})
	}

	t.Run("unknown code is runtime_error", func(t *testing.T) {
		err := &scionrt.LaunchFailure{Code: "something_new", Step: api.LaunchStepContainerCreate, Err: raw}
		code, step, msg := classifyStartError(context.Background(), err, "", api.LaunchStepScheduling, store.LaunchKindCreate)
		if code != api.LaunchCodeRuntimeError || step != api.LaunchStepScheduling || strings.Contains(msg, "secret") {
			t.Fatalf("got (%q, %q, %q), want runtime_error in scheduling with fixed text", code, step, msg)
		}
	})
	t.Run("no step falls back to launching", func(t *testing.T) {
		_, step, _ := classifyStartError(context.Background(), errors.New("boom"), "", "", store.LaunchKindCreate)
		if step != api.LaunchStepLaunching {
			t.Fatalf("step = %q, want launching", step)
		}
	})
}

// TestClassifyStartError_TimeoutInAwaitingCapacity: a launch that runs out
// of time is launch_timeout whatever Start returned, in the last step it
// reached, and says so for a GKE launch waiting for capacity (OQ-E2).
func TestClassifyStartError_TimeoutInAwaitingCapacity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	<-ctx.Done()

	code, step, msg := classifyStartError(ctx, errors.New("context deadline exceeded"), "", api.LaunchStepAwaitingCapacity, store.LaunchKindCreate)
	if code != api.LaunchCodeLaunchTimeout || step != api.LaunchStepAwaitingCapacity || msg != "timed out waiting for node capacity" {
		t.Fatalf("got (%q, %q, %q), want (launch_timeout, awaiting_capacity, timed out waiting for node capacity)", code, step, msg)
	}

	typed := &scionrt.LaunchFailure{Code: api.LaunchCodeImagePullFailed, Step: api.LaunchStepContainerCreate, Reason: "ImagePullBackOff", Err: errors.New("x")}
	code, step, msg = classifyStartError(ctx, typed, "", api.LaunchStepContainerCreate, store.LaunchKindCreate)
	if code != api.LaunchCodeLaunchTimeout || step != api.LaunchStepContainerCreate || msg != "launch timed out before the agent started" {
		t.Fatalf("typed failure under an expired ctx: got (%q, %q, %q), want launch_timeout in container_create", code, step, msg)
	}
}
