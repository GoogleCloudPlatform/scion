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
// report in a heartbeat (BrokerHeartbeat.ProfileSAMappings): the GSAs the
// profile can serve and the Kubernetes ServiceAccount (KSA) each runs as,
// from the broker's global kubernetes_service_account_mappings (profile
// plus runtime entry) and from ServiceAccount annotation discovery in the
// profile's namespace (ptone/scion#3329 phases 2 and 4).
//
// The broker sends the full report of every Kubernetes profile when any
// profile's report changes, and when the Hub asks for it
// (BrokerHeartbeatResponse.ProfileSAMappingsRequested). Every heartbeat
// carries the hash of each profile's report
// (BrokerHeartbeat.ProfileSAMappingsHashes), so the Hub can confirm that
// its stored copy is current without the full list. The Hub stores the
// report on the named profile (store.BrokerProfile.ServiceAccountMappings
// and related fields). An entry with an empty list means the profile maps
// nothing. The Hub ignores names it has no stored profile for, and drops
// the field for brokers that store no profiles (flat Runtime Broker rows).
//
// An entry means "mapped", not "ready": the broker cannot see whether the
// Workload Identity IAM binding exists.
type ProfileSAMappingsState struct {
	Name                   string                   `json:"name"`
	ServiceAccountMappings []BrokerProfileSAMapping `json:"serviceAccountMappings"`
	// Complete is true when the report lists every GSA the profile can
	// serve: the explicit mappings were read and annotation discovery
	// listed the namespace. False (also when omitted, as by an older
	// broker) means other GSAs may still be usable.
	Complete bool `json:"complete,omitempty"`
	// IncompleteReason is a fixed code saying why Complete is false
	// (api.BrokerKSADiscoveryUnavailable, api.BrokerKSADiscoveryListFailed,
	// api.BrokerKSADiscoveryPending or api.BrokerSAReportForceRuntime).
	// Empty when Complete is true.
	IncompleteReason string `json:"incompleteReason,omitempty"`
	// AmbiguousGSAs lists, sorted, the GSAs with no explicit mapping that
	// more than one KSA in the namespace is annotated with. The broker
	// refuses an assign dispatch for them, so they are not in
	// ServiceAccountMappings.
	AmbiguousGSAs []string `json:"ambiguousGSAs,omitempty"`
}

// ProfileSAMappingsHash is the hash of one profile's ProfileSAMappingsState,
// sent on every heartbeat (BrokerHeartbeat.ProfileSAMappingsHashes).
type ProfileSAMappingsHash struct {
	Name string `json:"name"`
	Hash string `json:"hash"`
}

// BrokerHeartbeatResponse is the Hub's reply to a heartbeat. A Hub that
// predates it replies with an empty body, which Heartbeat returns as nil.
type BrokerHeartbeatResponse struct {
	// ProfileSAMappingsHashes is true when the Hub reads
	// BrokerHeartbeat.ProfileSAMappingsHashes and asks for the full report
	// when it needs it. Only then does the broker stop re-sending an
	// unchanged report on a timer.
	ProfileSAMappingsHashes bool `json:"profileSAMappingsHashes,omitempty"`
	// ProfileSAMappingsRequested is true when a reported hash does not
	// match the Hub's stored report (or the Hub has none): the broker sends
	// the full report on its next heartbeat.
	ProfileSAMappingsRequested bool `json:"profileSAMappingsRequested,omitempty"`
}
