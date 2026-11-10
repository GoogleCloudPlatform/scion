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

// IsSuperAdminIssueVariant is the one normaliser the offline writer and the
// hub share: every spelling of the super-admin permission or scope is a
// variant; the member and hub-admin scopes and permissions are not.
func TestIsSuperAdminIssueVariant(t *testing.T) {
	for _, v := range []string{
		"test_identity:issue_super_admin", "test_identity.issue_super_admin",
		"TEST_IDENTITY.ISSUE_SUPER_ADMIN", "Test_Identity:Issue_Super_Admin",
		" test_identity.issue_super_admin ", "test_identity.issue_super-admin",
		"test_identity.issue_superadmin", "x:super-admin", "superadmin",
	} {
		if !IsSuperAdminIssueVariant(v) {
			t.Errorf("%q: want a super-admin variant", v)
		}
	}
	for _, v := range []string{
		TestInfraOfflineDelegationScope, TestInfraOfflineAdminDelegationScope,
		"test_identity.issue", "test_identity.issue_admin", "", "test_identity:issue_admin ",
	} {
		if IsSuperAdminIssueVariant(v) {
			t.Errorf("%q: want not a super-admin variant", v)
		}
	}
}
