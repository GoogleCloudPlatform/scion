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
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/egress"
	systemoneprovider "github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
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
	  "inputs": ["probe.text"],
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
	const testEnvName = "system_one_test_env"
	require.NoError(t, systemone.RegisterConsumerEnvironment(re, testSystemOneConsumer, spec.ContentEnvironment{
		Name:    testEnvName,
		Version: 1,
		Fields: []spec.FieldDecl{
			{Name: spec.ProbeFieldText, Kind: spec.FieldKindText, MaxBytes: spec.ProbeMaxBytes, Trust: spec.TrustUntrusted},
		},
		MaxTotalBytes: spec.ProbeMaxBytes,
	}))

	proj := &store.Project{
		ID:      "00000000-0000-0000-0000-00000000d100",
		Name:    "System One Live Parity Project",
		Slug:    "s1-live-parity-proj",
		OwnerID: admin.ID,
	}
	require.NoError(t, s.CreateProject(ctx, proj))

	// Phase 1: Initially OFF — Supports stays true for the test-granted consumer, admin routes 404, Compile returns UnsupportedDialectError.
	assert.True(t, re.Supports(testSystemOneConsumer, systemone.DialectName))
	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, srv, admin, http.MethodGet, systemOneAdminPath, nil).Code)
	_, err = re.Compile(ctx, systemone.DialectName, rawSpec, ruleengine.Kind(spec.KindRoute), ruleengine.EnvironmentRef{
		Consumer: testSystemOneConsumer,
		Name:     testEnvName,
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
	_, _, err = s.AddProjectSystemOneInstance(ctx, proj.ID, "ts-reg-live")
	require.NoError(t, err)

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
		Name:     testEnvName,
	})
	require.NoError(t, err)

	engDec, err := re.Evaluate(ctx, compiledWhileOn, ruleengine.Input{
		Payload: systemone.Request{
			Input:     systemone.ProbeInput(syntheticText),
			ProjectID: proj.ID,
		},
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
		Name:     testEnvName,
	})
	require.Error(t, err)
	var unsupOff *ruleengine.UnsupportedDialectError
	require.ErrorAs(t, err, &unsupOff)
	assert.Equal(t, systemone.DialectName, unsupOff.Dialect)
	assert.Equal(t, testSystemOneConsumer, unsupOff.Consumer)
	assert.Equal(t, ruleengine.ReasonUnknownDialect, unsupOff.Reason)
	assert.Equal(t, `unsupported dialect "system_one" for consumer "system_one_test": unknown_dialect`, err.Error())

	_, err = re.Evaluate(ctx, compiledWhileOn, ruleengine.Input{
		Payload: systemone.Request{
			Input:     systemone.ProbeInput(syntheticText),
			ProjectID: proj.ID,
		},
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
		Name:     testEnvName,
	})
	require.NoError(t, err)

	engDecAgain, err := re.Evaluate(ctx, compiledWhileOn, ruleengine.Input{
		Payload: systemone.Request{
			Input:     systemone.ProbeInput(syntheticText),
			ProjectID: proj.ID,
		},
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
	  "inputs": ["probe.text"],
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

		h, err := re.DialectFor(emptyConsumer, systemone.DialectName)
		assert.Nil(t, h)
		unsup = nil
		require.ErrorAs(t, err, &unsup)
		assert.Equal(t, ruleengine.ReasonEmptyConsumer, unsup.Reason)
	}

	// 2. No consumer is admitted to system_one by default: ungranted consumers fail
	// with consumer_not_registered, and "trigger" (registered with pure Grant("cel")
	// only) fails with dialect_not_allowed.
	for _, consumer := range []string{"trigger", "message", "probe", "admin-probe", "system_one", testSystemOneConsumer} {
		wantReason := ruleengine.ReasonConsumerNotRegistered
		if consumer == "trigger" {
			wantReason = ruleengine.ReasonDialectNotAllowed
		}
		assert.False(t, re.Supports(consumer, systemone.DialectName), "consumer %q must not be admitted to system_one by default", consumer)
		_, err := re.Compile(ctx, systemone.DialectName, rawSpec, ruleengine.KindFilter, ruleengine.EnvironmentRef{
			Consumer: consumer,
			Name:     spec.ProbeEnvironmentName,
		})
		var unsup *ruleengine.UnsupportedDialectError
		require.ErrorAs(t, err, &unsup)
		assert.Equal(t, systemone.DialectName, unsup.Dialect)
		assert.Equal(t, consumer, unsup.Consumer)
		assert.Equal(t, wantReason, unsup.Reason)

		forged := ruleengine.NewCompiled(consumer, systemone.DialectName, ruleengine.KindFilter, struct{}{})
		_, err = re.Evaluate(ctx, forged, ruleengine.Input{Payload: systemone.ProbeInput("text")}, ruleengine.Budget{})
		unsup = nil
		require.ErrorAs(t, err, &unsup)
		assert.Equal(t, wantReason, unsup.Reason)

		h, err := re.DialectFor(consumer, systemone.DialectName)
		assert.Nil(t, h)
		unsup = nil
		require.ErrorAs(t, err, &unsup)
		assert.Equal(t, wantReason, unsup.Reason)
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

	hIO, err := re.DialectFor("pure_only_test_consumer", systemone.DialectName)
	assert.Nil(t, hIO)
	unsupIO = nil
	require.ErrorAs(t, err, &unsupIO)
	assert.Equal(t, ruleengine.ReasonIONotAllowed, unsupIO.Reason)

	assert.Zero(t, providerCalls.Load(), "denied default admission must never make a provider call")
}

type hubDialectPublicResolver struct{}

func (hubDialectPublicResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
}

type hubDialectTargetDialer struct {
	targetAddr string
}

func (d hubDialectTargetDialer) DialContext(ctx context.Context, network, _ string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, network, d.targetAddr)
}

