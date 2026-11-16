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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/egress"
	systemoneruntime "github.com/GoogleCloudPlatform/scion/pkg/systemone/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
)

const testSystemOneConsumer = "system_one_test"

func setSystemOneExperimentLive(t *testing.T, srv *Server, enabled bool) {
	t.Helper()
	ops := srv.GetOperationalSettings()
	doc := fmt.Sprintf(`{"overrides":{%q:%t}}`, experiments.SystemOne, enabled)
	_, err := ops.Update(
		context.Background(),
		"experiments",
		json.RawMessage(doc),
		"test-admin",
		ops.SectionRevision("experiments"),
		"managed",
	)
	require.NoError(t, err)
}

func TestSystemOneRules_LiveExperimentToggleAndProbeParity(t *testing.T) {
	ctx := context.Background()

	// Start one Hub Server process with hub.system_one initially OFF.
	srv, s, _ := newSystemOneTestServer(t, false)
	admin, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-reg-admin", store.SystemRoleSuperAdmin))
	require.NoError(t, err)

	const apiKey = "sk-offline-hub-reg-test-5566"
	var providerCalls atomic.Int32

	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
		assert.Equal(t, "Bearer "+apiKey, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-TypeSafe-Request-Id", "req_hub_reg_01")
		_, _ = w.Write([]byte(`{
		  "model": "jev-1.14.0",
		  "answers": {
		    "is_outage": {
		      "type": "noul",
		      "noul": 0.89
		    },
		    "service": {
		      "type": "choice",
		      "choice": "auth",
		      "confidence": 0.81,
		      "probabilities": {"auth": 0.78, "billing": 0.14, "other": 0.08}
		    }
		  },
		  "usage": {"input_tokens": 30, "output_tokens": 4}
		}`))
	}))
	t.Cleanup(tlsSrv.Close)

	srv.setSystemOneRuntimeOptions(
		systemoneruntime.WithEgressOptionsForTesting(
			egress.WithLoopbackServerForTesting(tlsSrv.URL, tlsSrv.Client()),
		),
	)

	const rawSpec = `{
	  "version": 1,
	  "questions": [
	    {"id": "is_outage", "type": "binary", "instructions": "Does the text report a service outage?"},
	    {
	      "id": "service",
	      "type": "choice",
	      "instructions": "Which service is affected?",
	      "options": [
	        {"id": "auth", "description": "Authentication"},
	        {"id": "billing", "description": "Billing"},
	        {"id": "other", "description": "Other"}
	      ]
	    }
	  ],
	  "output": {"question": "service"}
	}`
	const syntheticText = "Synthetic token exchange failures observed on login."

	re := srv.RuleEngine()
	require.NotNil(t, re)
	assert.Contains(t, re.Dialects(), systemone.DialectName)
	require.NoError(t, re.AllowConsumerDialects(
		testSystemOneConsumer,
		ruleengine.GrantIO(systemone.DialectName),
		ruleengine.Grant(ruleengine.DialectCEL),
	))

	// Phase 1: Initially OFF — Supports stays true for the test-granted consumer, admin routes 404, Compile returns UnsupportedDialectError.
	assert.True(t, re.Supports(testSystemOneConsumer, systemone.DialectName))
	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, srv, admin, http.MethodGet, systemOneAdminPath, nil).Code)
	_, err = re.Compile(ctx, systemone.DialectName, rawSpec, ruleengine.Kind(spec.KindRoute), ruleengine.EnvironmentRef{
		Consumer: testSystemOneConsumer,
		Name:     spec.ProbeEnvironmentName,
	})
	require.Error(t, err)
	var unsupErr *ruleengine.UnsupportedDialectError
	require.ErrorAs(t, err, &unsupErr)
	assert.Equal(t, systemone.DialectName, unsupErr.Dialect)
	assert.Equal(t, testSystemOneConsumer, unsupErr.Consumer)
	assert.Equal(t, ruleengine.ReasonUnknownDialect, unsupErr.Reason)
	assert.Equal(t, `unsupported dialect "system_one" for consumer "system_one_test": unknown_dialect`, err.Error())

	// Phase 2: Flip hub.system_one ON in the same Hub process without restart.
	setSystemOneExperimentLive(t, srv, true)

	createRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":    "ts-reg-live",
		"adapter": "typesafe",
		"model":   "jev-1.14.0",
		"auth":    map[string]any{"kind": "api_key", "apiKey": apiKey},
	})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())

	instRow, err := s.GetSystemOneInstanceByName(ctx, "ts-reg-live")
	require.NoError(t, err)
	instRow.Endpoint = tlsSrv.URL
	require.NoError(t, s.UpdateSystemOneInstance(ctx, instRow))

	// Evaluate via admin /probe endpoint.
	probeRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/ts-reg-live/probe", ProbeSystemOneInstanceRequest{
		Kind:  string(spec.KindRoute),
		Spec:  json.RawMessage(rawSpec),
		Probe: &ProbeSystemOneInput{Text: syntheticText},
	})
	require.Equal(t, http.StatusOK, probeRec.Code, probeRec.Body.String())
	var probeResp ProbeSystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(probeRec.Body.Bytes(), &probeResp))
	require.Equal(t, systemoneruntime.ClassSuccess, probeResp.Class)
	require.NotNil(t, probeResp.Decision)
	require.NotNil(t, probeResp.Decision.String)
	assert.Equal(t, "auth", *probeResp.Decision.String)
	require.NotNil(t, probeResp.Evaluation)
	assert.Equal(t, int32(1), providerCalls.Load())

	// Compile and evaluate the identical spec via srv.RuleEngine().
	compiledWhileOn, err := re.Compile(ctx, systemone.DialectName, rawSpec, ruleengine.Kind(spec.KindRoute), ruleengine.EnvironmentRef{
		Consumer: testSystemOneConsumer,
		Name:     spec.ProbeEnvironmentName,
	})
	require.NoError(t, err)

	engDec, err := re.Evaluate(ctx, compiledWhileOn, ruleengine.Input{
		Payload: systemone.ProbeInput(syntheticText),
	}, ruleengine.Budget{})
	require.NoError(t, err)
	assert.Equal(t, ruleengine.Kind(spec.KindRoute), engDec.Kind)
	assert.Equal(t, "auth", engDec.String)
	assert.Equal(t, int32(2), providerCalls.Load())

	engEval, ok := engDec.Detail.(*systemone.Evaluation)
	require.True(t, ok)
	assert.Equal(t, probeResp.Evaluation.Class, engEval.Class)
	assert.Equal(t, probeResp.Evaluation.Rule.ContentHash, engEval.Rule.ContentHash)
	assert.Equal(t, probeResp.Evaluation.ConfiguredModel, engEval.ConfiguredModel)
	assert.Equal(t, probeResp.Evaluation.ResolvedModel, engEval.ResolvedModel)
	assert.Equal(t, probeResp.Evaluation.Input, engEval.Input)
	assert.Equal(t, probeResp.Evaluation.Answers, engEval.Answers)
	assert.Contains(t, engEval.Answers["service"].NativeMetrics, "typesafe.confidence")
	require.Len(t, engEval.Attempts, 1)

	// Phase 3: Flip hub.system_one OFF in the same Hub process without restart.
	setSystemOneExperimentLive(t, srv, false)

	assert.True(t, re.Supports(testSystemOneConsumer, systemone.DialectName),
		"Engine.Supports reports registration and consumer grant only and remains true when experiment is off")
	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, srv, admin, http.MethodGet, systemOneAdminPath, nil).Code)

	_, err = re.Compile(ctx, systemone.DialectName, rawSpec, ruleengine.Kind(spec.KindRoute), ruleengine.EnvironmentRef{
		Consumer: testSystemOneConsumer,
		Name:     spec.ProbeEnvironmentName,
	})
	require.Error(t, err)
	var unsupOff *ruleengine.UnsupportedDialectError
	require.ErrorAs(t, err, &unsupOff)
	assert.Equal(t, systemone.DialectName, unsupOff.Dialect)
	assert.Equal(t, testSystemOneConsumer, unsupOff.Consumer)
	assert.Equal(t, ruleengine.ReasonUnknownDialect, unsupOff.Reason)
	assert.Equal(t, `unsupported dialect "system_one" for consumer "system_one_test": unknown_dialect`, err.Error())

	_, err = re.Evaluate(ctx, compiledWhileOn, ruleengine.Input{
		Payload: systemone.ProbeInput(syntheticText),
	}, ruleengine.Budget{})
	require.Error(t, err)
	var failedOff *ruleengine.FailedError
	require.ErrorAs(t, err, &failedOff)
	assert.Equal(t, systemone.ReasonDialectDisabled, failedOff.Reason)
	var causeUnsup *ruleengine.UnsupportedDialectError
	require.ErrorAs(t, failedOff.Cause, &causeUnsup)
	assert.Equal(t, systemone.DialectName, causeUnsup.Dialect)
	assert.Equal(t, int32(2), providerCalls.Load(),
		"evaluating a rule compiled while on after flipping experiment off must make zero provider calls")

	// Phase 4: Flip hub.system_one back ON in the same Hub process without restart.
	setSystemOneExperimentLive(t, srv, true)

	assert.Equal(t, http.StatusOK, doRequestAsUser(t, srv, admin, http.MethodGet, systemOneAdminPath, nil).Code)
	_, err = re.Compile(ctx, systemone.DialectName, rawSpec, ruleengine.Kind(spec.KindRoute), ruleengine.EnvironmentRef{
		Consumer: testSystemOneConsumer,
		Name:     spec.ProbeEnvironmentName,
	})
	require.NoError(t, err)

	engDecAgain, err := re.Evaluate(ctx, compiledWhileOn, ruleengine.Input{
		Payload: systemone.ProbeInput(syntheticText),
	}, ruleengine.Budget{})
	require.NoError(t, err)
	assert.Equal(t, "auth", engDecAgain.String)
	assert.Equal(t, int32(3), providerCalls.Load())
}

