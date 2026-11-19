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

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/egress"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider/geminidecisions"
	systemoneruntime "github.com/GoogleCloudPlatform/scion/pkg/systemone/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
	"github.com/knadh/koanf/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const inProcessSystemOneHubID = "system-one-test-hub"

type inProcessSystemOneHub struct {
	server       *hub.Server
	httpServer   *httptest.Server
	client       hubclient.Client
	secretStore  *secret.LocalBackend
	devAuthToken string
}

// startInProcessSystemOneHub boots a real Hub server on loopback with SQLite,
// a local internal secret backend, dev-token session authority, and the
// hub.system_one experiment enabled. Shared by the offline CLI/Hub round-trip
// test and the manual systemone_live harness.
func startInProcessSystemOneHub(t *testing.T) *inProcessSystemOneHub {
	t.Helper()
	ctx := context.Background()

	s := newTestStore(t)
	devToken, err := apiclient.GenerateDevToken()
	require.NoError(t, err)

	srv, err := hub.New(hub.ServerConfig{
		HubID:        inProcessSystemOneHubID,
		DevAuthToken: devToken,
	}, s)
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	localBackend := secret.NewLocalBackend(s, inProcessSystemOneHubID, "system-one-test-signing-key")
	srv.SetSecretBackend(localBackend)

	ops := hub.NewOperationalSettings(s.(store.HubSettingStore), koanf.New("."), koanf.New("."))
	_, err = ops.Refresh(ctx)
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)

	expDoc := fmt.Sprintf(`{"overrides":{%q:true}}`, experiments.SystemOne)
	_, err = ops.Update(ctx, "experiments", json.RawMessage(expDoc), "test-admin", ops.SectionRevision("experiments"), "managed")
	require.NoError(t, err)

	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	client, err := hubclient.New(httpSrv.URL, hubclient.WithDevToken(devToken))
	require.NoError(t, err)

	return &inProcessSystemOneHub{
		server:       srv,
		httpServer:   httpSrv,
		client:       client,
		secretStore:  localBackend,
		devAuthToken: devToken,
	}
}

// buildSystemOneLiveProbeSpec builds and validates the AC-12 inline portable
// probe specification (one binary question and one three-option choice question
// over synthetic probe.text).
func buildSystemOneLiveProbeSpec(t *testing.T) json.RawMessage {
	t.Helper()
	probeSpec := spec.Spec{
		Version: spec.CurrentVersion,
		Form:    spec.FormPortable,
		Questions: []spec.Question{
			{
				ID:           "mentions_tests",
				Type:         spec.PrimitiveBinary,
				Instructions: "Does probe.text describe automated test results?",
				Criteria: &spec.BinaryCriteria{
					True:  "The text mentions automated tests or test outcomes.",
					False: "The text does not mention automated tests.",
				},
			},
			{
				ID:           "build_outcome",
				Type:         spec.PrimitiveChoice,
				Instructions: "Classify the build outcome reported in probe.text.",
				Options: []spec.Option{
					{ID: "passed", Description: "The build and tests succeeded."},
					{ID: "failed", Description: "The build or tests failed."},
					{ID: "unknown", Description: "No build outcome is stated."},
				},
			},
		},
	}
	raw, err := json.Marshal(probeSpec)
	require.NoError(t, err)
	_, err = spec.Compile(raw, spec.CompileOptions{Kind: spec.KindJudgments})
	require.NoError(t, err)
	return json.RawMessage(raw)
}

