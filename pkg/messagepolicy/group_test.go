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

package messagepolicy_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/messagepolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	systemoneruntime "github.com/GoogleCloudPlatform/scion/pkg/systemone/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
)

const groupRuleBodyOnly = `{
	"dialect": "system_one",
	"inputs": ["message.body"],
	"spec": {
		"version": 1,
		"binding": {"instance": "inst-a"},
		"questions": [{
			"id": "asks_choice",
			"type": "binary",
			"instructions": "Does the message ask the recipient to choose among alternatives?"
		}]
	}
}`

const groupRuleBodyOnlyAlt = `{
	"dialect": "system_one",
	"inputs": ["message.body"],
	"spec": {
		"version": 1,
		"binding": {"instance": "inst-a"},
		"questions": [{
			"id": "asks_choice",
			"type": "binary",
			"instructions": "Does the message contain an urgent escalation?"
		}]
	}
}`

type groupTestHarness struct {
	ev    *messagepolicy.Evaluator
	rt    *systemoneruntime.Runtime
	calls *atomic.Int32
}

func newGroupTestHarness(
	t *testing.T,
	instances []systemoneruntime.Instance,
	enabled func() bool,
	invokeFn func(ctx context.Context, tr provider.Transport, iv provider.InstanceView, req provider.ModelRequest) (provider.ModelResult, error),
) *groupTestHarness {
	t.Helper()
	var calls atomic.Int32
	reg := provider.NewRegistry()
	reg.MustRegister(&mpFakeAdapter{
		name: "fake-group",
		invokeFn: func(ctx context.Context, tr provider.Transport, iv provider.InstanceView, req provider.ModelRequest) (provider.ModelResult, error) {
			calls.Add(1)
			return invokeFn(ctx, tr, iv, req)
		},
	})
	rt := systemoneruntime.New(systemoneruntime.Config{
		Registry:    reg,
		Instances:   systemoneruntime.StaticInstances(instances...),
		Credentials: systemoneruntime.StaticCredential("sk-group-test-secret"),
	})
	t.Cleanup(rt.CloseIdleConnections)

	eng := ruleengine.NewEngine()
	celDialect, err := ruleengine.NewCELDialect()
	require.NoError(t, err)
	celEng, err := messagepolicy.NewCELEngine()
	require.NoError(t, err)
	require.NoError(t, celDialect.RegisterEnvironment(celEng))
	require.NoError(t, eng.Register(celDialect))

	if enabled == nil {
		enabled = func() bool { return true }
	}
	require.NoError(t, eng.Register(systemone.NewDialect(systemone.DialectConfig{
		Runtime: rt,
		Enabled: enabled,
	})))
	require.NoError(t, eng.AllowConsumerDialects(
		messagepolicy.ConsumerName,
		ruleengine.Grant(ruleengine.DialectCEL),
		ruleengine.GrantIO(systemone.DialectName),
	))
	require.NoError(t, systemone.RegisterConsumerEnvironment(eng, messagepolicy.ConsumerName, messagepolicy.SystemOneContentEnv()))

	ev, err := messagepolicy.New(eng)
	require.NoError(t, err)
	return &groupTestHarness{ev: ev, rt: rt, calls: &calls}
}

func defaultGroupInstance(name string) systemoneruntime.Instance {
	return systemoneruntime.Instance{
		ID:       "id-" + name,
		Name:     name,
		Adapter:  "fake-group",
		Model:    "fake-model-1",
		Endpoint: "https://fake.example.test",
		Auth:     systemoneruntime.InstanceAuth{Kind: "api_key", SecretName: "systemone.instance." + name},
		Enabled:  true,
		Revision: 1,
	}
}

func okBinaryGroupResult(model string, val bool) provider.ModelResult {
	v := val
	dist := map[string]float64{"true": 0.1, "false": 0.9}
	if val {
		dist = map[string]float64{"true": 0.9, "false": 0.1}
	}
	return provider.ModelResult{
		ResolvedModel: model,
		HTTPStatus:    200,
		Answers: map[string]provider.Answer{
			"asks_choice": {
				QuestionID:   "asks_choice",
				Primitive:    spec.PrimitiveBinary,
				Status:       provider.AnswerStatusAnswered,
				Bool:         &v,
				Distribution: dist,
			},
		},
		Usage: provider.Usage{InputTokens: 17, OutputTokens: 2},
	}
}

type groupMemberSpec struct {
	PolicyID        string
	PolicyName      string
	Revision        int64
	SystemOneRule   string
	ViolationRule   string
	OnUncertain     string
	OnFailed        string
	OnFailedReasons map[string]string
}

type groupMemberResult struct {
	Classification messagepolicy.Classification
	Outcome        string
	Elapsed        time.Duration
}

