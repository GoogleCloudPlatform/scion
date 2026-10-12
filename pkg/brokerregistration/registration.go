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

// Package brokerregistration registers a flat Runtime Broker instance with a
// remote Hub and validates its activation on every start
// (.design/flat-runtime-brokers-contract.md section 6, R9 and R10 "P2.1
// remote registration" and "P2.1 remote activation validation").
//
// Registration is user-authorized: the hubclient passed to RegisterInstance
// carries the operator's user credential, and the Hub admits it through the
// existing user-credential registration endpoint and broker.create. A Runtime
// Broker HMAC identity is not admitted for registration.
package brokerregistration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// HubCredentialsDirName is the instance-scoped credentials directory inside
// an instance directory: <global>/runtime-brokers/<key>/hub-credentials.
const HubCredentialsDirName = "hub-credentials"

// InstanceCredentialsDir returns the instance-scoped Hub credentials
// directory for instance key under globalDir. It never names the legacy
// <global>/hub-credentials directory or broker-credentials.json.
func InstanceCredentialsDir(globalDir, key string) string {
	return filepath.Join(brokeridentity.InstanceDir(globalDir, key), HubCredentialsDirName)
}

// Options are optional registration inputs. The zero value registers with no
// capabilities, labels or workspace storage descriptor.
type Options struct {
	// Hostname and Version are sent on the join.
	Hostname string
	Version  string
	// Capabilities are the instance's static capabilities, sent on the
	// registration and on the join so the Hub knows them before the first
	// heartbeat.
	Capabilities []string
	// AutoProvide is stored as sent (contract R8).
	AutoProvide bool
	// Labels are sent on the registration.
	Labels map[string]string
	// WorkspaceStorage is the instance's workspace storage descriptor.
	WorkspaceStorage *api.BrokerWorkspaceStorage
	// TransportMode and TransportAudience are saved with the credentials.
	TransportMode     string
	TransportAudience string
	// HubEndpoint is the Hub endpoint the client reached the Hub on. It is
	// saved with the credentials, so the instance keeps using it, as the
	// legacy join does. The Hub's own endpoint in the join response is used
	// only when it is empty: a Hub without a configured public endpoint
	// reports one that a remote broker may not be able to reach (for
	// example http://localhost:<port>).
	HubEndpoint string
}

// Option sets an Options field.
type Option func(*Options)

// WithOptions replaces all options at once.
func WithOptions(o Options) Option { return func(dst *Options) { *dst = o } }

// RegistrationRefusal is a refused remote registration. It wraps the
// activation acknowledgement error (errors.As finds the
// *brokeridentity.AckError with its code) and adds what an operator needs
// for reconciliation. Nothing was saved and the instance is not activated.
type RegistrationRefusal struct {
	// Phase is brokeridentity.PhaseRegister or brokeridentity.PhaseJoin.
	Phase string
	// RuntimeBrokerID and Name identify the instance being registered.
	RuntimeBrokerID string
	Name            string
	// SecretRotated is true for a join-phase refusal: the Hub already
	// replaced the row's secret with one this instance discarded.
	SecretRotated bool
	// Err is the acknowledgement error.
	Err error
}

func (e *RegistrationRefusal) Error() string {
	leftovers := fmt.Sprintf("A Hub that does not support flat Runtime Brokers may already have created a legacy row with Runtime Broker ID %s, "+
		"or re-registered an existing row matched by the name %q. Nothing was deleted or rewritten automatically; "+
		"an administrator reconciles that row. No credentials were saved.", e.RuntimeBrokerID, e.Name)
	if e.SecretRotated {
		leftovers = fmt.Sprintf("The join rotated the secret of Runtime Broker %s (%q) and the returned secret was discarded; "+
			"the row may show online until the stale Runtime Broker sweep marks it offline. "+
			"Nothing was deleted or rewritten automatically; rerun the registration once every Hub replica supports flat Runtime Brokers. "+
			"No credentials were saved.", e.RuntimeBrokerID, e.Name)
	}
	return fmt.Sprintf("flat Runtime Broker registration refused: %v. %s", e.Err, leftovers)
}

func (e *RegistrationRefusal) Unwrap() error { return e.Err }