func TestAdminSystemOne_InProcessHubRoundTrip(t *testing.T) {
	resetAdminSystemOneFlags(t)
	env := startInProcessSystemOneHub(t)

	// Verify the AC-12 spec compiles cleanly offline too.
	_ = buildSystemOneLiveProbeSpec(t)

	systemOneAdminClientLoader = func() (hubclient.SystemOneService, error) {
		return env.client.SystemOne(), nil
	}

	const offlineSyntheticKey = "offline-synthetic-key-never-sent-externally"

	createOut, createErr, err := executeAdminSystemOneSubcmd(t, []string{
		"create", "typesafe-local",
		"--adapter", "typesafe",
		"--model", "jev-1.14.0",
		"--key-stdin",
	}, strings.NewReader(offlineSyntheticKey+"\n"))
	require.NoError(t, err)
	assert.Contains(t, createOut, "Created System One instance typesafe-local")
	assert.NotContains(t, createOut, offlineSyntheticKey)
	assert.NotContains(t, createErr, offlineSyntheticKey)

	// Confirm the credential landed in the Hub's internal secret store.
	inst, err := env.client.SystemOne().GetInstance(context.Background(), "typesafe-local")
	require.NoError(t, err)
	assert.Equal(t, "https://api.typesafe.ai", inst.Endpoint)
	require.NotEmpty(t, inst.Auth.SecretName)

	sv, err := env.secretStore.Get(context.Background(), inst.Auth.SecretName, store.ScopeHub, inProcessSystemOneHubID)
	require.NoError(t, err)
	assert.Equal(t, store.SecretTypeInternal, sv.SecretType)
	assert.Equal(t, offlineSyntheticKey, sv.Value)

	resetAdminSystemOneFlags(t)
	systemOneAdminClientLoader = func() (hubclient.SystemOneService, error) {
		return env.client.SystemOne(), nil
	}
	listOut, _, err := executeAdminSystemOneSubcmd(t, []string{"list"}, nil)
	require.NoError(t, err)
	assert.Contains(t, listOut, "typesafe-local")
	assert.Contains(t, listOut, inst.Auth.SecretName)
	assert.NotContains(t, listOut, offlineSyntheticKey)

	resetAdminSystemOneFlags(t)
	systemOneAdminClientLoader = func() (hubclient.SystemOneService, error) {
		return env.client.SystemOne(), nil
	}
	delOut, _, err := executeAdminSystemOneSubcmd(t, []string{"delete", "typesafe-local"}, nil)
	require.NoError(t, err)
	assert.Contains(t, delOut, "Deleted System One instance typesafe-local")
}

