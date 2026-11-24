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
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/systemone/egress"
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
		SupportedAuth: map[string]provider.AuthScheme{
			"none":    {Kind: provider.AuthKindNone},
			"api_key": {Kind: provider.AuthKindBearer},
		},
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

func TestAdmission_SemaphoreAdmitsWaiterWhenPermitFreedWithinAdmissionWait(t *testing.T) {
	var providerCalls atomic.Int32
	started := make(chan struct{}, 2)
	release := make(chan struct{})

	adapter := &admissionFakeAdapter{
		name: "adm_handoff",
		invoke: func(ctx context.Context, _ provider.Transport, _ provider.InstanceView, req provider.ModelRequest) (provider.ModelResult, error) {
			providerCalls.Add(1)
			if len(req.Input) > 0 && req.Input[0].Value == "holder" {
				started <- struct{}{}
				select {
				case <-release:
					return binarySuccessResult(), nil
				case <-ctx.Done():
					return provider.ModelResult{}, ctx.Err()
				}
			}
			return binarySuccessResult(), nil
		},
	}
	reg := provider.NewRegistry()
	require.NoError(t, reg.Register(adapter))

	inst := Instance{
		ID:      "inst-handoff",
		Name:    "handoff-inst",
		Adapter: "adm_handoff",
		Model:   "test-model-1",
		Auth:    InstanceAuth{Kind: "none"},
		Limits:  InstanceLimits{MaxInFlight: 1},
		Enabled: true,
	}
	rt := New(Config{
		Registry:              reg,
		Instances:             StaticInstances(inst),
		AdmissionWait:         200 * time.Millisecond,
		MaxInFlightPerProject: -1,
	})
	compiled := compileAdmissionTestRule(t, "handoff-inst")

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := rt.Evaluate(context.Background(), compiled, ProbeInput("holder"), EvalOptions{})
		assert.NoError(t, err)
	}()
	<-started

	// Release the holder ~25ms into the waiter's 200ms admissionWait; waiter must be admitted.
	go func() {
		time.Sleep(25 * time.Millisecond)
		close(release)
	}()

	dec, err := rt.Evaluate(context.Background(), compiled, ProbeInput("waiter"), EvalOptions{})
	require.NoError(t, err, "waiter should be admitted once holder releases permit within admissionWait")
	assert.True(t, dec.Bool)
	require.Len(t, dec.Eval.Attempts, 1)
	assert.Equal(t, 1, dec.Eval.Attempts[0].Requests)
	wg.Wait()
	assert.Equal(t, int32(2), providerCalls.Load())
}

