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

package hubclient

import (
	"context"
	"net/url"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// RuntimeBrokerSettings is the read view of a Runtime Broker's Hub-stored
// settings (GET /api/v1/runtime-brokers/{id}/settings).
type RuntimeBrokerSettings struct {
	BrokerID  string                         `json:"brokerId"`
	Settings  RuntimeBrokerSettingsValues    `json:"settings"`
	Effective RuntimeBrokerSettingsEffective `json:"effective"`
	Revision  int64                          `json:"revision"`
}

// RuntimeBrokerSettingsValues are the broker's own stored settings.
type RuntimeBrokerSettingsValues struct {
	// MaxAgents is the broker's own agent limit: nil inherit, 0 unlimited,
	// >0 cap.
	MaxAgents *int64 `json:"maxAgents,omitempty"`
}

// RuntimeBrokerSettingsEffective is the Hub's resolution of each setting.
type RuntimeBrokerSettingsEffective struct {
	MaxAgents RuntimeBrokerEffectiveSetting `json:"maxAgents"`
}

// RuntimeBrokerEffectiveSetting is one resolved setting. A nil Value means
// the Hub could not resolve it; 0 means unlimited.
type RuntimeBrokerEffectiveSetting struct {
	Value  *int64 `json:"value"`
	Source string `json:"source"`
	// Count is the number of active reservations counted against the key.
	Count *int64 `json:"count,omitempty"`
	// Inherited is the value the broker would have with no setting of its
	// own (entitlement bindings, then the Hub default).
	Inherited RuntimeBrokerInheritedSetting `json:"inherited"`
}

// RuntimeBrokerInheritedSetting is the inherited resolution of a setting.
type RuntimeBrokerInheritedSetting struct {
	Value  *int64 `json:"value"`
	Source string `json:"source"`
}

// RuntimeBrokerSettingsGetter is implemented by the RuntimeBrokerService
// values this package returns. It is a separate interface, not part of
// RuntimeBrokerService, so existing implementations outside this package
// keep compiling.
type RuntimeBrokerSettingsGetter interface {
	GetSettings(ctx context.Context, brokerID string) (*RuntimeBrokerSettings, error)
}

// GetSettings reads a Runtime Broker's Hub-stored settings. It sends one
// GET and nothing else.
func (s *runtimeBrokerService) GetSettings(ctx context.Context, brokerID string) (*RuntimeBrokerSettings, error) {
	resp, err := s.c.get(ctx, "/api/v1/runtime-brokers/"+url.PathEscape(brokerID)+"/settings", nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[RuntimeBrokerSettings](resp)
}