func TestSystemOneRules_DefaultDenyNoConsumerAdmitted(t *testing.T) {
	ctx := context.Background()

	// Start a Hub Server with hub.system_one ON and an enabled instance backed by
	// an in-test TLS server, proving default-deny blocks admission with zero
	// outbound provider calls even when the experiment is enabled.
	srv, s, _ := newSystemOneTestServer(t, true)
	admin, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-deny-admin", store.SystemRoleSuperAdmin))
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

	createRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":    "ts-default-deny",
		"adapter": "typesafe",
		"model":   "jev-1.14.0",
		"auth":    map[string]any{"kind": "api_key", "apiKey": "sk-offline-default-deny-0001"},
	})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())

	instRow, err := s.GetSystemOneInstanceByName(ctx, "ts-default-deny")
	require.NoError(t, err)
	instRow.Endpoint = tlsSrv.URL
	require.NoError(t, s.UpdateSystemOneInstance(ctx, instRow))

	const rawSpec = `{
	  "version": 1,
	  "questions": [{"id": "q1", "type": "binary", "instructions": "Does this report an outage?"}],
	  "output": {"question": "q1"}
	}`

	re := srv.RuleEngine()
	require.NotNil(t, re)
	assert.Contains(t, re.Dialects(), systemone.DialectName)

	// 1. Empty/whitespace consumer -> empty_consumer.
	for _, emptyConsumer := range []string{"", "   "} {
		assert.False(t, re.Supports(emptyConsumer, systemone.DialectName))
		_, err := re.Compile(ctx, systemone.DialectName, rawSpec, ruleengine.KindFilter, ruleengine.EnvironmentRef{
			Consumer: emptyConsumer,
			Name:     spec.ProbeEnvironmentName,
		})
		var unsup *ruleengine.UnsupportedDialectError
		require.ErrorAs(t, err, &unsup)
		assert.Equal(t, ruleengine.ReasonEmptyConsumer, unsup.Reason)

		forged := ruleengine.NewCompiled(emptyConsumer, systemone.DialectName, ruleengine.KindFilter, struct{}{})
		_, err = re.Evaluate(ctx, forged, ruleengine.Input{Payload: systemone.ProbeInput("text")}, ruleengine.Budget{})
		unsup = nil
		require.ErrorAs(t, err, &unsup)
		assert.Equal(t, ruleengine.ReasonEmptyConsumer, unsup.Reason)
	}

	// 2. P1 registers no consumer allow-list: every named consumer is denied by default.
	for _, consumer := range []string{"trigger", "message", "probe", "admin-probe", "system_one", testSystemOneConsumer} {
		assert.False(t, re.Supports(consumer, systemone.DialectName), "consumer %q must not be admitted to system_one by default", consumer)
		_, err := re.Compile(ctx, systemone.DialectName, rawSpec, ruleengine.KindFilter, ruleengine.EnvironmentRef{
			Consumer: consumer,
			Name:     spec.ProbeEnvironmentName,
		})
		var unsup *ruleengine.UnsupportedDialectError
		require.ErrorAs(t, err, &unsup)
		assert.Equal(t, systemone.DialectName, unsup.Dialect)
		assert.Equal(t, consumer, unsup.Consumer)
		assert.Equal(t, ruleengine.ReasonConsumerNotRegistered, unsup.Reason)

		forged := ruleengine.NewCompiled(consumer, systemone.DialectName, ruleengine.KindFilter, struct{}{})
		_, err = re.Evaluate(ctx, forged, ruleengine.Input{Payload: systemone.ProbeInput("text")}, ruleengine.Budget{})
		unsup = nil
		require.ErrorAs(t, err, &unsup)
		assert.Equal(t, ruleengine.ReasonConsumerNotRegistered, unsup.Reason)
	}

	// 3. Granting system_one without AllowIO is also denied with io_not_allowed.
	require.NoError(t, re.AllowConsumerDialects("pure_only_test_consumer", ruleengine.Grant(systemone.DialectName)))
	assert.False(t, re.Supports("pure_only_test_consumer", systemone.DialectName))
	_, err = re.Compile(ctx, systemone.DialectName, rawSpec, ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: "pure_only_test_consumer",
		Name:     spec.ProbeEnvironmentName,
	})
	var unsupIO *ruleengine.UnsupportedDialectError
	require.ErrorAs(t, err, &unsupIO)
	assert.Equal(t, ruleengine.ReasonIONotAllowed, unsupIO.Reason)

	assert.Zero(t, providerCalls.Load(), "denied default admission must never make a provider call")
}