// evalPlannedGroups calls the shared production PlanSystemOneGroups and
// RunGroupPlan helpers (the exact same path called by hub's
// evalPlannedSystemOnePolicies) and resolves each member's outcome.
func evalPlannedGroups(
	soCtx context.Context,
	ev *messagepolicy.Evaluator,
	members []groupMemberSpec,
	projectID string,
	facts messagepolicy.Facts,
	celBudget *ruleengine.BudgetTracker,
	soBudget time.Duration,
) ([]groupMemberResult, messagepolicy.GroupPlan) {
	candidates := make([]messagepolicy.GroupCandidate, len(members))
	for i, m := range members {
		candidates[i] = messagepolicy.GroupCandidate{
			Index:         i,
			PolicyID:      m.PolicyID,
			PolicyName:    m.PolicyName,
			Revision:      m.Revision,
			SystemOneRule: m.SystemOneRule,
			ViolationRule: m.ViolationRule,
		}
	}
	baseReq := systemone.Request{
		Input:     messagepolicy.BuildSystemOneInput(facts),
		ProjectID: projectID,
	}
	keyCfg := messagepolicy.GroupKeyConfig{
		Consumer:     messagepolicy.ConsumerName,
		Dialect:      systemone.DialectName,
		AllowlistRef: projectID,
		MaxEvalTime:  soBudget,
	}
	plan := ev.PlanSystemOneGroups(soCtx, candidates, baseReq, keyCfg)
	raw := ev.RunGroupPlan(
		soCtx,
		plan,
		baseReq,
		facts,
		[]messagepolicy.EvalOption{messagepolicy.WithMaxEvalTime(soBudget)},
		[]messagepolicy.EvalOption{messagepolicy.WithBudget(celBudget)},
	)
	out := make([]groupMemberResult, len(members))
	for _, mr := range raw {
		m := members[mr.Candidate.Index]
		out[mr.Candidate.Index] = groupMemberResult{
			Classification: mr.Classification,
			Outcome:        messagepolicy.ResolveOutcome(mr.Classification.Class, mr.Classification.Reason, m.OnUncertain, m.OnFailed, m.OnFailedReasons),
			Elapsed:        mr.Elapsed,
		}
	}
	return out, plan
}

func defaultGroupFacts(body string) messagepolicy.Facts {
	return messagepolicy.Facts{
		Surface:   messagepolicy.SurfaceAgentToUser,
		Sender:    messagepolicy.SenderFacts{Kind: "agent", ID: "ag-1", Slug: "agent-a", ProjectID: "proj-1"},
		Recipient: messagepolicy.RecipientFacts{Kind: "user", ID: "u-1", Slug: "alice"},
		Message:   messagepolicy.MessageFacts{Type: "message", Body: body},
	}
}

func TestGroups_KeyFields(t *testing.T) {
	contentEnv := messagepolicy.SystemOneContentEnv()
	compile := func(raw string) *spec.Compiled {
		t.Helper()
		c, err := spec.Compile([]byte(raw), spec.CompileOptions{
			Kind:        spec.Kind(messagepolicy.KindJudgments),
			Environment: &contentEnv,
		})
		require.NoError(t, err)
		return c
	}

	baseCompiled := compile(groupRuleBodyOnly)
	baseReq := systemone.Request{
		ProjectID: "proj-1",
		RuleID:    "pol-1",
		Revision:  1,
		Input: systemone.Input{
			Fields: []provider.InputField{
				{Name: messagepolicy.FieldMessageBody, Kind: spec.FieldKindText, Trust: spec.TrustUntrusted, Value: "hello"},
				{Name: messagepolicy.FieldSenderSlug, Kind: spec.FieldKindText, Trust: spec.TrustTrusted, Value: "agent-a"},
			},
			ChoiceSets: map[string][]string{"topics": {"a", "b"}},
			Params:     map[string]string{"mode": "strict"},
		},
	}
	baseCfg := messagepolicy.GroupKeyConfig{
		Consumer:     messagepolicy.ConsumerName,
		Dialect:      systemone.DialectName,
		AllowlistRef: "proj-1",
		MaxEvalTime:  1500 * time.Millisecond,
	}
	baseKey := messagepolicy.GroupKey(baseCompiled, baseReq, baseCfg)
	require.NotEmpty(t, baseKey)

	// Non-key fields MUST NOT split the group.
	t.Run("policy_id_excluded", func(t *testing.T) {
		r := baseReq
		r.RuleID = "pol-999"
		assert.Equal(t, baseKey, messagepolicy.GroupKey(baseCompiled, r, baseCfg))
	})
	t.Run("policy_revision_excluded", func(t *testing.T) {
		r := baseReq
		r.Revision = 42
		assert.Equal(t, baseKey, messagepolicy.GroupKey(baseCompiled, r, baseCfg))
	})
	t.Run("element_index_excluded", func(t *testing.T) {
		idx := 3
		r := baseReq
		r.ElementIndex = &idx
		assert.Equal(t, baseKey, messagepolicy.GroupKey(baseCompiled, r, baseCfg))
	})
	t.Run("unselected_input_excluded", func(t *testing.T) {
		r := baseReq
		r.Input.Fields = []provider.InputField{
			{Name: messagepolicy.FieldMessageBody, Kind: spec.FieldKindText, Trust: spec.TrustUntrusted, Value: "hello"},
			{Name: messagepolicy.FieldSenderSlug, Kind: spec.FieldKindText, Trust: spec.TrustTrusted, Value: "agent-OTHER"},
		}
		assert.Equal(t, baseKey, messagepolicy.GroupKey(baseCompiled, r, baseCfg))
	})

	// Every identity field MUST split the group.
	t.Run("content_hash_splits", func(t *testing.T) {
		alt := compile(groupRuleBodyOnlyAlt)
		assert.NotEqual(t, baseKey, messagepolicy.GroupKey(alt, baseReq, baseCfg))
	})
	t.Run("spec_version_splits", func(t *testing.T) {
		cp := *baseCompiled
		cp.Spec.Version = 2
		assert.NotEqual(t, baseKey, messagepolicy.GroupKey(&cp, baseReq, baseCfg))
	})
	t.Run("dialect_splits", func(t *testing.T) {
		cfg := baseCfg
		cfg.Dialect = "system_one_v2"
		assert.NotEqual(t, baseKey, messagepolicy.GroupKey(baseCompiled, baseReq, cfg))
	})
	t.Run("rule_timeout_splits", func(t *testing.T) {
		cp := *baseCompiled
		cp.Timeout = 500 * time.Millisecond
		assert.NotEqual(t, baseKey, messagepolicy.GroupKey(&cp, baseReq, baseCfg))
	})
	t.Run("selected_input_value_splits", func(t *testing.T) {
		r := baseReq
		r.Input.Fields = []provider.InputField{
			{Name: messagepolicy.FieldMessageBody, Kind: spec.FieldKindText, Trust: spec.TrustUntrusted, Value: "different body"},
		}
		assert.NotEqual(t, baseKey, messagepolicy.GroupKey(baseCompiled, r, baseCfg))
	})
	t.Run("choice_sets_splits", func(t *testing.T) {
		r := baseReq
		r.Input.ChoiceSets = map[string][]string{"topics": {"a", "c"}}
		assert.NotEqual(t, baseKey, messagepolicy.GroupKey(baseCompiled, r, baseCfg))
	})
	t.Run("params_splits", func(t *testing.T) {
		r := baseReq
		r.Input.Params = map[string]string{"mode": "lenient"}
		assert.NotEqual(t, baseKey, messagepolicy.GroupKey(baseCompiled, r, baseCfg))
	})
	t.Run("project_id_splits", func(t *testing.T) {
		r := baseReq
		r.ProjectID = "proj-2"
		assert.NotEqual(t, baseKey, messagepolicy.GroupKey(baseCompiled, r, baseCfg))
	})
	t.Run("binding_instance_splits", func(t *testing.T) {
		cp := *baseCompiled
		cp.Spec.Binding.Instance = "inst-b"
		assert.NotEqual(t, baseKey, messagepolicy.GroupKey(&cp, baseReq, baseCfg))
	})
	t.Run("binding_pool_splits", func(t *testing.T) {
		cp := *baseCompiled
		cp.Spec.Binding.Pool = "pool-a"
		assert.NotEqual(t, baseKey, messagepolicy.GroupKey(&cp, baseReq, baseCfg))
	})
	t.Run("allowlist_ref_splits", func(t *testing.T) {
		cfg := baseCfg
		cfg.AllowlistRef = "proj-allowlist-2"
		assert.NotEqual(t, baseKey, messagepolicy.GroupKey(baseCompiled, baseReq, cfg))
	})
	t.Run("max_eval_time_splits", func(t *testing.T) {
		cfg := baseCfg
		cfg.MaxEvalTime = 2000 * time.Millisecond
		assert.NotEqual(t, baseKey, messagepolicy.GroupKey(baseCompiled, baseReq, cfg))
	})
	t.Run("budget_splits", func(t *testing.T) {
		cfg := baseCfg
		cfg.Budget = messagepolicy.NewSendBudget()
		assert.NotEqual(t, baseKey, messagepolicy.GroupKey(baseCompiled, baseReq, cfg))
		cfg2 := baseCfg
		cfg2.Budget = messagepolicy.NewSendBudget()
		assert.NotEqual(t, messagepolicy.GroupKey(baseCompiled, baseReq, cfg), messagepolicy.GroupKey(baseCompiled, baseReq, cfg2))
	})
	t.Run("consumer_splits", func(t *testing.T) {
		cfg := baseCfg
		cfg.Consumer = "other_consumer"
		assert.NotEqual(t, baseKey, messagepolicy.GroupKey(baseCompiled, baseReq, cfg))
	})
}

