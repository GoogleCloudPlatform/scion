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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Test-only helpers that exercise the SA mapping decision layer with
// allowCreate=false (experiment-off / bring-your-own GSA baseline). Keeping
// them in a _test.go file prevents production callers from accidentally
// bypassing allowCreate / hub.k8s_workload_identity_setup.

func profileSAMappingDecision(broker *store.RuntimeBroker, profileName, account string, now time.Time) SAMappingDecision {
	return profileSAMappingDecisionWithCreate(broker, profileName, account, false, now)
}

func profileSAMappingDecisionOn(brokerName string, profile *store.BrokerProfile, account string, now time.Time) SAMappingDecision {
	return profileSAMappingDecisionOnWithCreate(brokerName, profile, account, false, false, "", now)
}

func kubernetesIdentityNotMapped(broker *store.RuntimeBroker, profileName string, gcpID *store.GCPIdentityConfig, now time.Time) *identityNotMappedPrecheck {
	return kubernetesIdentityNotMappedWithCreate(broker, profileName, gcpID, false, now)
}

func (p kubernetesProfileMappings) decision(account string, now time.Time) SAMappingDecision {
	return p.decisionFor(account, false, now)
}

func (v projectSAMappingView) warningsAt(sas []*store.GCPServiceAccount, now time.Time) []string {
	return v.warningsAtWithExperiment(sas, false, now)
}

func gcpSAProfileMappings(email string, profiles []kubernetesProfileMappings, now time.Time) []GCPServiceAccountProfileMapping {
	return gcpSAProfileMappingsFor(email, false, profiles, now)
}
