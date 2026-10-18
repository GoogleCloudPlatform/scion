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

package runtime

import (
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// labelKeyRewriter is implemented by a runtime that stores labels under
// different keys than the ones it is given, because it rewrites label keys
// to its platform's rules.
type labelKeyRewriter interface {
	storedLabelKey(key string) string
}

// LabelKey returns the key under which rt stores the label key on the
// objects it creates, and so the key its List returns it under. Ownership
// decides which Runtime Broker instance manages which runtime objects, so
// the manager that stamps the reserved owner label
// (api.LabelRuntimeBrokerID) and every check that reads it, or the
// project, agent and name labels next to it, use this one mapping. It is
// key itself for every runtime that keeps label keys as given; only a
// runtime that rewrites keys (Cloud Run Instances) returns its rewritten
// form. A runtime never has two keys for one label.
func LabelKey(rt Runtime, key string) string {
	if k, ok := rt.(labelKeyRewriter); ok {
		return k.storedLabelKey(key)
	}
	return key
}

// storedLabelKey is the GCP-sanitized label key: Run rewrites every label
// key with sanitizeGCPLabelKey, and List returns the stored labels as they
// are.
func (r *CloudRunRuntime) storedLabelKey(key string) string {
	return sanitizeGCPLabelKey(key)
}

var _ labelKeyRewriter = (*CloudRunRuntime)(nil)

// checkOwnerLabel refuses a launch whose ownership labels Run could not
// store exactly. Run rewrites every label key and value to GCP's rules, so
// two keys can land on the same stored key and two values on the same
// stored value. These cases are refused before anything is provisioned:
//   - another label key that rewrites to the owner label key, since the
//     stored owner value would then depend on which key was written last;
//   - an owner value the rewrite would change, since two Runtime Broker IDs
//     could then be stored as the same value;
//   - for an owned launch (owner label set), a project ID, agent ID or name
//     value the rewrite would change, since ownership checks match those
//     stored values against the IDs and slugs the instance records.
//
// Accepted values are stored exactly as given, so an owned instance's labels
// always name exactly the instance, project, agent and slug that created it.
func (r *CloudRunRuntime) checkOwnerLabel(cfg RunConfig) error {
	key := r.storedLabelKey(api.LabelRuntimeBrokerID)
	for k := range cfg.Labels {
		if k != key && sanitizeGCPLabelKey(k) == key {
			return fmt.Errorf("cloudrun: label %q would be stored under the owner label key %q", k, key)
		}
	}
	v := cfg.Labels[key]
	if sanitizeGCPLabelValue(v) != v {
		return fmt.Errorf("cloudrun: owner label value %q is not a valid Cloud Run label value and would be rewritten", v)
	}
	if v == "" {
		return nil
	}
	for _, k := range ownedIdentityLabels {
		if iv := cfg.Labels[k]; sanitizeGCPLabelValue(iv) != iv {
			return fmt.Errorf("cloudrun: label %s value %q is not a valid Cloud Run label value and would be rewritten", k, iv)
		}
	}
	return nil
}

// ownedIdentityLabels are the labels, besides the owner label, that
// ownership checks read from an owned runtime object: its project ID, agent
// ID and slug.
var ownedIdentityLabels = []string{"scion.project_id", "agent_id", "scion.name"}
