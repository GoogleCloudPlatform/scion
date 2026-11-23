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

package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
)

type admissionFakeAdapter struct {
	name   string
	invoke func(ctx context.Context, t provider.Transport, inst provider.InstanceView, req provider.ModelRequest) (provider.ModelResult, error)
}

func (a *admissionFakeAdapter) Name() string    { return a.name }
func (a *admissionFakeAdapter) Version() string { return "v1-test" }
func (a *admissionFakeAdapter) Capabilities(provider.InstanceView) (provider.Capabilities, error) {
	return provider.Capabilities{
		Primitives: map[string]provider.PrimitiveLimits{
			spec.PrimitiveBinary: {Supported: true, DistributionCompleteness: provider.CompletenessComplete},
		},
		MaxQuestionsPerRequest: 4,
		MaxInputTokens:         4096,
		Truncation:             provider.TruncationNever,
		SideEffectFree:         true,
		AuthScheme:             provider.AuthScheme{Kind: provider.AuthKindNone},
	}, nil
}
func (a *admissionFakeAdapter) Validate(provider.InstanceView, spec.Spec) error { return nil }
func (a *admissionFakeAdapter) Invoke(ctx context.Context, t provider.Transport, inst provider.InstanceView, req provider.ModelRequest) (provider.ModelResult, error) {
	return a.invoke(ctx, t, inst, req)
}
func (a *admissionFakeAdapter) ListModels(_ context.Context, _ provider.Transport, _ provider.InstanceView) ([]string, error) {
	return []string{"model-a"}, nil
}

func compileAdmissionTestRule(t *testing.T, instanceName string) *spec.Compiled {
	t.Helper()
	raw := `{"version":1,"binding":{"instance":"` + instanceName + `"},"questions":[{"id":"q1","type":"binary","instructions":"Is this urgent?"}],"output":{"question":"q1"}}`
	if instanceName == "" {
		raw = `{"version":1,"questions":[{"id":"q1","type":"binary","instructions":"Is this urgent?"}],"output":{"question":"q1"}}`
	}
	compiled, err := spec.Compile([]byte(raw), spec.CompileOptions{Kind: spec.KindFilter})
	require.NoError(t, err)
	return compiled
}

func binarySuccessResult() provider.ModelResult {
	return provider.ModelResult{
		Answers: map[string]provider.Answer{
			"q1": {
				Status:       provider.AnswerStatusAnswered,
				Distribution: map[string]float64{"true": 0.9, "false": 0.1},
			},
		},
		ResolvedModel: "test-model-1",
		HTTPStatus:    200,
	}
}