// RegisterInstance registers a flat Runtime Broker instance with a remote
// Hub and saves its instance-scoped credentials. The caller has already
// strictly loaded inst (config.LoadRuntimeBrokerInstances) and loaded or
// created its identity with a verified execution scope (steps 1 and 2).
//
// Order (frozen, R10):
//  3. POST /api/v1/brokers with brokerId, name and runtimeTarget, then
//     CheckActivationAck(id, "register", ...). A refusal sends no join.
//  4. POST /api/v1/brokers/join with runtimeTarget, then
//     CheckActivationAck(id, "join", ...). A refusal discards the secret.
//  5. Only then write <credDir>/<hubName>.json (brokercredentials format,
//     0600, atomic).
//
// Existing credentials never short-circuit it: every call re-registers and
// re-joins, rotating the secret. client must carry a user credential.
func RegisterInstance(ctx context.Context, client hubclient.Client, inst config.V1RuntimeBrokerInstanceConfig, id *brokeridentity.Identity, hubName, credDir string, opts ...Option) (*brokercredentials.BrokerCredentials, error) {
	var o Options
	for _, fn := range opts {
		fn(&o)
	}
	if client == nil {
		return nil, errors.New("brokerregistration: a Hub client is required")
	}
	if id == nil {
		return nil, errors.New("brokerregistration: an instance identity is required")
	}
	if inst.Key != id.InstanceKey {
		return nil, fmt.Errorf("brokerregistration: instance key %q does not match identity key %q", inst.Key, id.InstanceKey)
	}
	if err := brokercredentials.ValidateName(hubName); err != nil {
		return nil, fmt.Errorf("brokerregistration: hub name: %w", err)
	}
	if credDir == "" {
		return nil, errors.New("brokerregistration: a credentials directory is required")
	}

	descriptor := descriptorFor(inst, id)

	// Step 3: registration, then its acknowledgement.
	createResp, err := client.RuntimeBrokers().Create(ctx, &hubclient.CreateBrokerRequest{
		BrokerID:      id.RuntimeBrokerID,
		Name:          inst.Name,
		Capabilities:  o.Capabilities,
		Labels:        o.Labels,
		AutoProvide:   o.AutoProvide,
		RuntimeTarget: descriptor,
	})
	if err != nil {
		return nil, fmt.Errorf("flat Runtime Broker registration failed: %w", err)
	}
	if ackErr := brokeridentity.CheckActivationAck(id, brokeridentity.PhaseRegister, createResp.BrokerID, createResp.RuntimeTarget); ackErr != nil {
		return nil, &RegistrationRefusal{Phase: brokeridentity.PhaseRegister, RuntimeBrokerID: id.RuntimeBrokerID, Name: inst.Name, Err: ackErr}
	}

	// Step 4: join, then its acknowledgement.
	joinResp, err := client.RuntimeBrokers().Join(ctx, &hubclient.JoinBrokerRequest{
		BrokerID:         id.RuntimeBrokerID,
		JoinToken:        createResp.JoinToken,
		Hostname:         o.Hostname,
		Version:          o.Version,
		Capabilities:     o.Capabilities,
		WorkspaceStorage: o.WorkspaceStorage,
		RuntimeTarget:    descriptor,
	})
	if err != nil {
		return nil, fmt.Errorf("flat Runtime Broker join failed: %w", err)
	}
	if ackErr := brokeridentity.CheckActivationAck(id, brokeridentity.PhaseJoin, joinResp.BrokerID, joinResp.RuntimeTarget); ackErr != nil {
		return nil, &RegistrationRefusal{Phase: brokeridentity.PhaseJoin, RuntimeBrokerID: id.RuntimeBrokerID, Name: inst.Name, SecretRotated: true, Err: ackErr}
	}
	if joinResp.SecretKey == "" {
		return nil, fmt.Errorf("flat Runtime Broker join for %s returned no secret; no credentials were saved", id.RuntimeBrokerID)
	}

	// Step 5: both acknowledgements passed; save.
	creds := &brokercredentials.BrokerCredentials{
		Name:              hubName,
		BrokerID:          id.RuntimeBrokerID,
		SecretKey:         joinResp.SecretKey,
		HubEndpoint:       savedHubEndpoint(o.HubEndpoint, joinResp.HubEndpoint),
		AuthMode:          brokercredentials.AuthModeHMAC,
		RegisteredAt:      time.Now().UTC(),
		TransportMode:     o.TransportMode,
		TransportAudience: o.TransportAudience,
	}
	if err := saveCredentialsAtomic(credDir, creds); err != nil {
		return nil, fmt.Errorf("flat Runtime Broker %s registered, but saving its credentials failed: %w", id.RuntimeBrokerID, err)
	}
	return creds, nil
}

// savedHubEndpoint is the Hub endpoint saved with the credentials: the one
// the client used, else the Hub's self-reported one.
func savedHubEndpoint(used, reported string) string {
	if used != "" {
		return used
	}
	return reported
}

// descriptorFor is the registration descriptor: the identity's target ID and
// type plus the configured display name (defaulting to the type).
func descriptorFor(inst config.V1RuntimeBrokerInstanceConfig, id *brokeridentity.Identity) *api.RuntimeTargetDescriptor {
	d := &api.RuntimeTargetDescriptor{ID: id.RuntimeTarget.ID, Type: id.RuntimeTarget.Type}
	if inst.RuntimeTarget != nil {
		d.DisplayName = inst.RuntimeTarget.DisplayName
	}
	if d.DisplayName == "" {
		d.DisplayName = d.Type
	}
	return d
}

// saveCredentialsAtomic writes creds to <dir>/<creds.Name>.json in the
// brokercredentials format: a temp file in the same directory, fsynced, then
// renamed over the target, then the directory fsynced.
func saveCredentialsAtomic(dir string, creds *brokercredentials.BrokerCredentials) error {
	if err := os.MkdirAll(dir, brokercredentials.DirMode); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+creds.Name+".json.tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(brokercredentials.FileMode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(dir, creds.Name+".json")); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