func TestGroups_OneExecutionPerGroup(t *testing.T) {
	h := newGroupTestHarness(t, []systemoneruntime.Instance{defaultGroupInstance("inst-a")}, nil, func(_ context.Context, _ provider.Transport, iv provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
		return okBinaryGroupResult(iv.Model, true), nil
	})

	members := []groupMemberSpec{
		{PolicyID: "p-3", PolicyName: "pol-c", Revision: 3, SystemOneRule: groupRuleBodyOnly, ViolationRule: `judgment_flags.asks_choice`},
		{PolicyID: "p-1", PolicyName: "pol-a", Revision: 1, SystemOneRule: groupRuleBodyOnly, ViolationRule: `judgment_flags.asks_choice`},
		{PolicyID: "p-2", PolicyName: "pol-b", Revision: 2, SystemOneRule: groupRuleBodyOnly, ViolationRule: `!judgment_flags.asks_choice`},
		{PolicyID: "p-4", PolicyName: "pol-d", Revision: 1, SystemOneRule: groupRuleBodyOnly, ViolationRule: `judgment_flags.asks_choice`},
	}

	res, plan := evalPlannedGroups(context.Background(), h.ev, members, "proj-1", defaultGroupFacts("Pick A or B"), messagepolicy.NewSendBudget(), time.Second)
	require.Len(t, plan.Groups, 1)
	assert.Equal(t, int32(1), h.calls.Load(), "N identical policies must trigger exactly 1 provider call")
	assert.Equal(t, messagepolicy.ClassViolation, res[0].Classification.Class)
	assert.Equal(t, messagepolicy.ClassViolation, res[1].Classification.Class)
	assert.Equal(t, messagepolicy.ClassPass, res[2].Classification.Class)
	assert.Equal(t, messagepolicy.ClassViolation, res[3].Classification.Class)

	// Two distinct keys produce exactly 2 provider calls.
	h.calls.Store(0)
	membersTwoKeys := []groupMemberSpec{
		{PolicyID: "p-1", PolicyName: "pol-a", Revision: 1, SystemOneRule: groupRuleBodyOnly, ViolationRule: `judgment_flags.asks_choice`},
		{PolicyID: "p-2", PolicyName: "pol-b", Revision: 2, SystemOneRule: groupRuleBodyOnly, ViolationRule: `judgment_flags.asks_choice`},
		{PolicyID: "p-3", PolicyName: "pol-c", Revision: 1, SystemOneRule: groupRuleBodyOnlyAlt, ViolationRule: `judgment_flags.asks_choice`},
		{PolicyID: "p-4", PolicyName: "pol-d", Revision: 2, SystemOneRule: groupRuleBodyOnlyAlt, ViolationRule: `judgment_flags.asks_choice`},
	}
	_, planTwo := evalPlannedGroups(context.Background(), h.ev, membersTwoKeys, "proj-1", defaultGroupFacts("Pick A or B"), messagepolicy.NewSendBudget(), time.Second)
	require.Len(t, planTwo.Groups, 2)
	assert.Equal(t, int32(2), h.calls.Load(), "2 distinct group keys must trigger exactly 2 provider calls")
}

