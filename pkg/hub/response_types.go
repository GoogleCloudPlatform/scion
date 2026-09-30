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

package hub

import (
	"encoding/json"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// AgentWithCapabilities wraps a store.Agent with capability annotations.
type AgentWithCapabilities struct {
	store.Agent
	Cap                 *Capabilities                    `json:"_capabilities,omitempty"`
	Messageability      interface{}                      `json:"_messageability,omitempty"`
	ResolvedHarness     string                           `json:"resolvedHarness,omitempty"`
	HarnessCapabilities *api.HarnessAdvancedCapabilities `json:"harnessCapabilities,omitempty"`
	CloudLogging        bool                             `json:"cloudLogging,omitempty"`
}

// MarshalJSON implements custom marshaling to avoid shadowing of fields by the embedded store.Agent.
func (a AgentWithCapabilities) MarshalJSON() ([]byte, error) {
	type AgentAlias store.Agent
	return json.Marshal(&struct {
		AgentAlias
		Cap                 *Capabilities                    `json:"_capabilities,omitempty"`
		Messageability      interface{}                      `json:"_messageability,omitempty"`
		ResolvedHarness     string                           `json:"resolvedHarness,omitempty"`
		HarnessCapabilities *api.HarnessAdvancedCapabilities `json:"harnessCapabilities,omitempty"`
		CloudLogging        bool                             `json:"cloudLogging,omitempty"`
	}{
		AgentAlias:          AgentAlias(a.Agent),
		Cap:                 a.Cap,
		Messageability:      a.Messageability,
		ResolvedHarness:     a.ResolvedHarness,
		HarnessCapabilities: a.HarnessCapabilities,
		CloudLogging:        a.CloudLogging,
	})
}

// UnmarshalJSON implements custom unmarshaling to handle the embedded store.Agent.
func (a *AgentWithCapabilities) UnmarshalJSON(data []byte) error {
	if err := json.Unmarshal(data, &a.Agent); err != nil {
		return err
	}
	type WrapperFields struct {
		Cap                 *Capabilities                    `json:"_capabilities,omitempty"`
		Messageability      *AgentMessageabilityDetail       `json:"_messageability,omitempty"`
		ResolvedHarness     string                           `json:"resolvedHarness,omitempty"`
		HarnessCapabilities *api.HarnessAdvancedCapabilities `json:"harnessCapabilities,omitempty"`
		CloudLogging        bool                             `json:"cloudLogging,omitempty"`
	}
	var wrapper WrapperFields
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return err
	}
	a.Cap = wrapper.Cap
	a.Messageability = wrapper.Messageability
	a.ResolvedHarness = wrapper.ResolvedHarness
	a.HarnessCapabilities = wrapper.HarnessCapabilities
	a.CloudLogging = wrapper.CloudLogging
	return nil
}

// ProjectWithCapabilities wraps a store.Project with capability annotations.
type ProjectWithCapabilities struct {
	store.Project
	Cap          *Capabilities `json:"_capabilities,omitempty"`
	CloudLogging bool          `json:"cloudLogging,omitempty"`
}

// MarshalJSON implements custom marshaling to avoid shadowing of fields by the embedded store.Project.
func (p ProjectWithCapabilities) MarshalJSON() ([]byte, error) {
	type ProjectAlias store.Project
	return json.Marshal(&struct {
		ProjectAlias
		Cap          *Capabilities `json:"_capabilities,omitempty"`
		CloudLogging bool          `json:"cloudLogging,omitempty"`
	}{
		ProjectAlias: ProjectAlias(p.Project),
		Cap:          p.Cap,
		CloudLogging: p.CloudLogging,
	})
}

// UnmarshalJSON implements custom unmarshaling to handle the embedded store.Project.
func (p *ProjectWithCapabilities) UnmarshalJSON(data []byte) error {
	if err := json.Unmarshal(data, &p.Project); err != nil {
		return err
	}
	type WrapperFields struct {
		Cap          *Capabilities `json:"_capabilities,omitempty"`
		CloudLogging bool          `json:"cloudLogging,omitempty"`
	}
	var wrapper WrapperFields
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return err
	}
	p.Cap = wrapper.Cap
	p.CloudLogging = wrapper.CloudLogging
	return nil
}

