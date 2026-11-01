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

//go:build !no_sqlite

package hub

import (
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
)

// The authorized_domains save check's test identity clamp
// (ptone/scion#6000) with the test admin tier ON and a store-backed kind
// lookup: a real (human) admin keeps the admin_emails bypass, and an issued
// test super-admin loses it. TestRejectAuthorizedDomainsLockout_TestIdentityClamp
// covers the fail-closed path (no store).
func TestRejectAuthorizedDomainsLockout_ClampStoreBacked(t *testing.T) {
	srv, s, _ := tsaServer(t)
	issuer := taUser(t, s, "ad-clamp-iss", store.SystemRoleSuperAdmin)
	resp := tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin)
	humanCtx := contextWithIdentityAndUser(tsaIdentity(t, s, issuer.ID))
	testCtx := contextWithIdentityAndUser(tsaIdentity(t, s, resp.Identity.ID))
	domains := []string{"other.example"}

	// A real admin listed in admin_emails is allowed, tier on.
	rr := httptest.NewRecorder()
	assert.False(t, srv.rejectAuthorizedDomainsLockout(humanCtx, rr, issuer.Email, domains, []string{issuer.Email}),
		"a real admin in admin_emails keeps the bypass: %d %s", rr.Code, rr.Body.String())

	// Control: the same real admin without the admin_emails entry is refused.
	rr = httptest.NewRecorder()
	assert.True(t, srv.rejectAuthorizedDomainsLockout(humanCtx, rr, issuer.Email, domains, nil),
		"a real admin outside admin_emails and the domain list is refused")

	// An issued test super-admin gets no bypass even when listed.
	rr = httptest.NewRecorder()
	assert.True(t, srv.rejectAuthorizedDomainsLockout(testCtx, rr, resp.Identity.Email, domains, []string{resp.Identity.Email}),
		"a test super-admin does not count as a real admin")
}
