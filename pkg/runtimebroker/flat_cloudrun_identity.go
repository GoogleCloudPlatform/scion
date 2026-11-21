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
	"fmt"
	"net/http"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// flatCloudRunIdentityPolicy is a flat Cloud Run instance's own GCP identity
// policy (ptone/scion#8182, ptone/scion#7764): its configured service_account,
// required block_service_account, and optional assignable_service_accounts
// allow-list from its runtime_target. It is a snapshot taken when the server
// is built and never changes; a change to the settings file takes effect at
// the next instance restart. A flat instance reads its identity policy only
// from here: never from profiles, runtime entries, the Hub database overlay
// or a project's settings.
type flatCloudRunIdentityPolicy struct {
	serviceAccount      string
	blockServiceAccount string
	assignable          map[string]struct{}
}

// newFlatCloudRunIdentityPolicy copies and normalizes the instance's policy.
func newFlatCloudRunIdentityPolicy(fi *FlatInstanceConfig) flatCloudRunIdentityPolicy {
	if fi == nil || fi.Instance.RuntimeTarget == nil {
		return flatCloudRunIdentityPolicy{}
	}
	t := fi.Instance.RuntimeTarget
	p := flatCloudRunIdentityPolicy{
		serviceAccount:      strings.ToLower(strings.TrimSpace(t.ServiceAccount)),
		blockServiceAccount: strings.ToLower(strings.TrimSpace(t.BlockServiceAccount)),
	}
	if len(t.AssignableServiceAccounts) > 0 {
		p.assignable = make(map[string]struct{}, len(t.AssignableServiceAccounts))
		for _, sa := range t.AssignableServiceAccounts {
			if norm := strings.ToLower(strings.TrimSpace(sa)); norm != "" {
				p.assignable[norm] = struct{}{}
			}
		}
	}
	return p
}

// blockAccount returns the instance's normalized block_service_account and
// whether one is configured.
func (p flatCloudRunIdentityPolicy) blockAccount() (string, bool) {
	return p.blockServiceAccount, p.blockServiceAccount != ""
}

// instanceAccount returns the instance's normalized runtime_target.service_account
// and whether one is configured.
func (p flatCloudRunIdentityPolicy) instanceAccount() (string, bool) {
	return p.serviceAccount, p.serviceAccount != ""
}

// accountAssignable reports whether saEmail is listed in the instance's
// runtime_target.assignable_service_accounts allow-list. An empty or absent
// allow-list allows no account (fail closed).
func (p flatCloudRunIdentityPolicy) accountAssignable(saEmail string) bool {
	if len(p.assignable) == 0 {
		return false
	}
	norm := strings.ToLower(strings.TrimSpace(saEmail))
	if norm == "" {
		return false
	}
	_, ok := p.assignable[norm]
	return ok
}

// flatCloudRunBlockUnsetRefusal refuses a flat Cloud Run dispatch when the
// instance's snapshot holds no block_service_account (defense in depth behind
// static instance validation). It names the agent and the setting path only,
// never an account.
func flatCloudRunBlockUnsetRefusal(agentName, instanceKey string) *startContextError {
	if instanceKey == "" {
		instanceKey = "<key>"
	}
	return &startContextError{
		Status: http.StatusBadRequest,
		Message: fmt.Sprintf("agent %q: flat Cloud Run instance %q has no block service account; set server.broker.instances[%s].runtime_target.block_service_account",
			agentName, instanceKey, instanceKey),
	}
}

// flatCloudRunServiceAccountUnsetRefusal refuses an explicit passthrough
// dispatch on a flat Cloud Run instance whose snapshot holds no
// runtime_target.service_account (defense in depth behind static instance
// validation). It names the agent and the setting path only, never an account.
func flatCloudRunServiceAccountUnsetRefusal(agentName, instanceKey string) *startContextError {
	if instanceKey == "" {
		instanceKey = "<key>"
	}
	return &startContextError{
		Status: http.StatusBadRequest,
		Message: fmt.Sprintf("agent %q: flat Cloud Run instance %q has no service account; set server.broker.instances[%s].runtime_target.service_account",
			agentName, instanceKey, instanceKey),
	}
}

// flatCloudRunNotAttachableRefusal refuses an assign dispatch whose service
// account is not listed in the flat Cloud Run instance's
// runtime_target.assignable_service_accounts allow-list. Status is 400 with
// the default validation_error code, matching legacy Cloud Run so Hub
// relayBrokerRefusal forwards the setting path verbatim without Kubernetes
// mapping prose. The message never carries the service account email or
// project.
func flatCloudRunNotAttachableRefusal(agentName, instanceKey string) *startContextError {
	if instanceKey == "" {
		instanceKey = "<key>"
	}
	return &startContextError{
		Status: http.StatusBadRequest,
		Message: fmt.Sprintf(
			"agent %q: GCP identity mode %q on Cloud Run Instances: service_account is not attachable on flat Cloud Run instance %q; add it to server.broker.instances[%s].runtime_target.assignable_service_accounts",
			agentName, store.GCPMetadataModeAssign, instanceKey, instanceKey),
	}
}
