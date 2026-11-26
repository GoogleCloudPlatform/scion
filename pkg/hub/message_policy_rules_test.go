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

//go:build !no_sqlite

package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/messagepolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/egress"
	systemoneruntime "github.com/GoogleCloudPlatform/scion/pkg/systemone/runtime"
)

type stubRuleDialect struct {
	name       string
	performsIO bool
}

func (d stubRuleDialect) Name() string { return d.name }

func (d stubRuleDialect) Compile(_ context.Context, _ string, kind ruleengine.Kind, env ruleengine.EnvironmentRef) (ruleengine.Compiled, error) {
	return ruleengine.NewCompiled(env.Consumer, d.name, kind, struct{}{}), nil
}

func (d stubRuleDialect) Evaluate(_ context.Context, _ ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
	return ruleengine.Decision{Kind: ruleengine.KindFilter, Bool: true}, nil
}

func (d stubRuleDialect) Declares() ruleengine.Capabilities {
	return ruleengine.Capabilities{
		PerformsIO:  d.performsIO,
		ResultKinds: []ruleengine.Kind{ruleengine.KindFilter},
	}
}

// M2 Admission guard for message_policy consumer registration (ptone/scion#8048,
// comment-6920751463):
// Verifies that registerMessagePolicyRules grants message_policy strictly
// [{Dialect: "cel", AllowIO: false}, {Dialect: "system_one", AllowIO: true}],
// registers the message_policy v1 System One ContentEnvironment, pins a non-nil
// Enabled gate on system_one so turning hub.system_one OFF fails closed with
// zero provider calls, refuses extra dialects and unregistered consumers, fails
// startup closed on registration error, and maps evaluation-time admission
// failure to ClassFailed.
func TestMessagePolicyRules_AdmissionGuard_Target9a(t *testing.T) {
	ctx := context.Background()

	// Start a Hub Server with hub.system_one OFF first to prove the pinned
	// non-nil Enabled gate blocks message_policy System One compile/eval with
	// zero provider calls before toggling hub.system_one ON.
	srv, s, _ := newSystemOneTestServer(t, false)
	admin, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "mp-reg-admin", store.SystemRoleSuperAdmin))
	require.NoError(t, err)

	var providerCalls atomic.Int32
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(tlsSrv.Close)

	srv.setSystemOneRuntimeOptions(
		systemoneruntime.WithEgressOptionsForTesting(
			egress.WithLoopbackServerForTesting(tlsSrv.URL, tlsSrv.Client()),
		),
	)

	re := srv.RuleEngine()
	require.NotNil(t, re)
	require.NotNil(t, srv.CELDialect())
	celEnvEng, ok := srv.CELDialect().CELEngine(messagepolicy.EnvironmentName)
	require.True(t, ok)
	require.NotNil(t, celEnvEng)

	// 1. message_policy is admitted to "cel" (AllowIO: false) and "system_one" (AllowIO: true),
	// and the message_policy v1 ContentEnvironment is registered on the system_one dialect.
	assert.True(t, re.Supports(messagepolicy.ConsumerName, ruleengine.DialectCEL))
	celHandle, err := re.DialectFor(messagepolicy.ConsumerName, ruleengine.DialectCEL)
	require.NoError(t, err)
	assert.Equal(t, ruleengine.DialectCEL, celHandle.Name())
	assert.False(t, celHandle.Declares().PerformsIO)

	assert.True(t, re.Supports(messagepolicy.ConsumerName, systemone.DialectName))
	s1Handle, err := re.DialectFor(messagepolicy.ConsumerName, systemone.DialectName)
	require.NoError(t, err)
	assert.Equal(t, systemone.DialectName, s1Handle.Name())
	assert.True(t, s1Handle.Declares().PerformsIO)
	soDialect, ok := s1Handle.(*systemone.Dialect)
	require.True(t, ok)
	gotEnv, hasEnv := soDialect.Environment(messagepolicy.EnvironmentName)
	require.True(t, hasEnv, "message_policy System One ContentEnvironment must be registered")
	assert.Equal(t, messagepolicy.SystemOneContentEnv(), gotEnv)

	ev, err := messagepolicy.New(re)
	require.NoError(t, err)
	res := ev.EvaluateViolation(ctx, "pol-hub-1", 1, `recipient.kind != "agent" && !message.has_choices`, messagepolicy.NewActivation(messagepolicy.Facts{
		Surface:   messagepolicy.SurfaceAgentToUser,
		Sender:    messagepolicy.SenderFacts{Kind: "agent"},
		Recipient: messagepolicy.RecipientFacts{Kind: "user"},
		Message:   messagepolicy.MessageFacts{Type: "instruction", Body: "Pick option A or B"},
	}))
	assert.Equal(t, messagepolicy.ClassViolation, res.Class)

	// 2. Non-nil Enabled gate fails closed when hub.system_one is OFF (0 provider calls).
	const rawS1Spec = `{
	  "dialect": "system_one",
	  "inputs": ["message.body"],
	  "spec": {
	    "version": 1,
	    "questions": [{"id": "q1", "type": "binary", "instructions": "Does the message ask a prose question?"}]
	  }
	}`
	_, err = re.Compile(ctx, systemone.DialectName, rawS1Spec, messagepolicy.KindJudgments, messagepolicy.SystemOneEnvironmentRef())
	var unsupOff *ruleengine.UnsupportedDialectError
	require.ErrorAs(t, err, &unsupOff)
	assert.Equal(t, systemone.DialectName, unsupOff.Dialect)
	assert.Equal(t, messagepolicy.ConsumerName, unsupOff.Consumer)
	assert.Equal(t, ruleengine.ReasonUnknownDialect, unsupOff.Reason)

	forgedS1 := ruleengine.NewCompiled(messagepolicy.ConsumerName, systemone.DialectName, messagepolicy.KindJudgments, struct{}{})
	_, err = re.Evaluate(ctx, forgedS1, ruleengine.Input{Payload: systemone.ProbeInput("hello")}, ruleengine.Budget{})
	var failedOff *ruleengine.FailedError
	require.ErrorAs(t, err, &failedOff)
	assert.Equal(t, systemone.ReasonDialectDisabled, failedOff.Reason)
	assert.Zero(t, providerCalls.Load(), "disabled hub.system_one must make zero provider calls")

	// Flip hub.system_one ON and seed an instance: compile succeeds for message_policy.
	setSystemOneExperimentLive(t, srv, true)
	createRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":    "ts-mp-guard",
		"adapter": "typesafe",
		"model":   "jev-1.14.0",
		"auth":    map[string]any{"kind": "api_key", "apiKey": "sk-offline-mp-guard-0001"},
	})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())
	instRow, err := s.GetSystemOneInstanceByName(ctx, "ts-mp-guard")
	require.NoError(t, err)
	instRow.Endpoint = tlsSrv.URL
	require.NoError(t, s.UpdateSystemOneInstance(ctx, instRow))

	compiledS1, err := ev.CompileSystemOne(ctx, rawS1Spec)
	require.NoError(t, err)
	assert.False(t, compiledS1.IsZero())

	// 3. Any additional pure or I/O dialect registered on the engine is refused for message_policy,
	// confirming grants are strictly ["cel", "system_one"].
	for _, extra := range []stubRuleDialect{
		{name: "extra_pure", performsIO: false},
		{name: "extra_io", performsIO: true},
	} {
		require.NoError(t, re.Register(extra))
		assert.False(t, re.Supports(messagepolicy.ConsumerName, extra.name))
		_, err = re.DialectFor(messagepolicy.ConsumerName, extra.name)
		var unsupExtra *ruleengine.UnsupportedDialectError
		require.ErrorAs(t, err, &unsupExtra)
		assert.Equal(t, ruleengine.ReasonDialectNotAllowed, unsupExtra.Reason)
	}

	// 4. Prove the "cel" grant registered by registerMessagePolicyRules has AllowIO == false:
	// if "cel" declared PerformsIO: true, message_policy's grant rejects it with io_not_allowed.
	ioTestEngine := ruleengine.NewEngine()
	require.NoError(t, ioTestEngine.Register(stubRuleDialect{name: ruleengine.DialectCEL, performsIO: true}))
	require.NoError(t, ioTestEngine.Register(systemone.NewDialect(systemone.DialectConfig{Enabled: func() bool { return true }})))
	ioTestCEL, err := ruleengine.NewCELDialect()
	require.NoError(t, err)
	ioTestSrv := &Server{rules: ioTestEngine, celDialect: ioTestCEL}
	require.NoError(t, ioTestSrv.registerMessagePolicyRules())
	assert.False(t, ioTestEngine.Supports(messagepolicy.ConsumerName, ruleengine.DialectCEL))
	_, err = ioTestEngine.DialectFor(messagepolicy.ConsumerName, ruleengine.DialectCEL)
	var unsupIO *ruleengine.UnsupportedDialectError
	require.ErrorAs(t, err, &unsupIO)
	assert.Equal(t, ruleengine.DialectCEL, unsupIO.Dialect)
	assert.Equal(t, messagepolicy.ConsumerName, unsupIO.Consumer)
	assert.Equal(t, ruleengine.ReasonIONotAllowed, unsupIO.Reason)

	// 5. Unregistered consumer is refused when compiling against the message_policy CEL environment.
	assert.False(t, re.Supports("unregistered_consumer", ruleengine.DialectCEL))
	_, err = re.Compile(ctx, ruleengine.DialectCEL, `true`, ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: "unregistered_consumer",
		Name:     messagepolicy.EnvironmentName,
	})
	var unsupUnreg *ruleengine.UnsupportedDialectError
	require.ErrorAs(t, err, &unsupUnreg)
	assert.Equal(t, ruleengine.DialectCEL, unsupUnreg.Dialect)
	assert.Equal(t, "unregistered_consumer", unsupUnreg.Consumer)
	assert.Equal(t, ruleengine.ReasonConsumerNotRegistered, unsupUnreg.Reason)

	// 6. Startup fails closed if registerMessagePolicyRules runs before newRuleEngine or runs twice.
	uninitSrv := &Server{}
	require.Error(t, uninitSrv.registerMessagePolicyRules())
	require.Error(t, srv.registerMessagePolicyRules(), "duplicate environment registration must fail closed")

	// 7. Evaluation-time admission failure maps to ClassFailed (never pass/admit by default).
	compiledCEL, err := re.Compile(ctx, ruleengine.DialectCEL, `true`, messagepolicy.KindViolation, messagepolicy.ViolationEnvironmentRef())
	require.NoError(t, err)
	require.NoError(t, re.AllowConsumerDialects(messagepolicy.ConsumerName)) // revoke all dialect grants

	_, evalErr := re.Evaluate(ctx, compiledCEL, ruleengine.Input{
		Activation: messagepolicy.NewActivation(messagepolicy.Facts{Surface: messagepolicy.SurfaceAgentToUser}).Map(),
	}, ruleengine.Budget{})
	require.Error(t, evalErr)
	classified := messagepolicy.ClassifyError(evalErr)
	assert.Equal(t, messagepolicy.ClassFailed, classified.Class)
	assert.Equal(t, ruleengine.ReasonDialectNotAllowed, classified.Reason)
	assert.True(t, classified.AdmissionError)
	assert.Equal(t, messagepolicy.OutcomeReject, messagepolicy.ResolveOutcome(
		classified.Class, classified.Reason, "", messagepolicy.OutcomeReject, nil,
	))

	evalRes := ev.EvaluateViolation(ctx, "pol-revoked", 2, `true`, messagepolicy.NewActivation(messagepolicy.Facts{
		Surface: messagepolicy.SurfaceAgentToUser,
	}))
	assert.Equal(t, messagepolicy.ClassFailed, evalRes.Class)
	assert.Equal(t, ruleengine.ReasonDialectNotAllowed, evalRes.Reason)
	assert.True(t, evalRes.AdmissionError)
}