func TestAdmission_TokenBucketTinyRateDoesNotOverflow(t *testing.T) {
	var providerCalls atomic.Int32
	adapter := &admissionFakeAdapter{
		name: "adm_tiny_rate",
		invoke: func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			providerCalls.Add(1)
			return binarySuccessResult(), nil
		},
	}
	reg := provider.NewRegistry()
	require.NoError(t, reg.Register(adapter))

	inst := Instance{
		ID:      "inst-tiny-rate",
		Name:    "tiny-rate-inst",
		Adapter: "adm_tiny_rate",
		Model:   "test-model-1",
		Auth:    InstanceAuth{Kind: "none"},
		Limits:  InstanceLimits{RatePerSec: 1e-11}, // would overflow time.Duration int64 if converted before comparing
		Enabled: true,
	}
	rt := New(Config{
		Registry:              reg,
		Instances:             StaticInstances(inst),
		AdmissionWait:         50 * time.Millisecond,
		MaxInFlightPerProject: -1,
	})
	compiled := compileAdmissionTestRule(t, "tiny-rate-inst")

	// First call consumes burst=1.
	dec, err := rt.Evaluate(context.Background(), compiled, ProbeInput("first"), EvalOptions{})
	require.NoError(t, err)
	assert.True(t, dec.Bool)
	assert.Equal(t, int32(1), providerCalls.Load())

	// Second call must fail fast with capacity (0 provider calls), never fail-open.
	t0 := time.Now()
	_, err = rt.Evaluate(context.Background(), compiled, ProbeInput("second"), EvalOptions{})
	elapsed := time.Since(t0)
	require.Error(t, err)
	eval, ok := EvaluationFrom(err)
	require.True(t, ok)
	assert.Equal(t, string(provider.FailCapacity), eval.Reason)
	assert.Equal(t, int32(1), providerCalls.Load())
	assert.Less(t, elapsed, 40*time.Millisecond)
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
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
		assert.Equal(t, "Bearer sk-test-allow-key", r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(tlsSrv.Close)

	adapter := &admissionFakeAdapter{
		name: "adm_allow",
		invoke: func(ctx context.Context, tr provider.Transport, iv provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, iv.Endpoint+"/eval", nil)
			if err != nil {
				return provider.ModelResult{}, err
			}
			resp, err := tr.Do(httpReq)
			if err != nil {
				return provider.ModelResult{}, err
			}
			_ = resp.Body.Close()
			return binarySuccessResult(), nil
		},
	}
	reg := provider.NewRegistry()
	require.NoError(t, reg.Register(adapter))

	inst := Instance{
		ID:       "inst-allow",
		Name:     "inst-a",
		Adapter:  "adm_allow",
		Model:    "test-model-1",
		Endpoint: tlsSrv.URL,
		Auth:     InstanceAuth{Kind: "api_key", SecretName: "test-allow-secret"},
		Enabled:  true,
	}
	allowlist := &mutableProjectAllowlist{}

	rt := New(Config{
		Registry:         reg,
		Instances:        StaticInstances(inst),
		ProjectAllowlist: allowlist,
		Credentials: func(context.Context, Instance) (string, error) {
			credCalls.Add(1)
			return "sk-test-allow-key", nil
		},
	}, WithEgressOptionsForTesting(egress.WithLoopbackServerForTesting(tlsSrv.URL, tlsSrv.Client())))
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
	// Evaluate resolves the api_key credential exactly once (proving the 0-read
	// assertions above are non-vacuous).
	allowlist.set("proj-1", []string{"inst-a"})
	require.NoError(t, rt.ValidateCompiled(WithEvalProjectID(context.Background(), "proj-1"), compiled))
	assert.Equal(t, int32(0), credCalls.Load(), "compile validation must not read credentials")
	dec, err := rt.Evaluate(context.Background(), compiled, ProbeInput("hello"), EvalOptions{ProjectID: "proj-1"})
	require.NoError(t, err)
	assert.True(t, dec.Bool)
	assert.Equal(t, int32(1), providerCalls.Load())
	assert.Equal(t, int32(1), credCalls.Load(), "allowed api_key evaluation must resolve credential once")
	models, err := rt.ListModels(WithEvalProjectID(context.Background(), "proj-1"), inst)
	require.NoError(t, err)
	assert.Equal(t, []string{"model-a"}, models)

	// 6. Remove inst-a from proj-1's allowlist -> takes effect immediately on next evaluation
	// with 0 additional provider requests and 0 additional credential reads.
	allowlist.set("proj-1", nil)
	credBeforeRemoval := credCalls.Load()
	_, err = rt.Evaluate(context.Background(), compiled, ProbeInput("hello"), EvalOptions{ProjectID: "proj-1"})
	require.Error(t, err)
	evalRemoved, ok := EvaluationFrom(err)
	require.True(t, ok)
	assert.Equal(t, string(provider.FailNoEligibleInstance), evalRemoved.Reason)
	assert.Equal(t, int32(1), providerCalls.Load(), "no additional provider request after removal")
	assert.Equal(t, credBeforeRemoval, credCalls.Load(), "no additional credential read after removal")

	// 7. Unbound rule with zero enabled instances fails ValidateCompiled with CategoryBinding
	// even when no project allowlist is configured (rev2-runtime O2).
	rtEmpty := New(Config{
		Registry:  reg,
		Instances: StaticInstances(),
	})
	unboundCompiled := compileAdmissionTestRule(t, "")
	err = rtEmpty.ValidateCompiled(context.Background(), unboundCompiled)
	require.Error(t, err)
	ceEmpty, ok := spec.AsCompileError(err)
	require.True(t, ok)
	assert.Equal(t, spec.CategoryBinding, ceEmpty.Category)

	// 8. Unbound rule with >1 enabled, allowlisted instances (and no defaultInstance)
	// is ambiguous and fails ValidateCompiled with CategoryBinding (rev3-runtime O1);
	// allowlisting exactly one succeeds at compile and evaluate.
	instB := Instance{
		ID:       "inst-allow-b",
		Name:     "inst-b",
		Adapter:  "adm_allow",
		Model:    "test-model-1",
		Endpoint: tlsSrv.URL,
		Auth:     InstanceAuth{Kind: "api_key", SecretName: "test-allow-secret"},
		Enabled:  true,
	}
	allowMulti := &mutableProjectAllowlist{}
	allowMulti.set("proj-multi", []string{"inst-a", "inst-b"})
	rtMulti := New(Config{
		Registry:         reg,
		Instances:        StaticInstances(inst, instB),
		ProjectAllowlist: allowMulti,
		Credentials: func(context.Context, Instance) (string, error) {
			return "sk-test-allow-key", nil
		},
	}, WithEgressOptionsForTesting(egress.WithLoopbackServerForTesting(tlsSrv.URL, tlsSrv.Client())))
	err = rtMulti.ValidateCompiled(WithEvalProjectID(context.Background(), "proj-multi"), unboundCompiled)
	require.Error(t, err)
	ceAmbig, ok := spec.AsCompileError(err)
	require.True(t, ok)
	assert.Equal(t, spec.CategoryBinding, ceAmbig.Category)
	assert.Contains(t, ceAmbig.Message, "ambiguous")

	allowMulti.set("proj-multi", []string{"inst-a"})
	require.NoError(t, rtMulti.ValidateCompiled(WithEvalProjectID(context.Background(), "proj-multi"), unboundCompiled))
	decMulti, err := rtMulti.Evaluate(context.Background(), unboundCompiled, ProbeInput("hello"), EvalOptions{ProjectID: "proj-multi"})
	require.NoError(t, err)
	assert.True(t, decMulti.Bool)
}

