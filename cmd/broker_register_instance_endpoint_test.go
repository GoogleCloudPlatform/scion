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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/brokerregistration"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// TestBrokerRegisterInstance_PassesTheEndpointItRegistersThrough: 'scion
// broker register --instance' hands registration the Hub endpoint it
// resolved (here SCION_HUB_ENDPOINT), so the instance credentials keep the
// endpoint the operator reached the Hub through (ptone/scion#3272).
func TestBrokerRegisterInstance_PassesTheEndpointItRegistersThrough(t *testing.T) {
	const endpoint = "http://hub.example:8080"
	setupRegisterInstanceTest(t, endpoint)
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
	assert.Equal(t, endpoint, got.HubEndpoint)
}

// TestBrokerRegisterInstance_LabelsTheRowAsARemoteBroker: 'scion broker
// register --instance' labels the registration with the established
// broker-role key and the value "remote", as legacy remote registration
// does, so Hub filters on that label treat it the same way.
func TestBrokerRegisterInstance_LabelsTheRowAsARemoteBroker(t *testing.T) {
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
	assert.Equal(t, map[string]string{"scion.io/broker-role": "remote"}, got.Labels)
}