func TestAdmission_InstanceMaxInFlightCapsConcurrencyAndReturnsCapacity(t *testing.T) {
	var (
		inFlight      atomic.Int32
		maxInFlight   atomic.Int32
		providerCalls atomic.Int32
	)
	started := make(chan struct{}, 10)
	release := make(chan struct{})

	adapter := &admissionFakeAdapter{
		name: "adm_fake",
		invoke: func(ctx context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			providerCalls.Add(1)
			cur := inFlight.Add(1)
			for {
				prev := maxInFlight.Load()
				if cur <= prev || maxInFlight.CompareAndSwap(prev, cur) {
					break
				}
			}
			started <- struct{}{}
			defer inFlight.Add(-1)
			select {
			case <-release:
				return binarySuccessResult(), nil
			case <-ctx.Done():
				return provider.ModelResult{}, ctx.Err()
			}
		},
	}
	reg := provider.NewRegistry()
	require.NoError(t, reg.Register(adapter))

	inst := Instance{
		ID:      "inst-1",
		Name:    "limited-inst",
		Adapter: "adm_fake",
		Model:   "test-model-1",
		Auth:    InstanceAuth{Kind: "none"},
		Limits:  InstanceLimits{MaxInFlight: 2},
		Enabled: true,
	}

	rt := New(Config{
		Registry:              reg,
		Instances:             StaticInstances(inst),
		AdmissionWait:         60 * time.Millisecond,
		MaxInFlightPerProject: -1, // isolate instance limit
	})

	compiled := compileAdmissionTestRule(t, "limited-inst")

	// Start 2 requests that occupy all 2 instance permits.
	var wg sync.WaitGroup
	holderErrs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, err := rt.Evaluate(context.Background(), compiled, ProbeInput("hello"), EvalOptions{})
			holderErrs[idx] = err
		}(i)
	}
	for i := 0; i < 2; i++ {
		<-started
	}

	// Launch 3 excess requests concurrently; all 3 must fail with capacity after ~admissionWait
	// and make 0 provider requests.
	type excessResult struct {
		err     error
		elapsed time.Duration
	}
	excessResults := make([]excessResult, 3)
	var excessWG sync.WaitGroup
	for i := 0; i < 3; i++ {
		excessWG.Add(1)
		go func(idx int) {
			defer excessWG.Done()
			t0 := time.Now()
			_, err := rt.Evaluate(context.Background(), compiled, ProbeInput("overflow"), EvalOptions{})
			excessResults[idx] = excessResult{err: err, elapsed: time.Since(t0)}
		}(i)
	}
	excessWG.Wait()

	close(release)
	wg.Wait()

	for i, err := range holderErrs {
		assert.NoError(t, err, "holder %d should succeed", i)
	}
	for i, r := range excessResults {
		require.Error(t, r.err, "excess %d should fail", i)
		eval, ok := EvaluationFrom(r.err)
		require.True(t, ok)
		assert.Equal(t, ClassFailed, eval.Class)
		assert.Equal(t, string(provider.FailCapacity), eval.Reason)
		require.Len(t, eval.Attempts, 1)
		assert.Equal(t, 0, eval.Attempts[0].Requests, "rejected evaluation must record 0 provider requests")
		assert.Equal(t, string(provider.FailCapacity), eval.Attempts[0].Reason)
		assert.GreaterOrEqual(t, r.elapsed, 45*time.Millisecond, "should wait up to admissionWait")
		assert.Less(t, r.elapsed, 400*time.Millisecond, "should not wait far beyond admissionWait")
	}
	assert.Equal(t, int32(2), maxInFlight.Load(), "max observed in-flight must never exceed MaxInFlight")
	assert.Equal(t, int32(2), providerCalls.Load(), "only admitted calls may invoke the provider")
}

func TestAdmission_TokenBucketRatePerSec(t *testing.T) {
	var providerCalls atomic.Int32
	adapter := &admissionFakeAdapter{
		name: "adm_rate",
		invoke: func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			providerCalls.Add(1)
			return binarySuccessResult(), nil
		},
	}
	reg := provider.NewRegistry()
	require.NoError(t, reg.Register(adapter))

	// 1 req/s -> burst of 1, next token takes 1000ms (> admissionWait 40ms), so
	// the second immediate call fails fast with capacity and 0 provider calls.
	inst := Instance{
		ID:      "inst-rate",
		Name:    "rate-inst",
		Adapter: "adm_rate",
		Model:   "test-model-1",
		Auth:    InstanceAuth{Kind: "none"},
		Limits:  InstanceLimits{RatePerSec: 1},
		Enabled: true,
	}

	rt := New(Config{
		Registry:              reg,
		Instances:             StaticInstances(inst),
		AdmissionWait:         40 * time.Millisecond,
		MaxInFlightPerProject: -1,
	})
	compiled := compileAdmissionTestRule(t, "rate-inst")

	dec, err := rt.Evaluate(context.Background(), compiled, ProbeInput("first"), EvalOptions{})
	require.NoError(t, err)
	assert.True(t, dec.Bool)
	assert.Equal(t, int32(1), providerCalls.Load())

	t0 := time.Now()
	_, err = rt.Evaluate(context.Background(), compiled, ProbeInput("second"), EvalOptions{})
	elapsed := time.Since(t0)
	require.Error(t, err)
	eval, ok := EvaluationFrom(err)
	require.True(t, ok)
	assert.Equal(t, string(provider.FailCapacity), eval.Reason)
	require.Len(t, eval.Attempts, 1)
	assert.Equal(t, 0, eval.Attempts[0].Requests)
	assert.Equal(t, int32(1), providerCalls.Load(), "rate-rejected call must make 0 provider requests")
	assert.Less(t, elapsed, 40*time.Millisecond, "when required token wait exceeds admissionWait, fail fast without sleeping")

	// With a faster rate (50 req/s -> 20ms refill < 150ms admissionWait), a burst
	// of 50+1 succeeds by waiting for the next token.
	fastInst := Instance{
		ID:      "inst-rate-fast",
		Name:    "rate-fast",
		Adapter: "adm_rate",
		Model:   "test-model-1",
		Auth:    InstanceAuth{Kind: "none"},
		Limits:  InstanceLimits{RatePerSec: 20}, // burst 20, refill 50ms/token
		Enabled: true,
	}
	rtFast := New(Config{
		Registry:              reg,
		Instances:             StaticInstances(fastInst),
		AdmissionWait:         150 * time.Millisecond,
		MaxInFlightPerProject: -1,
	})
	compiledFast := compileAdmissionTestRule(t, "rate-fast")
	for i := 0; i < 21; i++ {
		_, err := rtFast.Evaluate(context.Background(), compiledFast, ProbeInput("ok"), EvalOptions{})
		require.NoError(t, err, "call %d should succeed within admissionWait", i)
	}
}

