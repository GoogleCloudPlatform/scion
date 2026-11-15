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

//go:build systemone_live && !no_sqlite

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/metrics"
	systemoneruntime "github.com/GoogleCloudPlatform/scion/pkg/systemone/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSystemOneLive_TypeSafeAdminProbe implements AC-12 / §8 L4 Option A:
// starts the Hub in process with hub.system_one enabled, reads the bootstrap
// API key inside the test process only, registers a TypeSafe instance
// (jev-1.14.0) over loopback so the key is stored in the Hub's internal secret
// backend, runs the models check, and runs a single synthetic probe with one
// binary question and one three-option choice question.
//
// Manual gate only — never executed in CI, and skipped whenever the bootstrap
// environment variable is unset.
func TestSystemOneLive_TypeSafeAdminProbe(t *testing.T) {
	// Validate the AC-12 spec shape even when skipping so compile/skip checks
	// verify the spec stays in sync with pkg/systemone/spec.
	specRaw := buildSystemOneLiveProbeSpec(t)

	apiKey := strings.TrimSpace(os.Getenv("JEVKEY"))
	if apiKey == "" {
		t.Skip("bootstrap live-probe key is unset; skipping manual live TypeSafe probe")
	}

	env := startInProcessSystemOneHub(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	const (
		instanceName    = "typesafe-live-gate"
		configuredModel = "jev-1.14.0"
		syntheticSample = "Synthetic sample: the CI build finished in 42 seconds with all 18 unit tests passing and zero failures."
	)

	created, err := env.client.SystemOne().CreateInstance(ctx, &hubclient.CreateSystemOneInstanceRequest{
		Name:    instanceName,
		Adapter: "typesafe",
		Model:   configuredModel,
		Auth: &hubclient.CreateSystemOneInstanceAuthRequest{
			Kind:   "api_key",
			APIKey: apiKey,
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = env.client.SystemOne().DeleteInstance(context.Background(), instanceName)
	})
	require.NotEmpty(t, created.Auth.SecretName)

	// Confirm the key landed in the Hub's internal secret store.
	sv, err := env.secretStore.Get(ctx, created.Auth.SecretName, store.ScopeHub, inProcessSystemOneHubID)
	require.NoError(t, err)
	assert.Equal(t, store.SecretTypeInternal, sv.SecretType)

	// Step 1: models check (--models / {"models": true}).
	modelsResp, err := env.client.SystemOne().ProbeInstance(ctx, instanceName, &hubclient.ProbeSystemOneInstanceRequest{
		Models: true,
	})
	require.NoError(t, err)
	require.Equal(t, systemoneruntime.ClassSuccess, modelsResp.Class, "models probe failed with reason=%q", modelsResp.Reason)
	require.NotEmpty(t, modelsResp.Models)
	t.Logf("TypeSafe models (%d): %s", len(modelsResp.Models), strings.Join(modelsResp.Models, ", "))

	// Step 2: single probe with 1 binary + 1 three-option choice question over synthetic text.
	probeResp, err := env.client.SystemOne().ProbeInstance(ctx, instanceName, &hubclient.ProbeSystemOneInstanceRequest{
		Kind:   string(spec.KindJudgments),
		RuleID: "ac12-live-gate",
		Spec:   specRaw,
		Probe:  &hubclient.ProbeSystemOneInput{Text: syntheticSample},
	})
	require.NoError(t, err)

	var humanOut bytes.Buffer
	renderAdminSystemOneProbeHuman(&humanOut, probeResp)
	require.NotContains(t, humanOut.String(), apiKey)
	t.Logf("Live probe summary:\n%s", humanOut.String())

	rawRespJSON, err := json.Marshal(probeResp)
	require.NoError(t, err)
	require.NotContains(t, string(rawRespJSON), apiKey)

	require.Equal(t, systemoneruntime.ClassSuccess, probeResp.Class, "spec probe returned class=%s reason=%q", probeResp.Class, probeResp.Reason)
	eval := probeResp.Evaluation
	require.NotNil(t, eval)

	assert.Equal(t, configuredModel, eval.ConfiguredModel)
	assert.NotEmpty(t, eval.ResolvedModel)
	t.Logf("Model resolution: configured=%q resolved=%q model_alias_reported=%t notes=%v",
		eval.ConfiguredModel, eval.ResolvedModel, eval.HasModelAliasReported(), eval.Notes)

	binAns, ok := eval.Answers["mentions_tests"]
	require.True(t, ok, "missing binary question answer")
	require.NotNil(t, binAns.Bool)
	assert.NotEmpty(t, binAns.Distribution)
	assert.Contains(t, binAns.Metrics, metrics.NamePTop)
	assert.Contains(t, binAns.Metrics, metrics.NameMargin)
	assert.Contains(t, binAns.Metrics, metrics.NameNormEntropy)

	choiceAns, ok := eval.Answers["build_outcome"]
	require.True(t, ok, "missing choice question answer")
	require.NotNil(t, choiceAns.Choice)
	assert.Len(t, choiceAns.Distribution, 3)
	assert.Contains(t, choiceAns.NativeMetrics, "typesafe.confidence")
	assert.Contains(t, choiceAns.Metrics, metrics.NamePTop)
	assert.Contains(t, choiceAns.Metrics, metrics.NameMargin)
	assert.Contains(t, choiceAns.Metrics, metrics.NameNormEntropy)

	require.Len(t, eval.Attempts, 1)
	assert.Equal(t, instanceName, eval.Attempts[0].Instance)
	assert.Equal(t, "typesafe", eval.Attempts[0].Adapter)
	assert.Equal(t, systemoneruntime.AttemptSuccess, eval.Attempts[0].Outcome)
}
