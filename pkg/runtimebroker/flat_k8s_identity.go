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

package runtimebroker

import (
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// flatKubernetesIdentityPolicy is a flat Kubernetes instance's own GCP
// identity policy (ptone/scion#3274 amendment): the "block" ServiceAccount
// and the "assign" GSA-to-KSA mapping from its runtime_target. It is a
// snapshot taken when the server is built and never changes; a change to
// the settings file takes effect at the next instance restart. A flat
// instance reads its identity policy only from here: never from profiles,
// runtime entries, the Hub database overlay or a project's settings.
type flatKubernetesIdentityPolicy struct {
	blockServiceAccount string
	mappings            map[string]string
}

// newFlatKubernetesIdentityPolicy copies the instance's policy.
func newFlatKubernetesIdentityPolicy(fi *FlatInstanceConfig) flatKubernetesIdentityPolicy {
	if fi == nil || fi.Instance.RuntimeTarget == nil {
		return flatKubernetesIdentityPolicy{}
	}
	t := fi.Instance.RuntimeTarget
	p := flatKubernetesIdentityPolicy{blockServiceAccount: t.KubernetesBlockServiceAccount}
	if len(t.KubernetesServiceAccountMappings) > 0 {
		p.mappings = make(map[string]string, len(t.KubernetesServiceAccountMappings))
		for k, v := range t.KubernetesServiceAccountMappings {
			p.mappings[k] = v
		}
	}
	return p
}

// blockAccount returns the instance's block ServiceAccount and
// whether one is configured (an omitted one means the namespace's default
// ServiceAccount).
func (p flatKubernetesIdentityPolicy) blockAccount() (string, bool) {
	return p.blockServiceAccount, p.blockServiceAccount != ""
}

// mappedServiceAccount returns the KSA the instance maps saEmail to.
func (p flatKubernetesIdentityPolicy) mappedServiceAccount(saEmail string) (string, bool) {
	ksa, ok := p.mappings[saEmail]
	return ksa, ok && ksa != ""
}

// flatDispatchSelection is a flat instance's dispatch selection: its own
// instance key and persisted runtime target ID, with no profile or runtime
// entry. Early and late identity resolution compare it like a legacy
// selection; it is never a settings lookup key.
func (s *Server) flatDispatchSelection() dispatchProfileSelection {
	fi := s.flatInstance()
	if fi == nil {
		return dispatchProfileSelection{}
	}
	return dispatchProfileSelection{InstanceKey: fi.Instance.Key, RuntimeTargetID: fi.Identity.RuntimeTarget.ID}
}

// flatInstanceNamespace is the namespace a flat Kubernetes instance places
// pods in: its runtime's namespace (the activated target), else the
// namespace recorded in its execution scope.
func (s *Server) flatInstanceNamespace() string {
	if _, rt := s.defaultPair(); rt != nil {
		if k8s, ok := rt.(*scionrt.KubernetesRuntime); ok && k8s.DefaultNamespace != "" {
			return k8s.DefaultNamespace
		}
	}
	if fi := s.flatInstance(); fi != nil && fi.Identity.ExecutionScope.Kubernetes != nil {
		return fi.Identity.ExecutionScope.Kubernetes.Namespace
	}
	return ""
}
