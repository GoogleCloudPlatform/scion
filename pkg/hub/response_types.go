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

// ProjectWithCapabilities wraps a store.Project with capability annotations.
type ProjectWithCapabilities struct {
	store.Project
	Cap          *Capabilities `json:"_capabilities,omitempty"`
	CloudLogging bool          `json:"cloudLogging,omitempty"`
}

// TemplateWithCapabilities wraps a store.Template with capability annotations.
type TemplateWithCapabilities struct {
	store.Template
	Cap *Capabilities `json:"_capabilities,omitempty"`
}

// GroupWithCapabilities wraps a store.Group with capability annotations.
type GroupWithCapabilities struct {
	store.Group
	Cap *Capabilities `json:"_capabilities,omitempty"`
}

// UserWithCapabilities wraps a store.User with capability annotations.
type UserWithCapabilities struct {
	store.User
	Cap *Capabilities `json:"_capabilities,omitempty"`
}

// PolicyWithCapabilities wraps a store.Policy with capability annotations.
type PolicyWithCapabilities struct {
	store.Policy
	Cap *Capabilities `json:"_capabilities,omitempty"`
}

// RuntimeBrokerWithCapabilities wraps a store.RuntimeBroker with capability annotations.
type RuntimeBrokerWithCapabilities struct {
	store.RuntimeBroker
	Cap *Capabilities `json:"_capabilities,omitempty"`
}