func TestAdmission_PerProjectSemaphoreIsolatesProjects(t *testing.T) {
	startedA := make(chan struct{}, 4)
	releaseA := make(chan struct{})
	var callsA, callsB atomic.Int32

	adapter := &admissionFakeAdapter{
		name: "adm_proj",
		invoke: func(ctx context.Context, _ provider.Transport, _ provider.InstanceView, req provider.ModelRequest) (provider.ModelResult, error) {
			if len(req.Input) > 0 && req.Input[0].Value == "proj-A" {
				callsA.Add(1)
				startedA <- struct{}{}
				select {
				case <-releaseA:
					return binarySuccessResult(), nil
				case <-ctx.Done():
					return provider.ModelResult{}, ctx.Err()
				}
			}
			callsB.Add(1)
			return binarySuccessResult(), nil
		},
	}
	reg := provider.NewRegistry()
	require.NoError(t, reg.Register(adapter))

	inst := Instance{
		ID:      "inst-shared",
		Name:    "shared-inst",
		Adapter: "adm_proj",
		Model:   "test-model-1",
		Auth:    InstanceAuth{Kind: "none"},
		Limits:  InstanceLimits{MaxInFlight: 8},
		Enabled: true,
	}

	rt := New(Config{
		Registry:              reg,
		Instances:             StaticInstances(inst),
		MaxInFlightPerProject: 2,
		AdmissionWait:         50 * time.Millisecond,
	})
	compiled := compileAdmissionTestRule(t, "shared-inst")

	// Saturate project A's limit (2 in-flight).
	var wgA sync.WaitGroup
	for i := 0; i < 2; i++ {
		wgA.Add(1)
		go func() {
			defer wgA.Done()
			_, err := rt.Evaluate(context.Background(), compiled, ProbeInput("proj-A"), EvalOptions{ProjectID: "proj-A"})
			assert.NoError(t, err)
		}()
	}
	<-startedA
	<-startedA

	// A 3rd call for project A fails with capacity (0 requests).
	_, errA3 := rt.Evaluate(context.Background(), compiled, ProbeInput("proj-A"), EvalOptions{ProjectID: "proj-A"})
	require.Error(t, errA3)
	evalA3, ok := EvaluationFrom(errA3)
	require.True(t, ok)
	assert.Equal(t, string(provider.FailCapacity), evalA3.Reason)
	require.Len(t, evalA3.Attempts, 1)
	assert.Equal(t, 0, evalA3.Attempts[0].Requests)

	// Meanwhile project B succeeds immediately and is not starved by project A.
	for i := 0; i < 3; i++ {
		decB, errB := rt.Evaluate(context.Background(), compiled, ProbeInput("proj-B"), EvalOptions{ProjectID: "proj-B"})
		require.NoError(t, errB)
		assert.True(t, decB.Bool)
	}
	assert.Equal(t, int32(3), callsB.Load())

	close(releaseA)
	wgA.Wait()
	assert.Equal(t, int32(2), callsA.Load())
}

