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

package api

// ResolvedPlacement is the Kubernetes scheduling placement resolved for one
// agent start from the agent runtime's settings: a node selector,
// tolerations and a runtime class. It is computed per start and never
// persisted.
type ResolvedPlacement struct {
	NodeSelector     map[string]string `json:"nodeSelector,omitempty"`
	Tolerations      []K8sToleration   `json:"tolerations,omitempty"`
	RuntimeClassName string            `json:"runtimeClassName,omitempty"`
	// Sources records, per field, the settings keys that contributed the
	// value (for logs only).
	Sources map[string][]string `json:"sources,omitempty"`
	// OperatorOwned marks placement the operator owns: the agent's own
	// kubernetes block may add node selector keys but not change the
	// operator's keys or runtime class, its tolerations must be among the
	// operator's (none when the operator sets none), and it is checked even
	// when no placement is in effect (config.ApplyOperatorPlacement). Set
	// for flat instance starts. Never sent or persisted.
	OperatorOwned bool `json:"-"`
}

// InEffect reports whether placement is in effect: the placement is
// non-nil and any of NodeSelector, Tolerations or RuntimeClassName is
// non-empty. Sources alone does not put placement in effect.
func (p *ResolvedPlacement) InEffect() bool {
	if p == nil {
		return false
	}
	return len(p.NodeSelector) > 0 || len(p.Tolerations) > 0 || p.RuntimeClassName != ""
}
