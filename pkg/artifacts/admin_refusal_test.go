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
	"encoding/json"
	"net/http"
	"testing"
)

// TestAdminRefusalTextByCaller: a reader that may not administer an
// artifact gets the same 403 forbidden on every sharing route, with the
// agent text for an agent (its own artifact included) and the user text for
// a user; a caller that cannot read it gets a 404 byte-identical to a
// missing artifact's (ptone/scion#4576).
func TestAdminRefusalTextByCaller(t *testing.T) {
	f := newFixture(t, false)
	id := f.publish(agentA, "doc.md", []byte("# doc"), "").Artifact.ID
	const otherID = "00000000-0000-4000-8000-000000000001"
	routes := func(aid string) []struct{ method, path, body string } {
		base := "/api/v1/artifacts/" + aid
		return []struct{ method, path, body string }{
			{http.MethodGet, base + "/links", ""},
			{http.MethodPost, base + "/links", `{}`},
			{http.MethodDelete, base + "/links/" + otherID, ""},
			{http.MethodGet, base + "/grants", ""},
			{http.MethodPost, base + "/grants", `{"subjectKind": "principal", "subjectRef": "user:user-2", "permission": "review"}`},
			{http.MethodDelete, base + "/grants/" + otherID, ""},
			{http.MethodPatch, base, `{"expiresAt": null}`},
		}
	}
	jsonHdr := map[string]string{"Content-Type": "application/json"}
	for _, c := range []struct {
		name string
		p    principal
		want string
	}{
		{"owning agent", agentA, agentAdminForbiddenMessage},
		{"another agent of the project", agentB, agentAdminForbiddenMessage},
		{"user reader", userU, adminForbiddenMessage},
	} {
		for _, rt := range routes(id) {
			rec := f.do(&c.p, rt.method, rt.path, []byte(rt.body), jsonHdr)
			var e errorResponse
			_ = json.Unmarshal(rec.Body.Bytes(), &e)
			if rec.Code != http.StatusForbidden || e.Error.Code != "forbidden" || e.Error.Message != c.want {
				t.Errorf("%s: %s %s: %d %q %q, want 403 forbidden %q", c.name, rt.method, rt.path, rec.Code, e.Error.Code, e.Error.Message, c.want)
			}
		}
	}
	// A caller that cannot read the artifact, agent or user, learns nothing:
	// each route answers 404, exactly what it answers for an artifact that
	// does not exist.
	missing := routes("00000000-0000-4000-8000-0000000000ff")
	for _, p := range []principal{agentX, outside} {
		for i, rt := range routes(id) {
			rec := f.do(&p, rt.method, rt.path, []byte(rt.body), jsonHdr)
			got := response(rec)
			want := response(f.do(&p, missing[i].method, missing[i].path, []byte(missing[i].body), jsonHdr))
			if rec.Code != http.StatusNotFound || got != want {
				t.Errorf("unreadable as %s, %s %s:\n%s\nwant 404 as for a missing artifact:\n%s", p.ref, rt.method, rt.path, got, want)
			}
		}
	}
}