func TestGroups_IndependentConcurrent(t *testing.T) {
	// Two groups whose fakes block on each other's arrival: only passes if
	// groups execute concurrently (kills serializing groups).
	var arrived atomic.Int32
	bothArrived := make(chan struct{})
	h := newGroupTestHarness(t, []systemoneruntime.Instance{defaultGroupInstance("inst-a")}, nil, func(ctx context.Context, _ provider.Transport, iv provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
		if arrived.Add(1) == 2 {
			close(bothArrived)
		}
		select {
		case <-bothArrived:
			return okBinaryGroupResult(iv.Model, false), nil
		case <-ctx.Done():
			return provider.ModelResult{}, ctx.Err()
		}
	})

	members := []groupMemberSpec{
		{PolicyID: "p-1", PolicyName: "pol-a", Revision: 1, SystemOneRule: groupRuleBodyOnly, ViolationRule: `judgment_flags.asks_choice`},
		{PolicyID: "p-2", PolicyName: "pol-b", Revision: 1, SystemOneRule: groupRuleBodyOnlyAlt, ViolationRule: `judgment_flags.asks_choice`},
	}
	soCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	res, plan := evalPlannedGroups(soCtx, h.ev, members, "proj-1", defaultGroupFacts("Hello"), messagepolicy.NewSendBudget(), 2*time.Second)
	require.Len(t, plan.Groups, 2)
	assert.Equal(t, int32(2), h.calls.Load())
	assert.Equal(t, messagepolicy.ClassPass, res[0].Classification.Class)
	assert.Equal(t, messagepolicy.ClassPass, res[1].Classification.Class)
}

func TestGroups_MemberOutcomesRealPath(t *testing.T) {
	inst := defaultGroupInstance("inst-a")
	inst.Limits.MaxInFlight = 1

	holderIn := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	h := newGroupTestHarness(t, []systemoneruntime.Instance{inst}, nil, func(ctx context.Context, _ provider.Transport, iv provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
		once.Do(func() { close(holderIn) })
		select {
		case <-release:
		case <-ctx.Done():
		}
		return okBinaryGroupResult(iv.Model, false), nil
	})

	holderDone := make(chan struct{})
	go func() {
		defer close(holderDone)
		h.ev.EvaluateSystemOne(context.Background(), groupRuleBodyOnly, systemone.Request{
			ProjectID: "proj-1",
			RuleID:    "holder",
			Input:     messagepolicy.BuildSystemOneInput(defaultGroupFacts("holder")),
		}, messagepolicy.WithMaxEvalTime(3*time.Second))
	}()
	<-holderIn
	defer func() {
		close(release)
		<-holderDone
	}()

	members := []groupMemberSpec{
		{
			PolicyID:        "p-1",
			PolicyName:      "pol-admit-cap",
			Revision:        1,
			SystemOneRule:   groupRuleBodyOnly,
			ViolationRule:   `judgment_flags.asks_choice`,
			OnFailed:        messagepolicy.OutcomeReject,
			OnFailedReasons: map[string]string{string(provider.FailCapacity): messagepolicy.OutcomeAdmit},
		},
		{
			PolicyID:        "p-2",
			PolicyName:      "pol-reject-cap",
			Revision:        2,
			SystemOneRule:   groupRuleBodyOnly,
			ViolationRule:   `judgment_flags.asks_choice`,
			OnFailed:        messagepolicy.OutcomeAdmit,
			OnFailedReasons: map[string]string{string(provider.FailCapacity): messagepolicy.OutcomeReject},
		},
	}

	res, plan := evalPlannedGroups(context.Background(), h.ev, members, "proj-1", defaultGroupFacts("same"), messagepolicy.NewSendBudget(), time.Second)
	require.Len(t, plan.Groups, 1)
	assert.Equal(t, messagepolicy.ClassFailed, res[0].Classification.Class)
	assert.Equal(t, string(provider.FailCapacity), res[0].Classification.Reason)
	assert.Equal(t, messagepolicy.OutcomeAdmit, res[0].Outcome, "P1 must admit capacity via its own on_failed_reasons")

	assert.Equal(t, messagepolicy.ClassFailed, res[1].Classification.Class)
	assert.Equal(t, string(provider.FailCapacity), res[1].Classification.Reason)
	assert.Equal(t, messagepolicy.OutcomeReject, res[1].Outcome, "P2 must reject capacity via its own on_failed_reasons")
}

