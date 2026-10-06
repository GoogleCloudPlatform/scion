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

// HarnessConfigPolicyFunc evaluates a dispatch policy against the
// harness-config an agent launch has resolved: name is the resolved
// harness-config name and entry is the effective entry exactly as it will
// run (the resolved directory's config.yaml with the settings overlay
// applied). A non-nil error refuses the launch.
//
// It lives in pkg/config rather than pkg/api because its signature uses
// HarnessConfigEntry, and pkg/config imports pkg/api.
type HarnessConfigPolicyFunc func(name string, entry HarnessConfigEntry) error

type harnessConfigPolicyKey struct{}

// ContextWithHarnessConfigPolicy returns ctx carrying policy, which pkg/agent
// evaluates where launch resolves the harness-config (template and
// harness-config resolution, and each harness construction). The runtime
// broker attaches its container-script policy this way.
func ContextWithHarnessConfigPolicy(ctx context.Context, policy HarnessConfigPolicyFunc) context.Context {
	return context.WithValue(ctx, harnessConfigPolicyKey{}, policy)
}

// HarnessConfigPolicyFromContext returns the policy attached to ctx, or nil.
func HarnessConfigPolicyFromContext(ctx context.Context) HarnessConfigPolicyFunc {
	if ctx == nil {
		return nil
	}
	p, _ := ctx.Value(harnessConfigPolicyKey{}).(HarnessConfigPolicyFunc)
	return p
}
