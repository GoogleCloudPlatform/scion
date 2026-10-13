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

package config

import "context"

// FlatInstancePolicy is the local policy of one flat Runtime Broker instance
// (.design/flat-runtime-brokers/contract-appendix-p3.md FC5): the typed
// values of its server.broker.instances entry that replace the Runtime
// Broker Profile tier for every request the instance serves. It is a deep
// copy built once per instance server and must never be modified after
// construction; concurrent requests share it read-only.
type FlatInstancePolicy struct {
	// InstanceKey is the instance's key, used only in source labels.
	InstanceKey string
	// Target is a deep copy of the instance's runtime_target (the zero
	// value when the entry has none).
	Target V1RuntimeTargetConfig
	// Defaults is a deep copy of the instance's agent_defaults (the zero
	// value when the entry has none).
	Defaults V1InstanceAgentDefaults
}

// NewFlatInstancePolicy returns the policy of inst, deep-copied so that no
// map or pointer is shared with inst.
func NewFlatInstancePolicy(inst V1RuntimeBrokerInstanceConfig) *FlatInstancePolicy {
	p := &FlatInstancePolicy{InstanceKey: inst.Key}
	if inst.RuntimeTarget != nil {
		// Every pointer, map or slice field of V1RuntimeTargetConfig must
		// be copied here, so the policy never shares state with the entry
		// (TestNewFlatInstancePolicy_CopiesEveryReferenceField enforces it).
		p.Target = *inst.RuntimeTarget
		p.Target.KubernetesServiceAccountMappings = copyStringMap(inst.RuntimeTarget.KubernetesServiceAccountMappings)
	}
	if d := copyInstanceAgentDefaults(inst.AgentDefaults); d != nil {
		p.Defaults = *d
	}
	return p
}

// path returns the settings path of a field of this instance, for example
// "server.broker.instances[k8s-1].runtime_target.shared_dir_size", so an
// error about a value can name where it is set.
func (p *FlatInstancePolicy) path(field string) string {
	return "server.broker.instances[" + p.InstanceKey + "]." + field
}

type flatInstancePolicyKey struct{}

// WithFlatInstancePolicy returns ctx carrying p. A nil p leaves the request
// on legacy (profile) resolution.
func WithFlatInstancePolicy(ctx context.Context, p *FlatInstancePolicy) context.Context {
	return context.WithValue(ctx, flatInstancePolicyKey{}, p)
}

// FlatInstancePolicyFrom returns the policy ctx carries, or nil for a legacy
// request.
func FlatInstancePolicyFrom(ctx context.Context) *FlatInstancePolicy {
	if ctx == nil {
		return nil
	}
	p, _ := ctx.Value(flatInstancePolicyKey{}).(*FlatInstancePolicy)
	return p
}

// ForFlatInstance returns the settings view a request served by the flat
// instance with policy p reads: no profiles and no active profile, so any
// profile-tier lookup without a flat branch finds nothing, plus p, which the
// resolvers' flat branches read in place of the profile and runtime-entry
// tier. Every other setting is shared unchanged. vs and its maps are never
// modified. A nil p returns vs itself (legacy resolution).
func (vs *VersionedSettings) ForFlatInstance(p *FlatInstancePolicy) *VersionedSettings {
	if vs == nil || p == nil {
		return vs
	}
	view := *vs
	view.ActiveProfile = ""
	view.Profiles = nil
	view.flat = p
	return &view
}