func TestAdmission_CallerDeadlineDuringAdmissionClassifiesAsCanceled(t *testing.T) {
	block := make(chan struct{})
	inFlight := make(chan struct{})
	adapter := &admissionFakeAdapter{
		name: "adm_deadline",
		invoke: func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			select {
			case inFlight <- struct{}{}:
			default:
			}
			<-block
			return binarySuccessResult(), nil
		},
	}
	reg := provider.NewRegistry()
	require.NoError(t, reg.Register(adapter))

	inst := Instance{
		ID:      "inst-dead",
		Name:    "inst-dead",
		Adapter: "adm_deadline",
		Model:   "test-model-1",
		Auth:    InstanceAuth{Kind: "none"},
		Limits:  InstanceLimits{MaxInFlight: 1},
		Enabled: true,
	}
	rt := New(Config{
		Registry:              reg,
		Instances:             StaticInstances(inst),
		MaxInFlightPerProject: 4,
		AdmissionWait:         80 * time.Millisecond,
	})
	compiled := compileAdmissionTestRule(t, "inst-dead")

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = rt.Evaluate(context.Background(), compiled, ProbeInput("holder"), EvalOptions{ProjectID: "proj-1"})
	}()
	<-inFlight

	// 1. Caller deadline shorter than admissionWait -> FailCanceled (NOT capacity).
	shortCtx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	_, err := rt.Evaluate(shortCtx, compiled, ProbeInput("short-deadline"), EvalOptions{ProjectID: "proj-2"})
	require.Error(t, err)
	evalShort, ok := EvaluationFrom(err)
	require.True(t, ok)
	assert.Equal(t, string(provider.FailCanceled), evalShort.Reason)

	// 2. Caller deadline equal to AdmissionWait -> still FailCanceled (NOT capacity).
	equalCtx, cancelEqual := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancelEqual()
	_, err = rt.Evaluate(equalCtx, compiled, ProbeInput("equal-deadline"), EvalOptions{ProjectID: "proj-2"})
	require.Error(t, err)
	evalEqual, ok := EvaluationFrom(err)
	require.True(t, ok)
	assert.Equal(t, string(provider.FailCanceled), evalEqual.Reason)

	// 3. Live caller ctx saturating admissionWait -> still FailCapacity.
	_, err = rt.Evaluate(context.Background(), compiled, ProbeInput("live-ctx"), EvalOptions{ProjectID: "proj-2"})
	require.Error(t, err)
	evalCap, ok := EvaluationFrom(err)
	require.True(t, ok)
	assert.Equal(t, string(provider.FailCapacity), evalCap.Reason)

	// 4. Caller deadline later than admissionWait -> still FailCapacity.
	longCtx, cancelLong := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelLong()
	_, err = rt.Evaluate(longCtx, compiled, ProbeInput("long-deadline"), EvalOptions{ProjectID: "proj-2"})
	require.Error(t, err)
	evalLong, ok := EvaluationFrom(err)
	require.True(t, ok)
	assert.Equal(t, string(provider.FailCapacity), evalLong.Reason)

	close(block)
	wg.Wait()
}

