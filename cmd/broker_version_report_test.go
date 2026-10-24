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
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/brokerregistration"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/version"
)

// A Runtime Broker reports the same build token the hub compares with,
// version.Short() (the tag, or the short commit), not the raw ldflags
// version, which is empty on every untagged build (ptone/scion#5972). In a
// test binary version.Version is empty and version.Short() is not, so these
// assertions tell the two apart.

// TestCompleteJoinAndPersist_SendsBuildToken: the join request carries
// version.Short().
func TestCompleteJoinAndPersist_SendsBuildToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const brokerID = "11111111-2222-3333-4444-555555555555"
	var sent hubclient.JoinBrokerRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/brokers/join" {
			_ = json.NewDecoder(r.Body).Decode(&sent)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"brokerId":  brokerID,
			"secretKey": base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
		})
	}))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	res, err := completeJoinAndPersist(context.Background(), client, brokerJoinParams{
		Endpoint: "https://hub.example.com", HubName: "hub-example-com", BrokerID: brokerID,
		JoinToken: "scion_join_x", Hostname: "h", CredStore: brokercredentials.NewMultiStore(t.TempDir()),
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	require.NotEmpty(t, version.Short())
	assert.Equal(t, version.Short(), sent.Version, "the join sends the build token")
	assert.NotEmpty(t, sent.Version, "never the raw, possibly empty, ldflags version")
}

// TestBrokerRegisterInstance_SendsBuildToken: 'scion broker register
// --instance' hands registration version.Short().
func TestBrokerRegisterInstance_SendsBuildToken(t *testing.T) {
	setupRegisterInstanceTest(t, "http://hub.example:8080")
	brokerRegisterInstance = "remote-docker"
	orig := registerFlatInstance
	t.Cleanup(func() { registerFlatInstance = orig })
	var got *brokerregistration.Options
	registerFlatInstance = func(_ context.Context, _ hubclient.Client, _ config.V1RuntimeBrokerInstanceConfig, id *brokeridentity.Identity,
		_, _ string, o brokerregistration.Options) (*brokercredentials.BrokerCredentials, error) {
		got = &o
		return &brokercredentials.BrokerCredentials{BrokerID: id.RuntimeBrokerID, HubEndpoint: o.HubEndpoint}, nil
	}

	require.NoError(t, runBrokerRegister(brokerRegisterCmd, nil))
	require.NotNil(t, got, "registration was not called")
	assert.Equal(t, version.Short(), got.Version)
	assert.NotEmpty(t, got.Version)
}
