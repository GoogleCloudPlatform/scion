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
		ResolvedHarness     string                           `json:"resolvedHarness,omitempty"`
		HarnessCapabilities *api.HarnessAdvancedCapabilities `json:"harnessCapabilities,omitempty"`
		CloudLogging        bool                             `json:"cloudLogging,omitempty"`
		GroveID             string                           `json:"groveId"`
	}{
		AgentAlias:          AgentAlias(a.Agent),
		Cap:                 a.Cap,
		ResolvedHarness:     a.ResolvedHarness,
		HarnessCapabilities: a.HarnessCapabilities,
		CloudLogging:        a.CloudLogging,
		GroveID:             a.ProjectID,
	})
}

// UnmarshalJSON implements custom unmarshaling to handle embedded store.Agent and legacy fields.
func (a *AgentWithCapabilities) UnmarshalJSON(data []byte) error {
	if err := a.Agent.UnmarshalJSON(data); err != nil {
		return err
	}
	type WrapperFields struct {
		Cap                 *Capabilities                    `json:"_capabilities,omitempty"`
		ResolvedHarness     string                           `json:"resolvedHarness,omitempty"`
		HarnessCapabilities *api.HarnessAdvancedCapabilities `json:"harnessCapabilities,omitempty"`
		CloudLogging        bool                             `json:"cloudLogging,omitempty"`
	}
	var wrapper WrapperFields
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return err
	}
	a.Cap = wrapper.Cap
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
		GroveID      string        `json:"groveId"`
		GroveName    string        `json:"groveName"`
		Grove        string        `json:"grove"`
	}{
		ProjectAlias: ProjectAlias(p.Project),
		Cap:          p.Cap,
		CloudLogging: p.CloudLogging,
		GroveID:      p.ID,
		GroveName:    p.Name,
		Grove:        p.Slug,
	})
}

// UnmarshalJSON implements custom unmarshaling to handle embedded store.Project and legacy fields.
func (p *ProjectWithCapabilities) UnmarshalJSON(data []byte) error {
	if err := p.Project.UnmarshalJSON(data); err != nil {
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

// MarshalJSON implements custom marshaling to avoid shadowing of fields by the embedded store.Template.
func (t TemplateWithCapabilities) MarshalJSON() ([]byte, error) {
	type TemplateAlias store.Template
	return json.Marshal(&struct {
		TemplateAlias
		Cap     *Capabilities `json:"_capabilities,omitempty"`
		GroveID string        `json:"groveId,omitempty"`
	}{
		TemplateAlias: TemplateAlias(t.Template),
		Cap:           t.Cap,
		GroveID:       t.ProjectID,
	})
}

// UnmarshalJSON implements custom unmarshaling to handle embedded store.Template and legacy fields.
func (t *TemplateWithCapabilities) UnmarshalJSON(data []byte) error {
	// store.Template doesn't have UnmarshalJSON, but we call it anyway for consistency
	// and to handle future-proofing if it gets one.
	type TemplateAlias store.Template
	aux := &struct {
		*TemplateAlias
		Cap     *Capabilities `json:"_capabilities,omitempty"`
		GroveID string        `json:"groveId,omitempty"`
	}{
		TemplateAlias: (*TemplateAlias)(&t.Template),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	t.Cap = aux.Cap
	if t.ProjectID == "" && aux.GroveID != "" {
		t.ProjectID = aux.GroveID
	}
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
		Cap     *Capabilities `json:"_capabilities,omitempty"`
		GroveID string        `json:"groveId,omitempty"`
	}{
		GroupAlias: GroupAlias(g.Group),
		Cap:        g.Cap,
		GroveID:    g.ProjectID,
	})
}

// UnmarshalJSON implements custom unmarshaling to handle embedded store.Group and legacy fields.
func (g *GroupWithCapabilities) UnmarshalJSON(data []byte) error {
	type GroupAlias store.Group
	aux := &struct {
		*GroupAlias
		Cap     *Capabilities `json:"_capabilities,omitempty"`
		GroveID string        `json:"groveId,omitempty"`
	}{
		GroupAlias: (*GroupAlias)(&g.Group),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	g.Cap = aux.Cap
	if g.ProjectID == "" && aux.GroveID != "" {
		g.ProjectID = aux.GroveID
	}
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

// PolicyWithCapabilities wraps a store.Policy with capability annotations.
type PolicyWithCapabilities struct {
	store.Policy
	Cap *Capabilities `json:"_capabilities,omitempty"`
}

// MarshalJSON implements custom marshaling to avoid shadowing of fields by the embedded store.Policy.
func (p PolicyWithCapabilities) MarshalJSON() ([]byte, error) {
	type PolicyAlias store.Policy
	return json.Marshal(&struct {
		PolicyAlias
		Cap     *Capabilities `json:"_capabilities,omitempty"`
		GroveID string        `json:"groveId,omitempty"`
	}{
		PolicyAlias: PolicyAlias(p.Policy),
		Cap:         p.Cap,
		// For policies, GroveID is the ScopeID if ScopeType is "project"
		GroveID: func() string {
			if p.ScopeType == store.PolicyScopeProject {
				return p.ScopeID
			}
			return ""
		}(),
	})
}

// UnmarshalJSON implements custom unmarshaling to handle embedded store.Policy and legacy fields.
func (p *PolicyWithCapabilities) UnmarshalJSON(data []byte) error {
	type PolicyAlias store.Policy
	aux := &struct {
		*PolicyAlias
		Cap     *Capabilities `json:"_capabilities,omitempty"`
		GroveID string        `json:"groveId,omitempty"`
	}{
		PolicyAlias: (*PolicyAlias)(&p.Policy),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	p.Cap = aux.Cap
	if p.ScopeType == store.PolicyScopeProject && p.ScopeID == "" && aux.GroveID != "" {
		p.ScopeID = aux.GroveID
	}
	return nil
}

// RuntimeBrokerWithCapabilities wraps a store.RuntimeBroker with capability annotations.
type RuntimeBrokerWithCapabilities struct {
	store.RuntimeBroker
	Cap *Capabilities `json:"_capabilities,omitempty"`
}

// MarshalJSON implements custom marshaling to avoid shadowing of fields by the embedded store.RuntimeBroker.
func (b RuntimeBrokerWithCapabilities) MarshalJSON() ([]byte, error) {
	type BrokerAlias store.RuntimeBroker
	return json.Marshal(&struct {
		BrokerAlias
		Cap *Capabilities `json:"_capabilities,omitempty"`
	}{
		BrokerAlias: BrokerAlias(b.RuntimeBroker),
		Cap:         b.Cap,
	})
}

// UnmarshalJSON implements custom unmarshaling to handle embedded store.RuntimeBroker.
func (b *RuntimeBrokerWithCapabilities) UnmarshalJSON(data []byte) error {
	type BrokerAlias store.RuntimeBroker
	aux := &struct {
		*BrokerAlias
		Cap *Capabilities `json:"_capabilities,omitempty"`
	}{
		BrokerAlias: (*BrokerAlias)(&b.RuntimeBroker),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	b.Cap = aux.Cap
	return nil
}