// TestAdmission_DeterministicStopCauseAndBoundaryMatrix covers Fix Round 5
// Addendum (a)–(g) deterministically, including exact equal-bound precedence
// (via injected nowFn) and the deterministic waitCtx-fires-before-parent-Err
// regression (via injected withWaitCtxFn).
func TestAdmission_DeterministicStopCauseAndBoundaryMatrix(t *testing.T) {
	newTestRuntime := func(admissionWait time.Duration) (*Runtime, *spec.Compiled, chan struct{}, chan struct{}) {
		inFlight := make(chan struct{})
		release := make(chan struct{})
		var holderSignaled atomic.Bool
		adapter := &admissionFakeAdapter{
			name: "adm_det",
			invoke: func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
				if holderSignaled.CompareAndSwap(false, true) {
					close(inFlight)
					<-release
				}
				return binarySuccessResult(), nil
			},
		}
		reg := provider.NewRegistry()
		require.NoError(t, reg.Register(adapter))
		inst := Instance{
			ID:      "inst-det",
			Name:    "inst-det",
			Adapter: "adm_det",
			Model:   "test-model-1",
			Auth:    InstanceAuth{Kind: "none"},
			Limits:  InstanceLimits{MaxInFlight: 1},
			Enabled: true,
		}
		rt := New(Config{
			Registry:              reg,
			Instances:             StaticInstances(inst),
			MaxInFlightPerProject: 4,
			AdmissionWait:         admissionWait,
		})
		return rt, compileAdmissionTestRule(t, "inst-det"), inFlight, release
	}

	// (g) Deterministic regression: force waitCtx to expire BEFORE parent ctx.Err()
	// becomes visible (ctx.Err() is still nil when acquire returns), with
	// callerDL <= now+admissionWait. Old code checked only ctx.Err() != nil and
	// returned FailCapacity; new code preserves admissionStopAttemptDeadline -> FailCanceled.
	t.Run("g_waitCtx_expires_before_parent_ctx_err_visible", func(t *testing.T) {
		rt, compiled, inFlight, release := newTestRuntime(250 * time.Millisecond)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = rt.Evaluate(context.Background(), compiled, ProbeInput("holder"), EvalOptions{ProjectID: "proj-1"})
		}()
		<-inFlight

		// Caller deadline is 50ms from t0 (< 250ms admissionWait), so caller deadline
		// is the limiting bound. Inject withWaitCtxFn returning an already-canceled
		// waitCtx while parent callerCtx is STILL live (callerCtx.Err() == nil).
		t0 := time.Now()
		rt.admission.nowFn = func() time.Time { return t0 }
		rt.admission.withWaitCtxFn = func(parent context.Context, _ time.Time) (context.Context, context.CancelFunc) {
			wctx, cancel := context.WithCancel(parent)
			cancel() // expires waitCtx immediately while parent ctx.Err() is still nil
			return wctx, cancel
		}

		callerCtx, cancelCaller := context.WithDeadline(context.Background(), t0.Add(50*time.Millisecond))
		defer cancelCaller()
		require.NoError(t, callerCtx.Err(), "precondition: parent ctx.Err() must still be nil")

		_, err := rt.Evaluate(callerCtx, compiled, ProbeInput("waiter-g"), EvalOptions{ProjectID: "proj-2"})
		require.NoError(t, callerCtx.Err(), "parent ctx.Err() was still nil when Evaluate returned")
		require.Error(t, err)
		eval, ok := EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, string(provider.FailCanceled), eval.Reason)
		require.Len(t, eval.Attempts, 1)
		assert.Equal(t, 0, eval.Attempts[0].Requests)

		close(release)
		wg.Wait()
	})

	// (c) Exact equal bounds: callerDL.Equal(now + admissionWait).
	// Precedence: caller/attempt deadline takes precedence over admissionWait -> FailCanceled.
	t.Run("c_equal_bounds_caller_deadline_precedence", func(t *testing.T) {
		rt, compiled, inFlight, release := newTestRuntime(100 * time.Millisecond)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = rt.Evaluate(context.Background(), compiled, ProbeInput("holder"), EvalOptions{ProjectID: "proj-1"})
		}()
		<-inFlight

		t0 := time.Now()
		exactDL := t0.Add(100 * time.Millisecond)
		rt.admission.nowFn = func() time.Time { return t0 }
		// Also test with waitCtx expiring right at the boundary while parent ctx.Err() is nil.
		rt.admission.withWaitCtxFn = func(parent context.Context, _ time.Time) (context.Context, context.CancelFunc) {
			wctx, cancel := context.WithCancel(parent)
			cancel()
			return wctx, cancel
		}

		callerCtx, cancelCaller := context.WithDeadline(context.Background(), exactDL)
		defer cancelCaller()

		_, err := rt.Evaluate(callerCtx, compiled, ProbeInput("waiter-c"), EvalOptions{ProjectID: "proj-2"})
		require.Error(t, err)
		eval, ok := EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, string(provider.FailCanceled), eval.Reason)

		close(release)
		wg.Wait()
	})

	// (b) admissionWait strictly earlier than caller deadline -> FailCapacity
	// (even with deterministic immediate waitCtx expiration).
	t.Run("b_admission_wait_strictly_earlier_gives_capacity", func(t *testing.T) {
		rt, compiled, inFlight, release := newTestRuntime(100 * time.Millisecond)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = rt.Evaluate(context.Background(), compiled, ProbeInput("holder"), EvalOptions{ProjectID: "proj-1"})
		}()
		<-inFlight

		t0 := time.Now()
		rt.admission.nowFn = func() time.Time { return t0 }
		rt.admission.withWaitCtxFn = func(parent context.Context, _ time.Time) (context.Context, context.CancelFunc) {
			wctx, cancel := context.WithCancel(parent)
			cancel()
			return wctx, cancel
		}

		// Caller deadline is 500ms > 100ms admissionWait -> admissionWait is limiting bound.
		callerCtx, cancelCaller := context.WithDeadline(context.Background(), t0.Add(500*time.Millisecond))
		defer cancelCaller()

		_, err := rt.Evaluate(callerCtx, compiled, ProbeInput("waiter-b"), EvalOptions{ProjectID: "proj-2"})
		require.Error(t, err)
		eval, ok := EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, string(provider.FailCapacity), eval.Reason)

		close(release)
		wg.Wait()
	})

	// (d) Explicit cancellation during admission -> FailCanceled.
	t.Run("d_explicit_cancellation_during_admission", func(t *testing.T) {
		rt, compiled, inFlight, release := newTestRuntime(2 * time.Second)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = rt.Evaluate(context.Background(), compiled, ProbeInput("holder"), EvalOptions{ProjectID: "proj-1"})
		}()
		<-inFlight

		cancelCtx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(15 * time.Millisecond)
			cancel()
		}()
		_, err := rt.Evaluate(cancelCtx, compiled, ProbeInput("waiter-d"), EvalOptions{ProjectID: "proj-2"})
		require.Error(t, err)
		eval, ok := EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, string(provider.FailCanceled), eval.Reason)

		close(release)
		wg.Wait()
	})

	// (e) Successful acquisition just before the boundary proceeds normally.
	t.Run("e_acquire_just_before_boundary_succeeds", func(t *testing.T) {
		rt, compiled, inFlight, release := newTestRuntime(200 * time.Millisecond)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = rt.Evaluate(context.Background(), compiled, ProbeInput("holder"), EvalOptions{ProjectID: "proj-1"})
		}()
		<-inFlight

		go func() {
			time.Sleep(20 * time.Millisecond)
			close(release)
		}()

		callerCtx, cancelCaller := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancelCaller()
		dec, err := rt.Evaluate(callerCtx, compiled, ProbeInput("waiter-e"), EvalOptions{ProjectID: "proj-2"})
		require.NoError(t, err)
		assert.True(t, dec.Bool)

		wg.Wait()
	})

	// (f) Genuine exhaustion with time remaining -> FailCapacity with 0 requests.
	t.Run("f_genuine_exhaustion_with_time_remaining", func(t *testing.T) {
		rt, compiled, inFlight, release := newTestRuntime(40 * time.Millisecond)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = rt.Evaluate(context.Background(), compiled, ProbeInput("holder"), EvalOptions{ProjectID: "proj-1"})
		}()
		<-inFlight

		_, err := rt.Evaluate(context.Background(), compiled, ProbeInput("waiter-f"), EvalOptions{ProjectID: "proj-2"})
		require.Error(t, err)
		eval, ok := EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, string(provider.FailCapacity), eval.Reason)
		require.Len(t, eval.Attempts, 1)
		assert.Equal(t, 0, eval.Attempts[0].Requests)

		close(release)
		wg.Wait()
	})
}
