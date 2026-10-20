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

import "testing"

func TestIsHostSATranslationRecord(t *testing.T) {
	cases := []struct {
		name string
		sa   *GCPServiceAccount
		want bool
	}{
		{"nil", nil, false},
		{"translation record", &GCPServiceAccount{Scope: ScopeHub, CreatedBy: HostSATranslationCreator}, true},
		{"hub record by a user", &GCPServiceAccount{Scope: ScopeHub, CreatedBy: "user-1"}, false},
		{"project record with the creator", &GCPServiceAccount{Scope: ScopeProject, CreatedBy: HostSATranslationCreator}, false},
	}
	for _, tc := range cases {
		if got := IsHostSATranslationRecord(tc.sa); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestHostSATranslationRecordNotBoundTo(t *testing.T) {
	rec := func(scopeID string) *GCPServiceAccount {
		return &GCPServiceAccount{Scope: ScopeHub, ScopeID: scopeID, CreatedBy: HostSATranslationCreator}
	}
	if HostSATranslationRecordNotBoundTo(rec("broker-a"), "broker-a") {
		t.Error("a record is usable on the broker it is bound to")
	}
	if !HostSATranslationRecordNotBoundTo(rec("broker-a"), "broker-b") {
		t.Error("a record bound to another broker is not usable")
	}
	if !HostSATranslationRecordNotBoundTo(rec("broker-a"), "") {
		t.Error("an agent without a broker is not on the record's broker")
	}
	if !HostSATranslationRecordNotBoundTo(rec(""), "broker-b") {
		t.Error("a record with no broker binding is usable on no broker")
	}
	if !HostSATranslationRecordNotBoundTo(rec(""), "") {
		t.Error("a record with no broker binding is not usable even without a broker")
	}
	if HostSATranslationRecordNotBoundTo(&GCPServiceAccount{Scope: ScopeHub, ScopeID: "", CreatedBy: "user-1"}, "broker-b") {
		t.Error("only translation records are checked")
	}
	if HostSATranslationRecordNotBoundTo(&GCPServiceAccount{Scope: ScopeProject, ScopeID: "p1", CreatedBy: HostSATranslationCreator}, "broker-b") {
		t.Error("a project record is not a translation record")
	}
}
