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

package artifacts

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// canRetire on GET (ptone/scion#4535): the flag is the delete route's
// decision (retireAuthority) for the same caller.

// canRetireCase is one caller on an artifact and the expected canRetire.
type canRetireCase struct {
	name string
	// fixture returns the fixture, the artifact and the caller.
	fixture func(t *testing.T) (*fixture, string, principal)
	retire  bool
}

func canRetireCases() []canRetireCase {
	steward := func(p principal, setup func(f *stewardFixture)) func(t *testing.T) (*fixture, string, principal) {
		return func(t *testing.T) (*fixture, string, principal) {
			f := newStewardFixture(t)
			if setup != nil {
				setup(f)
			}
			return f.fixture, f.id, p
		}
	}
	return []canRetireCase{
		{name: "owner", retire: true, fixture: func(t *testing.T) (*fixture, string, principal) {
			f, id := newLinkFixture(t)
			return f, id, userU
		}},
		{name: "admin grantee", retire: true, fixture: steward(stAdminGrantee, nil)},
		{name: "steward project owner", retire: true, fixture: steward(ownerSteward, nil)},
		{name: "steward delegator", retire: true, fixture: steward(delegSteward, nil)},
		{name: "home-project admin custodian", retire: true, fixture: steward(custodianP, nil)},
		{name: "plain reader", retire: false, fixture: steward(plainReader, nil)},
		{name: "owning agent", retire: false, fixture: steward(agentA, nil)},
		{name: "home-project admin whose credential does not permit manage", retire: false,
			fixture: steward(custodianP, func(f *stewardFixture) { f.host.deny(custodianP, "project-1", PermissionManage) })},
		{name: "admin of another project", retire: false, fixture: steward(plainReader, func(f *stewardFixture) {
			f.host.allowAdmin(plainReader, "project-2")
		})},
	}
}

// TestCanRetireCapability: GET of the artifact and of a version states
// canRetire for the owner, an admin grantee, both stewards and a
// home-project admin, and for nobody else; it is omitted when false.
func TestCanRetireCapability(t *testing.T) {
	for _, tc := range canRetireCases() {
		t.Run(tc.name, func(t *testing.T) {
			f, id, who := tc.fixture(t)
			for _, target := range []string{artifactPath(id), artifactPath(id) + "/versions/1"} {
				rec := f.do(&who, http.MethodGet, target, nil, nil)
				if rec.Code != http.StatusOK {
					t.Fatalf("GET %s: %d %s", target, rec.Code, rec.Body.String())
				}
				if got := decodeInto[ArtifactResponse](t, rec).CanRetire; got != tc.retire {
					t.Errorf("GET %s canRetire = %v, want %v", target, got, tc.retire)
				}
				if !tc.retire && strings.Contains(rec.Body.String(), `"canRetire"`) {
					t.Errorf("GET %s: canRetire present when false: %s", target, rec.Body.String())
				}
			}
		})
	}
}

// TestCanRetireMatchesDeleteRoute: for every case, canRetire is exactly
// the decision DELETE /{id} makes for the same caller (204 when true, 403
// when false). It fails if the flag's check and the route's check diverge.
func TestCanRetireMatchesDeleteRoute(t *testing.T) {
	for _, tc := range canRetireCases() {
		t.Run(tc.name, func(t *testing.T) {
			f, id, who := tc.fixture(t)
			get := f.do(&who, http.MethodGet, artifactPath(id), nil, nil)
			if get.Code != http.StatusOK {
				t.Fatalf("GET: %d %s", get.Code, get.Body.String())
			}
			field := decodeInto[ArtifactResponse](t, get).CanRetire
			want := http.StatusForbidden
			if field {
				want = http.StatusNoContent
			}
			if rec := f.del(who, id); rec.Code != want {
				t.Errorf("canRetire = %v but DELETE answered %d %s", field, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestCanRetireUnreadableIsMissing: a home-project admin who cannot read
// the artifact gets the 404 of a missing artifact, with no canRetire.
func TestCanRetireUnreadableIsMissing(t *testing.T) {
	f := newStewardFixture(t)
	blind := principal{PrincipalKindUser, "user-blind-admin", ""}
	f.host.allowAdmin(blind, "project-1")
	rec := f.do(&blind, http.MethodGet, artifactPath(f.id), nil, nil)
	if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "canRetire") {
		t.Fatalf("GET by a non-reading admin: %d %s", rec.Code, rec.Body.String())
	}
}

// TestCanRetireIsAdviceDeleteDecidesFresh: canRetire is a snapshot taken
// at GET. When the authority behind it is withdrawn before the DELETE (an
// admin grant removed, a steward clause gone, the home-project admin's
// credential no longer permitting artifact.manage), the DELETE decides
// again and refuses with 403, deleting nothing. It fails if the route ever
// trusts the flag, cached or client-supplied, instead of deciding afresh.
func TestCanRetireIsAdviceDeleteDecidesFresh(t *testing.T) {
	for _, tc := range []struct {
		name   string
		who    principal
		revoke func(f *stewardFixture)
	}{
		{"admin grant removed", stAdminGrantee, func(f *stewardFixture) {
			f.exec(f.t, `DELETE FROM artifact_grant WHERE artifact_id = ? AND subject_ref = ?`,
				f.id, PrincipalRef(stAdminGrantee.kind, stAdminGrantee.ref))
		}},
		{"steward clause gone", ownerSteward, func(f *stewardFixture) {
			f.host.mu.Lock()
			defer f.host.mu.Unlock()
			delete(f.host.stewards, PrincipalRef(ownerSteward.kind, ownerSteward.ref)+" "+agentA.ref+" project-1")
		}},
		{"custodian credential narrowed", custodianP, func(f *stewardFixture) {
			f.host.deny(custodianP, "project-1", PermissionManage)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStewardFixture(t)
			get := f.do(&tc.who, http.MethodGet, artifactPath(f.id), nil, nil)
			if get.Code != http.StatusOK || !decodeInto[ArtifactResponse](t, get).CanRetire {
				t.Fatalf("GET before the change: %d %s; want canRetire true", get.Code, get.Body.String())
			}
			tc.revoke(f)
			if rec := f.del(tc.who, f.id); rec.Code != http.StatusForbidden {
				t.Fatalf("DELETE after the authority was withdrawn: %d %s; want 403", rec.Code, rec.Body.String())
			}
			if _, err := f.store.GetArtifact(context.Background(), f.id); err != nil {
				t.Fatalf("the refused delete deleted the artifact: %v", err)
			}
		})
	}
}