func TestSystemOneRules_DialectForwardsProjectIDForProjectScopedNamedSecret(t *testing.T) {
	ctx := context.Background()

	srv, s, _ := newSystemOneTestServer(t, true)
	admin, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-projsec-admin", store.SystemRoleSuperAdmin))
	require.NoError(t, err)

	const (
		projAPIKey = "AIza-project-scoped-dialect-canary-9911"
	)

	proj := &store.Project{
		ID:      "00000000-0000-0000-0000-00000000d101",
		Name:    "Dialect Secret Project",
		Slug:    "dialect-secret-project",
		OwnerID: admin.ID,
	}
	require.NoError(t, s.CreateProject(ctx, proj))
	projID := proj.ID

	// Store a project-scoped GEMINI_API_KEY variable secret in the Hub secret backend.
	_, _, err = srv.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name:       systemoneprovider.DefaultGeminiAPIKeySecretName,
		Value:      projAPIKey,
		SecretType: store.SecretTypeVariable,
		Scope:      store.ScopeProject,
		ScopeID:    projID,
	})
	require.NoError(t, err)

	var (
		providerCalls atomic.Int32
		seenKeyHeader string
	)
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
		seenKeyHeader = r.Header.Get("x-goog-api-key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
		  "answers": {"q1": {"type": "noul", "noul": 0.95, "confidence": 0.95}},
		  "usage": {"total_input_tokens": 14, "total_output_tokens": 0, "total_tokens": 14}
		}`))
	}))
	t.Cleanup(tlsSrv.Close)

	srv.setSystemOneRuntimeOptions(
		systemoneruntime.WithEgressOptionsForTesting(
			egress.WithDialerForTesting(hubDialectPublicResolver{}, hubDialectTargetDialer{targetAddr: tlsSrv.Listener.Addr().String()}),
			egress.WithTLSConfigForTesting(&tls.Config{InsecureSkipVerify: true}), //nolint:gosec // offline test TLS server
		),
	)

	createRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":    "gemini-proj-sec",
		"adapter": "gemini_decisions",
		"model":   "gemini-2.5-flash",
		"auth": map[string]any{
			"kind": "api_key",
			"secretRef": map[string]any{
				"name":  systemoneprovider.DefaultGeminiAPIKeySecretName,
				"scope": systemoneprovider.SecretScopeProject,
			},
		},
	})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())

	re := srv.RuleEngine()
	require.NotNil(t, re)
	require.NoError(t, re.AllowConsumerDialects(testSystemOneConsumer, ruleengine.GrantIO(systemone.DialectName)))
	const testEnvName = "system_one_test_projsec_env"
	require.NoError(t, systemone.RegisterConsumerEnvironment(re, testSystemOneConsumer, spec.ContentEnvironment{
		Name:    testEnvName,
		Version: 1,
		Fields: []spec.FieldDecl{
			{Name: spec.ProbeFieldText, Kind: spec.FieldKindText, MaxBytes: spec.ProbeMaxBytes, Trust: spec.TrustUntrusted},
		},
		MaxTotalBytes: spec.ProbeMaxBytes,
	}))

	const rawRule = `{
	  "version": 1,
	  "inputs": ["probe.text"],
	  "binding": {"instance": "gemini-proj-sec"},
	  "questions": [{"id": "q1", "type": "binary", "instructions": "Does the text report an outage?"}],
	  "output": {"question": "q1"}
	}`
	compiled, err := re.Compile(ctx, systemone.DialectName, rawRule, ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: testSystemOneConsumer,
		Name:     testEnvName,
	})
	require.NoError(t, err)

	// 1. Bare Input (no ProjectID) fails closed with no_eligible_instance (0 provider calls).
	_, err = re.Evaluate(ctx, compiled, ruleengine.Input{
		Payload: systemone.ProbeInput("no project id"),
	}, ruleengine.Budget{})
	require.Error(t, err)
	var failedErr *ruleengine.FailedError
	require.ErrorAs(t, err, &failedErr)
	assert.Equal(t, string(systemoneprovider.FailNoEligibleInstance), failedErr.Reason)
	assert.Zero(t, providerCalls.Load())

	// 2. Allowlist the instance for projID, then Request with authorized ProjectID
	// resolves the project-scoped secret through the dialect.
	_, _, err = s.AddProjectSystemOneInstance(ctx, projID, "gemini-proj-sec")
	require.NoError(t, err)

	elemIdx := 1
	dec, err := re.Evaluate(ctx, compiled, ruleengine.Input{
		Payload: systemone.Request{
			Input:        systemone.ProbeInput("synthetic outage"),
			ProjectID:    projID,
			RuleID:       "rule-hub-proj-sec",
			Revision:     3,
			ElementIndex: &elemIdx,
		},
	}, ruleengine.Budget{})
	require.NoError(t, err)
	assert.True(t, dec.Bool)
	assert.Equal(t, int32(1), providerCalls.Load())
	assert.Equal(t, projAPIKey, seenKeyHeader)

	eval, ok := dec.Detail.(*systemone.Evaluation)
	require.True(t, ok)
	assert.Equal(t, projID, eval.ProjectID)
	assert.Equal(t, "rule-hub-proj-sec", eval.Rule.RuleID)
	assert.Equal(t, int64(3), eval.Rule.Revision)
	require.NotNil(t, eval.Rule.ElementIndex)
	assert.Equal(t, 1, *eval.Rule.ElementIndex)
	require.NotNil(t, eval.Auth)
	require.NotNil(t, eval.Auth.SecretRef)
	assert.Equal(t, systemoneprovider.SecretScopeProject, eval.Auth.SecretRef.ScopeUsed)
}