func TestAdmission_WaitNeverExceedsAttemptDeadlineOrCallerCancel(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})

	adapter := &admissionFakeAdapter{
		name: "adm_dl",
		invoke: func(ctx context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			started <- struct{}{}
			select {
			case <-release:
				return binarySuccessResult(), nil
			case <-ctx.Done():
				return provider.ModelResult{}, ctx.Err()
			}
		},
	}
	reg := provider.NewRegistry()
	require.NoError(t, reg.Register(adapter))

	inst := Instance{
		ID:      "inst-dl",
		Name:    "dl-inst",
		Adapter: "adm_dl",
		Model:   "test-model-1",
		Auth:    InstanceAuth{Kind: "none"},
		Limits:  InstanceLimits{MaxInFlight: 1},
		Enabled: true,
	}

	// AdmissionWait is very long (2s), so attempt deadline / caller cancel must bound it.
	rt := New(Config{
		Registry:              reg,
		Instances:             StaticInstances(inst),
		AdmissionWait:         2 * time.Second,
		MaxInFlightPerProject: -1,
	})
	compiled := compileAdmissionTestRule(t, "dl-inst")

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = rt.Evaluate(context.Background(), compiled, ProbeInput("hold"), EvalOptions{})
	}()
	<-started

	// 1. Short attempt budget (50ms << 2s admissionWait) must return within budget.
	t0 := time.Now()
	_, err := rt.Evaluate(context.Background(), compiled, ProbeInput("short-budget"), EvalOptions{
		Budget: 50 * time.Millisecond,
	})
	elapsed := time.Since(t0)
	require.Error(t, err)
	eval, ok := EvaluationFrom(err)
	require.True(t, ok)
	assert.Equal(t, string(provider.FailCapacity), eval.Reason)
	require.Len(t, eval.Attempts, 1)
	assert.Equal(t, 0, eval.Attempts[0].Requests)
	assert.Less(t, elapsed, 400*time.Millisecond, "admission wait must never exceed attempt deadline")

	// 2. Caller cancel during admission wait returns FailCanceled immediately.
	cancelCtx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	t1 := time.Now()
	_, errCancel := rt.Evaluate(cancelCtx, compiled, ProbeInput("cancel-me"), EvalOptions{})
	cancelElapsed := time.Since(t1)
	require.Error(t, errCancel)
	evalCancel, ok := EvaluationFrom(errCancel)
	require.True(t, ok)
	assert.Equal(t, string(provider.FailCanceled), evalCancel.Reason)
	assert.Less(t, cancelElapsed, 400*time.Millisecond)

	close(release)
	wg.Wait()
}

func TestAdmission_RateLimitedRetryAfterBeyondRemainingFailsImmediately(t *testing.T) {
	adapter := &admissionFakeAdapter{
		name: "adm_429",
		invoke: func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			return provider.ModelResult{}, provider.NewProviderError(
				provider.FailRateLimited,
				true,
				10*time.Second, // far beyond remaining budget
				429,
				"rate limited",
			)
		},
	}
	reg := provider.NewRegistry()
	require.NoError(t, reg.Register(adapter))

	inst := Instance{
		ID:      "inst-429",
		Name:    "inst-429",
		Adapter: "adm_429",
		Model:   "test-model-1",
		Auth:    InstanceAuth{Kind: "none"},
		Enabled: true,
	}
	rt := New(Config{
		Registry:  reg,
		Instances: StaticInstances(inst),
	})
	compiled := compileAdmissionTestRule(t, "inst-429")

	t0 := time.Now()
	_, err := rt.Evaluate(context.Background(), compiled, ProbeInput("hello"), EvalOptions{
		Budget: 500 * time.Millisecond,
	})
	elapsed := time.Since(t0)
	require.Error(t, err)
	eval, ok := EvaluationFrom(err)
	require.True(t, ok)
	assert.Equal(t, string(provider.FailRateLimited), eval.Reason)
	assert.Less(t, elapsed, 200*time.Millisecond, "rate_limited with RetryAfter beyond remaining time must fail immediately without sleeping")
}

type mutableProjectAllowlist struct {
	mu      sync.RWMutex
	allowed map[string][]string
}

func (m *mutableProjectAllowlist) AllowedInstances(_ context.Context, projectID string) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := append([]string(nil), m.allowed[projectID]...)
	return out, nil
}

func (m *mutableProjectAllowlist) set(projectID string, names []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.allowed == nil {
		m.allowed = make(map[string][]string)
	}
	m.allowed[projectID] = append([]string(nil), names...)
}

