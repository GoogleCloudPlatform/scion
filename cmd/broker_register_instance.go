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

package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/brokerownership"
	"github.com/GoogleCloudPlatform/scion/pkg/brokerregistration"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"github.com/GoogleCloudPlatform/scion/pkg/version"
)

// brokerRegisterInstance is the --instance flag of 'scion broker register':
// the key of a configured flat Runtime Broker instance to register.
var brokerRegisterInstance string

// runBrokerRegisterInstance registers one configured flat Runtime Broker
// instance with the Hub (contract R10, P2.1 remote registration):
//  1. strictly load server.broker.instances and select the instance by key;
//  2. load or create its identity and verify its execution scope;
//  3. brokerregistration.RegisterInstance: register and join with the
//     runtime target, check both acknowledgements, and only then save the
//     instance-scoped credentials.
//
// It uses the operator's user credential (the existing user-credential
// registration endpoint and broker.create). It does not need the instance
// to be running: the host validates the saved binding at its next start.
func runBrokerRegisterInstance(cmd *cobra.Command, key string) error {
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return fmt.Errorf("resolving the global Scion directory: %w", err)
	}
	settings, err := config.LoadSettings(globalDir)
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}
	endpoint := GetHubEndpoint(settings)
	if endpoint == "" {
		return fmt.Errorf("hub endpoint not configured; configure via SCION_HUB_ENDPOINT, hub.endpoint in settings.yaml, or --hub flag")
	}

	// Step 1: the strictly loaded instance with this key.
	instances, err := config.LoadRuntimeBrokerInstances("")
	if err != nil {
		return fmt.Errorf("server.broker.instances: %w", err)
	}
	var inst *config.V1RuntimeBrokerInstanceConfig
	for i := range instances {
		if instances[i].Key == key {
			inst = &instances[i]
			break
		}
	}
	if inst == nil {
		return fmt.Errorf("no Runtime Broker instance with key %q is configured under server.broker.instances", key)
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
	defer cancel()

	// Step 2: identity with a verified execution scope.
	rt, err := newFlatInstanceRuntime(ctx, *inst)
	if err != nil {
		return fmt.Errorf("flat Runtime Broker instance %q: %w", key, err)
	}
	scope, err := flatScopeProber(ctx, *inst, rt)
	if err != nil {
		return fmt.Errorf("flat Runtime Broker instance %q: %w", key, err)
	}
	var vsBroker *config.V1BrokerConfig
	if vs, _, vsErr := config.LoadEffectiveSettings(""); vsErr == nil && vs != nil && vs.Server != nil {
		vsBroker = vs.Server.Broker
	}
	id, err := brokeridentity.LoadOrCreate(brokeridentity.InstanceDir(globalDir, key), key, inst.RuntimeTarget.Type, scope,
		legacyRuntimeBrokerIDs(nil, settings, vsBroker, globalDir))
	if err != nil {
		return fmt.Errorf("flat Runtime Broker instance %q: %w", key, err)
	}

	client, err := getHubClient(settings)
	if err != nil {
		return err
	}
	hubName := brokerHubName
	if hubName == "" {
		hubName = brokercredentials.DeriveHubName(endpoint)
	}
	if hubName == "" {
		hubName = "default"
	}
	transportMode := brokerTransportMode
	if transportMode == "" {
		transportMode = os.Getenv(transportauth.EnvTransportMode)
	}
	transportAudience := brokerTransportAudience
	if transportAudience == "" {
		transportAudience = os.Getenv(transportauth.EnvTransportAudience)
	}
	hostname, _ := os.Hostname()

	creds, err := registerFlatInstance(ctx, client, *inst, id, hubName, brokerregistration.InstanceCredentialsDir(globalDir, key), brokerregistration.Options{
		Hostname:          hostname,
		Version:           version.Version,
		Capabilities:      runtimebroker.StaticCapabilityNames(rt),
		AutoProvide:       brokerAutoProvide,
		Labels:            map[string]string{brokerownership.LabelBrokerRole: "remote"},
		WorkspaceStorage:  loadBrokerRegistrationWorkspaceStorage(),
		TransportMode:     transportMode,
		TransportAudience: transportAudience,
		HubEndpoint:       endpoint,
	})
	if err != nil {
		return err
	}
	fmt.Printf("Registered flat Runtime Broker instance %q as Runtime Broker %s (%s), runtime target %s\n",
		key, creds.BrokerID, inst.Name, id.RuntimeTarget.ID)
	fmt.Printf("Instance credentials saved to %s\n", brokerregistration.InstanceCredentialsDir(globalDir, key))
	return nil
}

// registerFlatInstance is brokerregistration.RegisterInstance; a variable so
// command tests can observe the call.
var registerFlatInstance = func(ctx context.Context, client hubclient.Client, inst config.V1RuntimeBrokerInstanceConfig, id *brokeridentity.Identity, hubName, credDir string, o brokerregistration.Options) (*brokercredentials.BrokerCredentials, error) {
	return brokerregistration.RegisterInstance(ctx, client, inst, id, hubName, credDir, brokerregistration.WithOptions(o))
}
