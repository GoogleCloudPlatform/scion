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

package store

// HostSATranslationCreator is the CreatedBy of the hub-scoped records the Hub
// keeps for a broker's host service account when it translates GCP identity
// passthrough to assign mode on a sandbox profile. No API sets it: service
// account create endpoints record the caller's user ID, and updates never
// change CreatedBy.
const HostSATranslationCreator = "system:passthrough-translation"

// IsHostSATranslationRecord reports whether sa is such a record. It is the
// single test for one, shared by every surface that refuses these records as
// a direct reference (agent assignment and defaults, lifecycle-hook execution
// identities) and by the start, token-mint and move checks.
func IsHostSATranslationRecord(sa *GCPServiceAccount) bool {
	return sa != nil && sa.Scope == ScopeHub && sa.CreatedBy == HostSATranslationCreator
}

// HostSATranslationRecordBoundElsewhere reports whether sa is a translation
// record bound to a broker other than brokerID. The binding is the record's
// ScopeID, which holds the broker it was created for. A record with an empty
// ScopeID carries no binding and is never reported as bound elsewhere. The
// store does not accept an empty ScopeID (the schema requires a non-empty
// scope_id), so that branch is defensive.
func HostSATranslationRecordBoundElsewhere(sa *GCPServiceAccount, brokerID string) bool {
	return IsHostSATranslationRecord(sa) && sa.ScopeID != "" && sa.ScopeID != brokerID
}