func TestAdminSystemOne_GeminiDecisionsVerticalProbeAndTypeSafeRegression(t *testing.T) {
	// Synthetic from docs (not recorded from the live API): ai.google.dev/gemini-api/docs/decisions-api & api/decisions.
	resetAdminSystemOneFlags(t)
	env := startInProcessSystemOneHub(t)

	type recordedCall struct {
		path string
		goog string
		auth string
		body string
	}
	var (
		mu           sync.Mutex
		geminiStatus = http.StatusOK
		geminiBody   string
		calls        []recordedCall
	)

	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		if r.Body != nil {
			_, _ = buf.ReadFrom(r.Body)
			_ = r.Body.Close()
		}
		mu.Lock()
		calls = append(calls, recordedCall{
			path: r.URL.Path,
			goog: r.Header.Get("x-goog-api-key"),
			auth: r.Header.Get("Authorization"),
			body: buf.String(),
		})
		st := geminiStatus
		gBody := geminiBody
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1beta/decisions":
			w.WriteHeader(st)
			_, _ = w.Write([]byte(gBody))
		case "/v1/systemone":
			w.Header().Set("X-Typesafe-Request-Id", "req-cli-ts-1")
			_, _ = w.Write([]byte(`{
				"model": "jev-1.14.0",
				"answers": {
					"mentions_tests": {"type": "noul", "noul": 0.94},
					"build_outcome": {
						"type": "choice",
						"choice": "passed",
						"confidence": 0.85,
						"probabilities": {"passed": 0.90, "failed": 0.06, "unknown": 0.04}
					}
				},
				"usage": {"input_tokens": 29, "output_tokens": 5}
			}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(tlsSrv.Close)

	env.server.SetSystemOneRuntimeOptionsForTesting(
		t,
		systemoneruntime.WithEgressOptionsForTesting(
			egress.WithLoopbackServerForTesting(tlsSrv.URL, tlsSrv.Client()),
		),
	)

	const (
		geminiSecret   = "offline-gemini-cli-secret-never-disclose-8414"
		typeSafeSecret = "offline-typesafe-cli-secret-never-disclose-8414"
		safetyCanary   = "CANARY_CLI_400_SAFETY_MESSAGE_8414"
	)

	specRaw := buildSystemOneLiveProbeSpec(t)
	specFile := filepath.Join(t.TempDir(), "probe-spec.json")
	require.NoError(t, os.WriteFile(specFile, specRaw, 0o600))

	// Create gemini_decisions instance without --endpoint first to verify default endpoint via CLI.
	systemOneAdminClientLoader = func() (hubclient.SystemOneService, error) {
		return env.client.SystemOne(), nil
	}
	defOut, defErr, err := executeAdminSystemOneSubcmd(t, []string{
		"create", "gemini-default",
		"--adapter", geminidecisions.AdapterName,
		"--model", "gemini-decision-1",
		"--key-stdin",
	}, strings.NewReader(geminiSecret+"\n"))
	require.NoError(t, err)
	assert.Contains(t, defOut, "Endpoint: "+geminidecisions.DefaultEndpoint)
	assert.NotContains(t, defOut, geminiSecret)
	assert.NotContains(t, defErr, geminiSecret)

	// Create gemini_decisions instance targeting offline loopback TLS server for vertical probe.
	resetAdminSystemOneFlags(t)
	systemOneAdminClientLoader = func() (hubclient.SystemOneService, error) {
		return env.client.SystemOne(), nil
	}
	createOut, createErr, err := executeAdminSystemOneSubcmd(t, []string{
		"create", "gemini-local",
		"--adapter", geminidecisions.AdapterName,
		"--model", "gemini-decision-1",
		"--endpoint", tlsSrv.URL,
		"--key-stdin",
	}, strings.NewReader(geminiSecret+"\n"))
	require.NoError(t, err)
	assert.Contains(t, createOut, "Created System One instance gemini-local")
	assert.NotContains(t, createOut, geminiSecret)
	assert.NotContains(t, createErr, geminiSecret)

	inst, err := env.client.SystemOne().GetInstance(context.Background(), "gemini-local")
	require.NoError(t, err)
	sv, err := env.secretStore.Get(context.Background(), inst.Auth.SecretName, store.ScopeHub, inProcessSystemOneHubID)
	require.NoError(t, err)
	assert.Equal(t, store.SecretTypeInternal, sv.SecretType)
	assert.Equal(t, geminiSecret, sv.Value)

	// Probe --models on gemini_decisions fails with "adapter does not support model listing".
	resetAdminSystemOneFlags(t)
	restoreAllSilenceUsage(t)
	systemOneAdminClientLoader = func() (hubclient.SystemOneService, error) {
		return env.client.SystemOne(), nil
	}
	_, _, modelsErr := executeAdminSystemOneSubcmd(t, []string{"probe", "gemini-local", "--models"}, nil)
	require.Error(t, modelsErr)
	assert.Contains(t, modelsErr.Error(), "adapter does not support model listing")

	// Probe 1: missing usage -> renders model_unreported and "usage unavailable (usage_unreported)", never 0 tokens.
	mu.Lock()
	geminiStatus = http.StatusOK
	geminiBody = `{
		"answers": {
			"mentions_tests": {"type": "noul", "noul": 0.99, "confidence": 0.99},
			"build_outcome": {
				"type": "choice",
				"choice": "passed",
				"confidence": 0.99,
				"probabilities": {"failed": 0.004, "passed": 0.99, "unknown": 0.005}
			}
		}
	}`
	mu.Unlock()

	resetAdminSystemOneFlags(t)
	systemOneAdminClientLoader = func() (hubclient.SystemOneService, error) {
		return env.client.SystemOne(), nil
	}
	probeHuman, probeHumanErr, err := executeAdminSystemOneSubcmd(t, []string{
		"probe", "gemini-local",
		"--spec-file", specFile,
		"--text", "All 18 automated unit tests passed.",
	}, nil)
	require.NoError(t, err)
	assert.Contains(t, probeHuman, "Class: success")
	assert.Contains(t, probeHuman, "Configured Model:     gemini-decision-1")
	assert.Contains(t, probeHuman, "Resolved Model:       unreported (model_unreported)")
	assert.Contains(t, probeHuman, "Usage:                usage unavailable (usage_unreported)")
	assert.Contains(t, probeHuman, "Notes:                model_unreported, usage_unreported")
	assert.Contains(t, probeHuman, "gemini.confidence=0.9900")
	assert.NotContains(t, probeHuman, "input_tokens=0")
	assert.NotContains(t, probeHuman, geminiSecret)
	assert.NotContains(t, probeHumanErr, geminiSecret)

	// Probe 1b: same missing-usage response in --format json omits "usage".
	resetAdminSystemOneFlags(t)
	outputFormat = "json"
	systemOneAdminClientLoader = func() (hubclient.SystemOneService, error) {
		return env.client.SystemOne(), nil
	}
	probeJSON, _, err := executeAdminSystemOneSubcmd(t, []string{
		"probe", "gemini-local",
		"--spec-file", specFile,
		"--text", "All 18 automated unit tests passed.",
	}, nil)
	require.NoError(t, err)
	assert.NotContains(t, probeJSON, `"usage"`)
	assert.Contains(t, probeJSON, `"model_unreported"`)
	assert.Contains(t, probeJSON, `"usage_unreported"`)
	assert.NotContains(t, probeJSON, geminiSecret)

	// Probe 1c: Gemini response with measured usage renders input_tokens=58, output_tokens=0 alongside model_unreported.
	mu.Lock()
	geminiStatus = http.StatusOK
	geminiBody = `{
		"answers": {
			"mentions_tests": {"type": "noul", "noul": 0.99, "confidence": 0.99},
			"build_outcome": {
				"type": "choice",
				"choice": "passed",
				"confidence": 0.99,
				"probabilities": {"failed": 0.004, "passed": 0.99, "unknown": 0.005}
			}
		},
		"usage": {"total_input_tokens": 58, "total_output_tokens": 0}
	}`
	mu.Unlock()

	resetAdminSystemOneFlags(t)
	systemOneAdminClientLoader = func() (hubclient.SystemOneService, error) {
		return env.client.SystemOne(), nil
	}
	measuredHuman, _, err := executeAdminSystemOneSubcmd(t, []string{
		"probe", "gemini-local",
		"--spec-file", specFile,
		"--text", "All 18 automated unit tests passed.",
	}, nil)
	require.NoError(t, err)
	assert.Contains(t, measuredHuman, "Resolved Model:       unreported (model_unreported)")
	assert.Contains(t, measuredHuman, "Usage:                input_tokens=58, output_tokens=0")
	assert.NotContains(t, measuredHuman, geminiSecret)

	// Probe 2: HTTP 400 safety block -> exit code 1, reason content_blocked, error body never leaked.
	mu.Lock()
	geminiStatus = http.StatusBadRequest
	geminiBody = fmt.Sprintf(`{"error":{"code":"safety","message":%q}}`, safetyCanary)
	mu.Unlock()

	resetAdminSystemOneFlags(t)
	restoreAllSilenceUsage(t)
	systemOneAdminClientLoader = func() (hubclient.SystemOneService, error) {
		return env.client.SystemOne(), nil
	}
	blockOut, blockStderr, blockErr := executeAdminSystemOneSubcmd(t, []string{
		"probe", "gemini-local",
		"--spec-file", specFile,
		"--text", "Synthetic safety test.",
	}, nil)
	require.Error(t, blockErr)
	assert.Equal(t, 1, exitCodeFor(blockErr))
	assert.Contains(t, blockOut, "Class: failed")
	assert.Contains(t, blockOut, "Reason: content_blocked")
	assert.Contains(t, blockOut, "Resolved Model:       unreported (model_unreported)")
	assert.Contains(t, blockOut, "Usage:                usage unavailable")
	assert.NotContains(t, blockOut, safetyCanary)
	assert.NotContains(t, blockStderr, safetyCanary)
	assert.NotContains(t, blockErr.Error(), safetyCanary)
	assert.NotContains(t, blockOut, geminiSecret)

	// TypeSafe regression probe on the same Hub: still sends Authorization: Bearer and never x-goog-api-key.
	resetAdminSystemOneFlags(t)
	systemOneAdminClientLoader = func() (hubclient.SystemOneService, error) {
		return env.client.SystemOne(), nil
	}
	_, _, err = executeAdminSystemOneSubcmd(t, []string{
		"create", "typesafe-reg-cli",
		"--adapter", "typesafe",
		"--model", "jev-1.14.0",
		"--endpoint", tlsSrv.URL,
		"--key-stdin",
	}, strings.NewReader(typeSafeSecret+"\n"))
	require.NoError(t, err)

	resetAdminSystemOneFlags(t)
	systemOneAdminClientLoader = func() (hubclient.SystemOneService, error) {
		return env.client.SystemOne(), nil
	}
	tsOut, _, err := executeAdminSystemOneSubcmd(t, []string{
		"probe", "typesafe-reg-cli",
		"--spec-file", specFile,
		"--text", "All 18 automated unit tests passed.",
	}, nil)
	require.NoError(t, err)
	assert.Contains(t, tsOut, "Class: success")
	assert.Contains(t, tsOut, "Resolved Model:       jev-1.14.0")
	assert.Contains(t, tsOut, "Usage:                input_tokens=29, output_tokens=5")
	assert.NotContains(t, tsOut, typeSafeSecret)

	mu.Lock()
	require.Len(t, calls, 5)
	for _, gc := range calls[:4] {
		assert.Equal(t, "/v1beta/decisions", gc.path)
		assert.Equal(t, geminiSecret, gc.goog)
		assert.Empty(t, gc.auth)
		assert.NotContains(t, gc.body, geminiSecret)
	}
	assert.Equal(t, "/v1/systemone", calls[4].path)
	assert.Equal(t, "Bearer "+typeSafeSecret, calls[4].auth)
	assert.Empty(t, calls[4].goog)
	assert.NotContains(t, calls[4].body, typeSafeSecret)
	mu.Unlock()
}