func TestGroups_SingleMemberParity(t *testing.T) {
	h := newGroupTestHarness(t, []systemoneruntime.Instance{defaultGroupInstance("inst-a")}, nil, func(_ context.Context, _ provider.Transport, iv provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
		return okBinaryGroupResult(iv.Model, true), nil
	})
	facts := defaultGroupFacts("Option A or B?")
	members := []groupMemberSpec{
		{
			PolicyID:      "p-solo",
			PolicyName:    "solo",
			Revision:      5,
			SystemOneRule: groupRuleBodyOnly,
			ViolationRule: `judgment_flags.asks_choice`,
			OnUncertain:   messagepolicy.OutcomeReject,
			OnFailed:      messagepolicy.OutcomeReject,
		},
	}

	grouped, plan := evalPlannedGroups(context.Background(), h.ev, members, "proj-1", facts, messagepolicy.NewSendBudget(), time.Second)
	require.Len(t, plan.Groups, 1)
	require.Len(t, grouped, 1)

	direct := h.ev.EvaluatePolicy(
		context.Background(), "p-solo", 5, `judgment_flags.asks_choice`, groupRuleBodyOnly, "proj-1", facts,
		[]messagepolicy.EvalOption{messagepolicy.WithBudget(messagepolicy.NewSendBudget())},
		[]messagepolicy.EvalOption{messagepolicy.WithMaxEvalTime(time.Second)},
	)
	directOutcome := messagepolicy.ResolveOutcome(direct.Class, direct.Reason, messagepolicy.OutcomeReject, messagepolicy.OutcomeReject, nil)

	gCls := grouped[0].Classification
	assert.Equal(t, direct.Class, gCls.Class)
	assert.Equal(t, direct.Reason, gCls.Reason)
	assert.Equal(t, directOutcome, grouped[0].Outcome)
	assert.Equal(t, direct.Judgments, gCls.Judgments)
	assert.Empty(t, gCls.SourceEvalID)
	require.NotNil(t, gCls.Evaluation)
	assert.False(t, gCls.Evaluation.CacheHit)
	assert.Equal(t, "p-solo", gCls.Evaluation.Rule.RuleID)
	assert.Equal(t, int64(5), gCls.Evaluation.Rule.Revision)
	require.NotNil(t, gCls.Evaluation.Usage)
	assert.Equal(t, direct.Evaluation.Usage, gCls.Evaluation.Usage)
}

func TestGroups_CompileFailureUngrouped(t *testing.T) {
	// Case 1: valid policies group while a malformed rule stays ungrouped.
	h := newGroupTestHarness(t, []systemoneruntime.Instance{defaultGroupInstance("inst-a")}, nil, func(_ context.Context, _ provider.Transport, iv provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
		return okBinaryGroupResult(iv.Model, false), nil
	})
	members := []groupMemberSpec{
		{PolicyID: "p-1", PolicyName: "pol-a", Revision: 1, SystemOneRule: groupRuleBodyOnly, ViolationRule: `judgment_flags.asks_choice`},
		{PolicyID: "p-bad", PolicyName: "pol-bad", Revision: 1, SystemOneRule: `{"dialect":"system_one","spec":{"version":999}}`, ViolationRule: `judgment_flags.asks_choice`, OnFailed: messagepolicy.OutcomeReject},
		{PolicyID: "p-2", PolicyName: "pol-b", Revision: 1, SystemOneRule: groupRuleBodyOnly, ViolationRule: `judgment_flags.asks_choice`},
	}
	res, plan := evalPlannedGroups(context.Background(), h.ev, members, "proj-1", defaultGroupFacts("hi"), messagepolicy.NewSendBudget(), time.Second)
	require.Len(t, plan.Groups, 1)
	require.Len(t, plan.Ungrouped, 1)
	assert.Equal(t, int32(1), h.calls.Load())
	assert.Equal(t, messagepolicy.ClassPass, res[0].Classification.Class)
	assert.Equal(t, messagepolicy.ClassFailed, res[1].Classification.Class)
	assert.Equal(t, messagepolicy.ReasonCompileError, res[1].Classification.Reason)
	assert.Equal(t, messagepolicy.ClassPass, res[2].Classification.Class)

	// Case 2: dialect_disabled keeps its per-policy reason and never groups.
	hDis := newGroupTestHarness(t, []systemoneruntime.Instance{defaultGroupInstance("inst-a")}, func() bool { return false }, func(_ context.Context, _ provider.Transport, iv provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
		return okBinaryGroupResult(iv.Model, false), nil
	})
	disMembers := []groupMemberSpec{
		{
			PolicyID:        "p-1",
			PolicyName:      "pol-a",
			Revision:        1,
			SystemOneRule:   groupRuleBodyOnly,
			ViolationRule:   `judgment_flags.asks_choice`,
			OnFailedReasons: map[string]string{systemone.ReasonDialectDisabled: messagepolicy.OutcomeAdmit},
		},
		{
			PolicyID:        "p-2",
			PolicyName:      "pol-b",
			Revision:        1,
			SystemOneRule:   groupRuleBodyOnly,
			ViolationRule:   `judgment_flags.asks_choice`,
			OnFailedReasons: map[string]string{systemone.ReasonDialectDisabled: messagepolicy.OutcomeReject},
		},
	}
	disRes, disPlan := evalPlannedGroups(context.Background(), hDis.ev, disMembers, "proj-1", defaultGroupFacts("hi"), messagepolicy.NewSendBudget(), time.Second)
	assert.Empty(t, disPlan.Groups, "dialect_disabled must never be grouped")
	require.Len(t, disPlan.Ungrouped, 2)
	assert.Equal(t, int32(0), hDis.calls.Load())
	assert.Equal(t, systemone.ReasonDialectDisabled, disRes[0].Classification.Reason)
	assert.Equal(t, messagepolicy.OutcomeAdmit, disRes[0].Outcome)
	assert.Equal(t, systemone.ReasonDialectDisabled, disRes[1].Classification.Reason)
	assert.Equal(t, messagepolicy.OutcomeReject, disRes[1].Outcome)
}

