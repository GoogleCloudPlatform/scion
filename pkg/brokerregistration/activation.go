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

package brokerregistration

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// NotRegisteredError is the flat_runtime_broker_not_registered refusal: a
// remote flat instance has no instance-scoped credentials. It is never
// answered by a fallback to legacy credentials.
type NotRegisteredError struct {
	InstanceKey     string
	RuntimeBrokerID string
}

// Code returns the frozen error code.
func (e *NotRegisteredError) Code() string { return api.ErrCodeFlatRuntimeBrokerNotRegistered }

func (e *NotRegisteredError) Error() string {
	return fmt.Sprintf("%s: flat Runtime Broker instance %q (Runtime Broker %s) has no instance-scoped Hub credentials; "+
		"register it with 'scion broker register --instance %s'. Legacy Runtime Broker credentials are never used for a flat instance",
		api.ErrCodeFlatRuntimeBrokerNotRegistered, e.InstanceKey, e.RuntimeBrokerID, e.InstanceKey)
}

// LoadInstanceCredentials loads only the instance-scoped credentials of id,
// <globalDir>/runtime-brokers/<key>/hub-credentials/*.json, sorted by hub
// name. With none present it returns a *NotRegisteredError; the legacy
// <globalDir>/hub-credentials directory and broker-credentials.json are never
// read.
func LoadInstanceCredentials(globalDir string, id *brokeridentity.Identity) ([]brokercredentials.BrokerCredentials, error) {
	if id == nil {
		return nil, errors.New("brokerregistration: an instance identity is required")
	}
	list, err := brokercredentials.NewMultiStore(InstanceCredentialsDir(globalDir, id.InstanceKey)).List()
	if err != nil {
		return nil, fmt.Errorf("loading instance credentials for %q: %w", id.InstanceKey, err)
	}
	if len(list) == 0 {
		return nil, &NotRegisteredError{InstanceKey: id.InstanceKey, RuntimeBrokerID: id.RuntimeBrokerID}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list, nil
}

// ValidateActivation is the required remote activation validation (R10). It
// runs on every activation of a remote flat instance, before the control
// channel, heartbeat or any dispatch is accepted, and returns nil only for a
// bound result:
//  1. creds must be the instance's own credentials (nil: not registered);
//  2. it authenticates with them (an authentication failure is returned as
//     is: not activated);
//  3. it reads the Hub's current binding for the instance's Runtime Broker ID;
//  4. it runs CheckActivationAck(id, "activate", got.ID, got.RuntimeTarget).
//
// Carrier: (a), an HMAC-authenticated self-read of the instance's own row,
// GET /api/v1/runtime-brokers/{id}. The Hub already authorizes a Runtime
// Broker reading its own row and returns the stored runtimeTarget, so this
// needs no Hub or wire change, and it runs before (and independently of) the
// control-channel handshake.
//
// client may be nil, in which case an HMAC client is built from creds. A
// non-nil client must authenticate with creds.
func ValidateActivation(ctx context.Context, client hubclient.Client, id *brokeridentity.Identity, creds *brokercredentials.BrokerCredentials) error {
	if id == nil {
		return errors.New("brokerregistration: an instance identity is required")
	}
	if creds == nil || creds.SecretKey == "" {
		return &NotRegisteredError{InstanceKey: id.InstanceKey, RuntimeBrokerID: id.RuntimeBrokerID}
	}
	// Credentials minted for another Runtime Broker ID can never acknowledge
	// this identity's binding.
	if creds.BrokerID != id.RuntimeBrokerID {
		return brokeridentity.CheckActivationAck(id, brokeridentity.PhaseActivate, creds.BrokerID, nil)
	}
	if client == nil {
		secret, err := base64.StdEncoding.DecodeString(creds.SecretKey)
		if err != nil {
			return fmt.Errorf("flat Runtime Broker %s: instance credentials %q have an invalid secret: %w", id.RuntimeBrokerID, creds.Name, err)
		}
		client, err = hubclient.New(creds.HubEndpoint, hubclient.WithHMACAuth(creds.BrokerID, secret))
		if err != nil {
			return fmt.Errorf("flat Runtime Broker %s: creating the Hub client: %w", id.RuntimeBrokerID, err)
		}
	}
	got, err := client.RuntimeBrokers().Get(ctx, id.RuntimeBrokerID)
	if err != nil {
		return fmt.Errorf("flat Runtime Broker %s not activated: reading its Hub binding failed: %w", id.RuntimeBrokerID, err)
	}
	if got == nil {
		return brokeridentity.CheckActivationAck(id, brokeridentity.PhaseActivate, "", nil)
	}
	return brokeridentity.CheckActivationAck(id, brokeridentity.PhaseActivate, got.ID, got.RuntimeTarget)
}
