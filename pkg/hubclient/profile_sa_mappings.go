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

package hubclient

// ProfileSAMappingsState is one Kubernetes profile's GCP service account
// mappings in a heartbeat (BrokerHeartbeat.ProfileSAMappings): the GSAs the
// profile maps to a Kubernetes ServiceAccount in the broker's global
// kubernetes_service_account_mappings (profile plus runtime entry).
//
// The broker sends the list on its first heartbeat after start and whenever
// it changes; the Hub stores it on the named profile
// (store.BrokerProfile.ServiceAccountMappings, MappingsReported) and uses it
// only to warn about registered service accounts no profile maps
// (ptone/scion#3329 phase 2). An entry with an empty list means the profile
// maps nothing; a profile without an entry keeps what the Hub has stored.
// The Hub ignores names it has no stored profile for, and drops the field
// for brokers that store no profiles (flat Runtime Broker rows).
type ProfileSAMappingsState struct {
	Name                   string                   `json:"name"`
	ServiceAccountMappings []BrokerProfileSAMapping `json:"serviceAccountMappings"`
}