func TestGroups_CapacitySharedOneWait(t *testing.T) {
	const n = 10
	const budget = 600 * time.Millisecond

	inst := defaultGroupInstance("inst-a")
	inst.Limits.MaxInFlight = 1
	holderIn := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	h := newGroupTestHarness(t, []systemoneruntime.Instance{inst}, nil, func(ctx context.Context, _ provider.Transport, iv provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
		once.Do(func() { close(holderIn) })
		select {
		case <-release:
		case <-ctx.Done():
		}
		return okBinaryGroupResult(iv.Model, false), nil
	})

	holderDone := make(chan struct{})
	go func() {
		defer close(holderDone)
		h.ev.EvaluateSystemOne(context.Background(), groupRuleBodyOnly, systemone.Request{
			ProjectID: "proj-1",
			RuleID:    "holder",
			Input:     messagepolicy.BuildSystemOneInput(defaultGroupFacts("holder")),
		}, messagepolicy.WithMaxEvalTime(3*time.Second))
	}()
	<-holderIn
	defer func() {
		close(release)
		<-holderDone
	}()

	members := make([]groupMemberSpec, n)
	for i := 0; i < n; i++ {
		members[i] = groupMemberSpec{
			PolicyID:      fmt.Sprintf("pol-%02d", i),
			PolicyName:    fmt.Sprintf("name-%02d", i),
			Revision:      1,
			SystemOneRule: groupRuleBodyOnly,
			ViolationRule: `judgment_flags.asks_choice`,
		}
	}

	soCtx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	start := time.Now()
	res, plan := evalPlannedGroups(soCtx, h.ev, members, "proj-1", defaultGroupFacts("same"), messagepolicy.NewSendBudget(), budget)
	total := time.Since(start)

	require.Len(t, plan.Groups, 1)
	assert.Equal(t, int32(1), h.calls.Load(), "only holder invoked provider; group failed at admission")
	for i, r := range res {
		assert.Equal(t, messagepolicy.ClassFailed, r.Classification.Class, "member %d", i)
		assert.Equal(t, string(provider.FailCapacity), r.Classification.Reason, "member %d must see capacity, never canceled", i)
	}
	// One AdmissionWait (100ms), never N serial waits (which would be 1000ms or hit 600ms budget).
	assert.Less(t, total, 350*time.Millisecond, "10-member capacity group must finish in ~1 AdmissionWait, got %v", total)
}

func TestGroups_CallTimeoutShared(t *testing.T) {
	const n = 4
	const callTimeout = 150 * time.Millisecond
	const budget = 600 * time.Millisecond

	inst := defaultGroupInstance("inst-a")
	inst.Limits.CallTimeout = callTimeout
	h := newGroupTestHarness(t, []systemoneruntime.Instance{inst}, nil, func(ctx context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
		<-ctx.Done()
		return provider.ModelResult{}, ctx.Err()
	})

	members := make([]groupMemberSpec, n)
	for i := 0; i < n; i++ {
		members[i] = groupMemberSpec{
			PolicyID:      fmt.Sprintf("pol-%d", i),
			PolicyName:    fmt.Sprintf("name-%d", i),
			Revision:      1,
			SystemOneRule: groupRuleBodyOnly,
			ViolationRule: `judgment_flags.asks_choice`,
		}
	}

	soCtx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	start := time.Now()
	res, plan := evalPlannedGroups(soCtx, h.ev, members, "proj-1", defaultGroupFacts("slow"), messagepolicy.NewSendBudget(), budget)
	total := time.Since(start)

	require.Len(t, plan.Groups, 1)
	assert.Equal(t, int32(1), h.calls.Load())
	for i, r := range res {
		assert.Equal(t, messagepolicy.ClassFailed, r.Classification.Class, "member %d", i)
		assert.Equal(t, string(provider.FailTimeout), r.Classification.Reason, "member %d", i)
	}
	assert.Less(t, total, 400*time.Millisecond, "group must time out around CallTimeout (%v), not budget (%v); got %v", callTimeout, budget, total)
}

