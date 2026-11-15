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
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
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

	//Verify the AC-12 spec compiles cleanly offline too.
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