func TestRuntime_ProjectAllowlistCompileEvaluateAndCredentialIndependence(t *testing.T) {
	var (
		providerCalls atomic.Int32
		credCalls     atomic.Int32
	)
	adapter := &admissionFakeAdapter{
		name: "adm_allow",
		invoke: func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			providerCalls.Add(1)
			return binarySuccessResult(), nil
		},
	}
	reg := provider.NewRegistry()
	require.NoError(t, reg.Register(adapter))

	inst := Instance{
		ID:      "inst-allow",
		Name:    "inst-a",
		Adapter: "adm_allow",
		Model:   "test-model-1",
		Auth:    InstanceAuth{Kind: "none"},
		Enabled: true,
	}
	allowlist := &mutableProjectAllowlist{}

	rt := New(Config{
		Registry:         reg,
		Instances:        StaticInstances(inst),
		ProjectAllowlist: allowlist,
		Credentials: func(context.Context, Instance) (string, error) {
			credCalls.Add(1)
			return "", errors.New("credential should not be read")
		},
	})
	compiled := compileAdmissionTestRule(t, "inst-a")

	// 1. Default empty allowlist: Compile with project context refuses with CategoryBinding.
	err := rt.ValidateCompiled(WithEvalProjectID(context.Background(), "proj-1"), compiled)
	require.Error(t, err)
	ce, ok := spec.AsCompileError(err)
	require.True(t, ok)
	assert.Equal(t, spec.CategoryBinding, ce.Category)
	assert.Equal(t, int32(0), credCalls.Load())

	// 2. Default empty allowlist: Evaluate returns FailNoEligibleInstance with 0 requests and 0 cred reads.
	_, err = rt.Evaluate(context.Background(), compiled, ProbeInput("hello"), EvalOptions{ProjectID: "proj-1"})
	require.Error(t, err)
	eval, ok := EvaluationFrom(err)
	require.True(t, ok)
	assert.Equal(t, string(provider.FailNoEligibleInstance), eval.Reason)
	assert.Equal(t, int32(0), providerCalls.Load())
	assert.Equal(t, int32(0), credCalls.Load())

	// 3. RequireProject=true with empty ProjectID fails closed with FailNoEligibleInstance.
	_, err = rt.Evaluate(context.Background(), compiled, ProbeInput("hello"), EvalOptions{RequireProject: true})
	require.Error(t, err)
	evalMissing, ok := EvaluationFrom(err)
	require.True(t, ok)
	assert.Equal(t, string(provider.FailNoEligibleInstance), evalMissing.Reason)
	assert.Equal(t, int32(0), providerCalls.Load())
	assert.Equal(t, int32(0), credCalls.Load())

	// 4. ListModels with project context respects allowlist.
	_, err = rt.ListModels(WithEvalProjectID(context.Background(), "proj-1"), inst)
	require.Error(t, err)
	pe, ok := provider.AsProviderError(err)
	require.True(t, ok)
	assert.Equal(t, provider.FailNoEligibleInstance, pe.Reason)

	// 5. Add inst-a to proj-1's allowlist -> Compile, Evaluate, and ListModels succeed.
	allowlist.set("proj-1", []string{"inst-a"})
	require.NoError(t, rt.ValidateCompiled(WithEvalProjectID(context.Background(), "proj-1"), compiled))
	dec, err := rt.Evaluate(context.Background(), compiled, ProbeInput("hello"), EvalOptions{ProjectID: "proj-1"})
	require.NoError(t, err)
	assert.True(t, dec.Bool)
	assert.Equal(t, int32(1), providerCalls.Load())
	models, err := rt.ListModels(WithEvalProjectID(context.Background(), "proj-1"), inst)
	require.NoError(t, err)
	assert.Equal(t, []string{"model-a"}, models)

	// 6. Remove inst-a from proj-1's allowlist -> takes effect immediately on next evaluation.
	allowlist.set("proj-1", nil)
	_, err = rt.Evaluate(context.Background(), compiled, ProbeInput("hello"), EvalOptions{ProjectID: "proj-1"})
	require.Error(t, err)
	evalRemoved, ok := EvaluationFrom(err)
	require.True(t, ok)
	assert.Equal(t, string(provider.FailNoEligibleInstance), evalRemoved.Reason)
	assert.Equal(t, int32(1), providerCalls.Load(), "no additional provider request after removal")
}