func TestGroups_ResourceAccounting(t *testing.T) {
	// RatePerSec=0.2 gives burst=1 token: a second admission attempt within the
	// same send would fail with capacity. With 5 grouped members, only 1 permit
	// + 1 rate token + 1 provider request is consumed, so all 5 succeed.
	inst := defaultGroupInstance("inst-a")
	inst.Limits.MaxInFlight = 1
	inst.Limits.RatePerSec = 0.2
	h := newGroupTestHarness(t, []systemoneruntime.Instance{inst}, nil, func(_ context.Context, _ provider.Transport, iv provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
		return okBinaryGroupResult(iv.Model, false), nil
	})

	members := make([]groupMemberSpec, 5)
	for i := 0; i < 5; i++ {
		members[i] = groupMemberSpec{
			PolicyID:      fmt.Sprintf("pol-%d", i),
			PolicyName:    fmt.Sprintf("name-%d", i),
			Revision:      1,
			SystemOneRule: groupRuleBodyOnly,
			ViolationRule: `judgment_flags.asks_choice`,
		}
	}

	celBudget := messagepolicy.NewSendBudget()
	res, plan := evalPlannedGroups(context.Background(), h.ev, members, "proj-1", defaultGroupFacts("hi"), celBudget, time.Second)
	require.Len(t, plan.Groups, 1)
	assert.Equal(t, int32(1), h.calls.Load(), "provider request charged once per group")
	for i, r := range res {
		assert.Equal(t, messagepolicy.ClassPass, r.Classification.Class, "member %d", i)
	}
	assert.Greater(t, celBudget.Spent(), time.Duration(0), "CEL tracker must be charged per member")

	// Prove CEL budget is enforced per member: an already-exhausted tracker
	// fails every member's ApplyJudgment with budget_exhausted even when the
	// single group judgment succeeded.
	inst2 := defaultGroupInstance("inst-a")
	h2 := newGroupTestHarness(t, []systemoneruntime.Instance{inst2}, nil, func(_ context.Context, _ provider.Transport, iv provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
		return okBinaryGroupResult(iv.Model, false), nil
	})
	exhausted := ruleengine.NewBudgetTracker(time.Millisecond)
	exhausted.Exhaust()
	resEx, _ := evalPlannedGroups(context.Background(), h2.ev, members[:2], "proj-1", defaultGroupFacts("hi"), exhausted, time.Second)
	assert.Equal(t, int32(1), h2.calls.Load())
	for i, r := range resEx {
		assert.Equal(t, messagepolicy.ClassFailed, r.Classification.Class, "member %d", i)
		assert.Equal(t, messagepolicy.ReasonBudgetExhausted, r.Classification.Reason, "member %d", i)
	}
}

func TestGroups_SoCtxCancel(t *testing.T) {
	entered := make(chan struct{})
	h := newGroupTestHarness(t, []systemoneruntime.Instance{defaultGroupInstance("inst-a")}, nil, func(ctx context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
		close(entered)
		<-ctx.Done()
		return provider.ModelResult{}, ctx.Err()
	})

	soCtx, soCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer soCancel()

	members := []groupMemberSpec{
		{PolicyID: "p-1", PolicyName: "pol-a", Revision: 1, SystemOneRule: groupRuleBodyOnly, ViolationRule: `judgment_flags.asks_choice`},
		{PolicyID: "p-2", PolicyName: "pol-b", Revision: 2, SystemOneRule: groupRuleBodyOnly, ViolationRule: `judgment_flags.asks_choice`},
		{PolicyID: "p-3", PolicyName: "pol-c", Revision: 3, SystemOneRule: groupRuleBodyOnly, ViolationRule: `judgment_flags.asks_choice`},
	}

	done := make(chan []groupMemberResult, 1)
	go func() {
		res, _ := evalPlannedGroups(soCtx, h.ev, members, "proj-1", defaultGroupFacts("cancel me"), messagepolicy.NewSendBudget(), 3*time.Second)
		done <- res
	}()

	<-entered
	soCancel()

	res := <-done
	assert.Equal(t, int32(1), h.calls.Load())
	for i, r := range res {
		assert.Equal(t, messagepolicy.ClassFailed, r.Classification.Class, "member %d", i)
		assert.Equal(t, string(provider.FailCanceled), r.Classification.Reason, "member %d must receive canceled classification", i)
	}
}

func TestGroups_ProvenanceRows(t *testing.T) {
	h := newGroupTestHarness(t, []systemoneruntime.Instance{defaultGroupInstance("inst-a")}, nil, func(_ context.Context, _ provider.Transport, iv provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
		return okBinaryGroupResult(iv.Model, true), nil
	})

	// Deliberately unsorted input order + name tie to verify deterministic
	// primary selection: lowest PolicyName, then lowest PolicyID.
	members := []groupMemberSpec{
		{PolicyID: "id-z", PolicyName: "zeta", Revision: 9, SystemOneRule: groupRuleBodyOnly, ViolationRule: `judgment_flags.asks_choice`},
		{PolicyID: "id-b", PolicyName: "alpha", Revision: 4, SystemOneRule: groupRuleBodyOnly, ViolationRule: `!judgment_flags.asks_choice`},
		{PolicyID: "id-a", PolicyName: "alpha", Revision: 2, SystemOneRule: groupRuleBodyOnly, ViolationRule: `judgment_flags.asks_choice`},
	}

	res, plan := evalPlannedGroups(context.Background(), h.ev, members, "proj-1", defaultGroupFacts("choose"), messagepolicy.NewSendBudget(), time.Second)
	require.Len(t, plan.Groups, 1)
	assert.Equal(t, "id-a", plan.Groups[0].Primary.PolicyID, "primary must be lowest PolicyName then lowest PolicyID")
	assert.Equal(t, int32(1), h.calls.Load())

	primaryIdx := 2
	primaryEval := res[primaryIdx].Classification.Evaluation
	require.NotNil(t, primaryEval)
	assert.False(t, primaryEval.CacheHit)
	assert.Empty(t, res[primaryIdx].Classification.SourceEvalID)
	assert.Equal(t, "id-a", primaryEval.Rule.RuleID)
	assert.Equal(t, int64(2), primaryEval.Rule.Revision)
	require.NotNil(t, primaryEval.Usage)
	assert.Equal(t, 17, primaryEval.Usage.InputTokens)

	seenEvalIDs := map[string]bool{primaryEval.EvalID: true}
	for _, idx := range []int{0, 1} {
		cls := res[idx].Classification
		require.NotNil(t, cls.Evaluation)
		assert.True(t, cls.Evaluation.CacheHit, "secondary member %d must have CacheHit=true", idx)
		assert.Nil(t, cls.Evaluation.Usage, "secondary member %d must have nil Usage", idx)
		assert.Equal(t, primaryEval.EvalID, cls.SourceEvalID, "secondary member %d must reference primary EvalID", idx)
		assert.Equal(t, members[idx].PolicyID, cls.Evaluation.Rule.RuleID)
		assert.Equal(t, members[idx].Revision, cls.Evaluation.Rule.Revision)
		assert.NotEmpty(t, cls.Evaluation.EvalID)
		assert.False(t, seenEvalIDs[cls.Evaluation.EvalID], "each member must have a unique EvalID")
		seenEvalIDs[cls.Evaluation.EvalID] = true
	}
}