// TemplateWithCapabilities wraps a store.Template with capability annotations.
type TemplateWithCapabilities struct {
	store.Template
	Cap *Capabilities `json:"_capabilities,omitempty"`
}

// HarnessConfigWithCapabilities wraps a store.HarnessConfig with capability annotations.
// Unlike the other With-Capabilities wrapper types, this one has no field naming
// conflicts, so the embedded struct's default JSON marshaling is sufficient.
type HarnessConfigWithCapabilities struct {
	store.HarnessConfig
	Cap *Capabilities `json:"_capabilities,omitempty"`
}

// MarshalJSON implements custom marshaling to avoid shadowing of fields by the embedded store.Template.
func (t TemplateWithCapabilities) MarshalJSON() ([]byte, error) {
	type TemplateAlias store.Template
	return json.Marshal(&struct {
		TemplateAlias
		Cap *Capabilities `json:"_capabilities,omitempty"`
	}{
		TemplateAlias: TemplateAlias(t.Template),
		Cap:           t.Cap,
	})
}

// UnmarshalJSON implements custom unmarshaling to handle embedded store.Template.
func (t *TemplateWithCapabilities) UnmarshalJSON(data []byte) error {
	// store.Template doesn't have UnmarshalJSON, but we call it anyway for consistency
	// and to handle future-proofing if it gets one.
	type TemplateAlias store.Template
	aux := &struct {
		*TemplateAlias
		Cap *Capabilities `json:"_capabilities,omitempty"`
	}{
		TemplateAlias: (*TemplateAlias)(&t.Template),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	t.Cap = aux.Cap
	return nil
}

// GroupWithCapabilities wraps a store.Group with capability annotations.
type GroupWithCapabilities struct {
	store.Group
	Cap *Capabilities `json:"_capabilities,omitempty"`
}

// MarshalJSON implements custom marshaling to avoid shadowing of fields by the embedded store.Group.
func (g GroupWithCapabilities) MarshalJSON() ([]byte, error) {
	type GroupAlias store.Group
	return json.Marshal(&struct {
		GroupAlias
		Cap *Capabilities `json:"_capabilities,omitempty"`
	}{
		GroupAlias: GroupAlias(g.Group),
		Cap:        g.Cap,
	})
}

// UnmarshalJSON implements custom unmarshaling to handle embedded store.Group.
func (g *GroupWithCapabilities) UnmarshalJSON(data []byte) error {
	type GroupAlias store.Group
	aux := &struct {
		*GroupAlias
		Cap *Capabilities `json:"_capabilities,omitempty"`
	}{
		GroupAlias: (*GroupAlias)(&g.Group),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	g.Cap = aux.Cap
	return nil
}

// UserWithCapabilities wraps a store.User with capability annotations.
type UserWithCapabilities struct {
	store.User
	Cap *Capabilities `json:"_capabilities,omitempty"`
}

// MarshalJSON implements custom marshaling to avoid shadowing of fields by the embedded store.User.
func (u UserWithCapabilities) MarshalJSON() ([]byte, error) {
	type UserAlias store.User
	return json.Marshal(&struct {
		UserAlias
		Cap *Capabilities `json:"_capabilities,omitempty"`
	}{
		UserAlias: UserAlias(u.User),
		Cap:       u.Cap,
	})
}

// UnmarshalJSON implements custom unmarshaling to handle embedded store.User.
func (u *UserWithCapabilities) UnmarshalJSON(data []byte) error {
	type UserAlias store.User
	aux := &struct {
		*UserAlias
		Cap *Capabilities `json:"_capabilities,omitempty"`
	}{
		UserAlias: (*UserAlias)(&u.User),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	u.Cap = aux.Cap
	return nil
}

// RuntimeBrokerWithCapabilities wraps a store.RuntimeBroker with capability
// annotations and its effective agent capacity (ptone/scion#2061 P2.2,
// design.md §5.6, §5.9). AgentLimit/AgentCount/AgentLimitSource mirror the
// providers listing's projectProviderView fields exactly (same semantics:
// nil limit = unlimited, handlers_env_secrets.go) — both are populated
// through the shared resolveBrokerCapacity/brokerCapacity read model, never
// a second one (AC-P2-10). They carry no additional visibility check beyond
// the existing per-broker ActionRead capability filter in
// listRuntimeBrokers: whoever can already see a broker row sees its
// capacity fields too, the same rule the providers listing uses.
type RuntimeBrokerWithCapabilities struct {
	store.RuntimeBroker
	Cap *Capabilities `json:"_capabilities,omitempty"`
	// AgentLimit is the effective max_agents_per_broker ceiling for this
	// broker. Unset (nil) when unlimited, or when resolution didn't run or
	// failed — see resolveBrokerCapacity.
	AgentLimit *int64 `json:"agentLimit,omitempty"`
	// AgentCount is the number of active max_agents_per_broker reservations
	// held by this broker. Unset (nil) only when resolution didn't run or
	// failed — see resolveBrokerCapacity. Unlike AgentLimit, it is still
	// present (and may be a real, non-zero value) when the broker is
	// unlimited, since AgentLimit's nil there means "no cap", not "no
	// count"; a zero count is reported as 0, not omitted.
	AgentCount *int64 `json:"agentCount,omitempty"`
	// AgentLimitSource is one of the BrokerLimitSource* constants
	// (broker_capacity.go): "broker" | "entitlement" | "hub_default" |
	// "unlimited" | "not_enforced". It is the precedence step that produced
	// the result, not a statement about whether that result is a cap: when
	// the effective limit is <= 0 (unlimited), AgentLimit is absent but
	// AgentLimitSource is still whichever step produced it — "broker" for a
	// settings.maxAgents=0 override, "entitlement" or "hub_default" for a 0
	// binding or default. "unlimited" itself is reserved for
	// effectiveBrokerLimit's other branch — no limit definition or no quota
	// service configured at all — in which case brokerCapacity returns
	// before counting and resolveBrokerCapacity omits all three fields, so
	// "unlimited" is never actually observed here in practice (present in
	// the source constants, not in this field's real values).
	//
	// "not_enforced" (design.md Amendment A1, ptone/scion#2270/P1b, not yet
	// wired as of this field) means the P1b enforcement switch is off.
	// AgentLimit keeps whatever the precedence steps resolved — a cap, or
	// absent when that resolves to unlimited, exactly as in every other
	// source above — but the value is informational only and is not
	// currently enforced by Reserve. Every caller that renders AgentLimit
	// must also render AgentLimitSource, and must show "not_enforced"
	// visibly (not tooltip-only), since a limit shown without that context
	// would look enforced when it is not.
	AgentLimitSource string `json:"agentLimitSource,omitempty"`
}

// MarshalJSON implements custom marshaling to avoid shadowing of fields by the embedded store.RuntimeBroker.
func (b RuntimeBrokerWithCapabilities) MarshalJSON() ([]byte, error) {
	type BrokerAlias store.RuntimeBroker
	return json.Marshal(&struct {
		BrokerAlias
		Cap              *Capabilities `json:"_capabilities,omitempty"`
		AgentLimit       *int64        `json:"agentLimit,omitempty"`
		AgentCount       *int64        `json:"agentCount,omitempty"`
		AgentLimitSource string        `json:"agentLimitSource,omitempty"`
	}{
		BrokerAlias:      BrokerAlias(b.RuntimeBroker),
		Cap:              b.Cap,
		AgentLimit:       b.AgentLimit,
		AgentCount:       b.AgentCount,
		AgentLimitSource: b.AgentLimitSource,
	})
}

// UnmarshalJSON implements custom unmarshaling to handle embedded store.RuntimeBroker.
func (b *RuntimeBrokerWithCapabilities) UnmarshalJSON(data []byte) error {
	type BrokerAlias store.RuntimeBroker
	aux := &struct {
		*BrokerAlias
		Cap              *Capabilities `json:"_capabilities,omitempty"`
		AgentLimit       *int64        `json:"agentLimit,omitempty"`
		AgentCount       *int64        `json:"agentCount,omitempty"`
		AgentLimitSource string        `json:"agentLimitSource,omitempty"`
	}{
		BrokerAlias: (*BrokerAlias)(&b.RuntimeBroker),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	b.Cap = aux.Cap
	b.AgentLimit = aux.AgentLimit
	b.AgentCount = aux.AgentCount
	b.AgentLimitSource = aux.AgentLimitSource
	return nil
}