// Adapted p4 capacity probe (N=3/10/20) for planned groups.
func TestProbe_GroupsCapacityN(t *testing.T) {
	for _, tc := range []struct {
		n      int
		budget time.Duration
	}{
		{3, 600 * time.Millisecond},
		{10, 600 * time.Millisecond},
		{10, 380 * time.Millisecond},
		{20, 600 * time.Millisecond},
	} {
		inst := defaultGroupInstance("inst-a")
		inst.Limits.MaxInFlight = 1
		holderIn := make(chan struct{})
		release := make(chan struct{})
		var once sync.Once
		h := newGroupTestHarness(t, []systemoneruntime.Instance{inst}, nil, func(ctx context.Context, _ provider.Transport, iv provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			once.Do(func() { close(holderIn) })
			select {
			case <-release:
			case <-ctx.Done():
			}
			return okBinaryGroupResult(iv.Model, false), nil
		})
		holderDone := make(chan struct{})
		go func() {
			defer close(holderDone)
			h.ev.EvaluateSystemOne(context.Background(), groupRuleBodyOnly, systemone.Request{
				ProjectID: "proj-1",
				RuleID:    "holder",
				Input:     messagepolicy.BuildSystemOneInput(defaultGroupFacts("holder")),
			}, messagepolicy.WithMaxEvalTime(3*time.Second))
		}()
		<-holderIn

		members := make([]groupMemberSpec, tc.n)
		for i := 0; i < tc.n; i++ {
			members[i] = groupMemberSpec{
				PolicyID:      fmt.Sprintf("pol-%02d", i),
				PolicyName:    fmt.Sprintf("name-%02d", i),
				Revision:      1,
				SystemOneRule: groupRuleBodyOnly,
				ViolationRule: `judgment_flags.asks_choice`,
			}
		}

		soCtx, cancel := context.WithTimeout(context.Background(), tc.budget)
		start := time.Now()
		res, _ := evalPlannedGroups(soCtx, h.ev, members, "proj-1", defaultGroupFacts("same"), messagepolicy.NewSendBudget(), tc.budget)
		elapsed := time.Since(start)
		cancel()
		close(release)
		<-holderDone

		reasons := make([]string, tc.n)
		for i, r := range res {
			reasons[i] = r.Classification.Class + "/" + r.Classification.Reason
			assert.Equal(t, messagepolicy.ClassFailed, r.Classification.Class)
			assert.Equal(t, string(provider.FailCapacity), r.Classification.Reason)
		}
		t.Logf("groups capacity probe: budget=%v N=%d elapsed=%v provider_calls=%d reasons[0]=%s", tc.budget, tc.n, elapsed, h.calls.Load(), reasons[0])
	}
}

// Adapted p4 CallTimeout probe for planned groups (no late join / no skew effect).
func TestProbe_GroupsCallTimeoutSkew(t *testing.T) {
	const budget = 600 * time.Millisecond
	const callTimeout = 200 * time.Millisecond
	const n = 3

	for _, skew := range []time.Duration{5 * time.Millisecond, 60 * time.Millisecond} {
		inst := defaultGroupInstance("inst-a")
		inst.Limits.CallTimeout = callTimeout
		h := newGroupTestHarness(t, []systemoneruntime.Instance{inst}, nil, func(ctx context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			<-ctx.Done()
			return provider.ModelResult{}, errors.Join(ctx.Err())
		})

		members := make([]groupMemberSpec, n)
		for i := 0; i < n; i++ {
			members[i] = groupMemberSpec{
				PolicyID:      fmt.Sprintf("pol-%d", i),
				PolicyName:    fmt.Sprintf("name-%d", i),
				Revision:      1,
				SystemOneRule: groupRuleBodyOnly,
				ViolationRule: `judgment_flags.asks_choice`,
			}
		}

		soCtx, cancel := context.WithTimeout(context.Background(), budget)
		start := time.Now()
		res, _ := evalPlannedGroups(soCtx, h.ev, members, "proj-1", defaultGroupFacts("same"), messagepolicy.NewSendBudget(), budget)
		elapsed := time.Since(start)
		cancel()

		reasons := make([]string, n)
		lats := make([]time.Duration, n)
		for i, r := range res {
			reasons[i] = r.Classification.Class + "/" + r.Classification.Reason
			lats[i] = r.Elapsed
			assert.Equal(t, messagepolicy.ClassFailed, r.Classification.Class)
			assert.Equal(t, string(provider.FailTimeout), r.Classification.Reason)
		}
		assert.Equal(t, int32(1), h.calls.Load())
		t.Logf("groups calltimeout probe: skew=%v elapsed=%v reasons=%v member_latencies=%v", skew, elapsed, reasons, lats)
	}
}
