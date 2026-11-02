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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// authorityRawObject decodes a single-agent response as raw JSON, so a test
// can assert on the keys the client receives rather than on struct fields.
func authorityRawObject(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

// authorityRawListItems decodes an agent list response's items as raw JSON.
func authorityRawListItems(t *testing.T, rec *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		Agents []map[string]any `json:"agents"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out.Agents
}

// TestAgentAuthority_DelegatedListOmitsAuthority: a hub agent list made
// with an agent delegated credential carries no "authority" key on any
// item, even with include=authority, while the same request on the
// authorizing user's session carries it on every item. The sorted form
// answers 400 before any list runs, and the project form is not admitted
// for a delegated credential.
func TestAgentAuthority_DelegatedListOmitsAuthority(t *testing.T) {
	w := newADTListWorld(t, "adt-auth-list")
	cred := w.delegated(t, w.adtIssueGrant(t, map[string]string{"kind": "hub"}, "agent:list", "agent:read").ID)
	q := url.Values{"include": {"authority"}}

	items := authorityRawListItems(t, w.listAgents(t, cred.Token, q))
	require.NotEmpty(t, items, "the delegated list returns agents")
	for _, it := range items {
		assert.NotContains(t, it, "authority", "delegated list item %v", it["id"])
	}

	// Control: the same request on Alice's session carries the key, so the
	// assertion above fails if the delegated list starts adding it.
	ctl := authorityRawListItems(t, w.listAgents(t, w.session(t, w.alice), q))
	require.NotEmpty(t, ctl)
	for _, it := range ctl {
		assert.Contains(t, it, "authority", "session list item %v", it["id"])
	}

	sorted := url.Values{"include": {"authority"}, "sort": {"created"}}
	adtAssertAPIError(t, w.listAgents(t, cred.Token, sorted), http.StatusBadRequest, errCodeDelegatedListSortUnsupported)

	rec := w.do(t, http.MethodGet, "/api/v1/projects/"+w.proj.ID+"/agents?include=authority", nil, adtBearer(cred.Token))
	adtAssertAPIError(t, rec, http.StatusForbidden, errCodeCredentialNotAdmitted)
}

// TestAgentAuthority_DelegatedGetOmitsAuthority: the single-agent GET made
// with an agent delegated credential carries no "authority" key, for an
// agent in another project and one in the caller's project, while the
// same GET on the authorizing user's session carries it.
func TestAgentAuthority_DelegatedGetOmitsAuthority(t *testing.T) {
	f := newADTFixture(t, "adt-auth-get")
	cred := f.delegated(t, f.hubGrant(t).ID)

	for _, id := range []string{f.agentB.ID, f.agentC.ID} {
		got := authorityRawObject(t, f.getAgent(t, cred.Token, id))
		assert.Equal(t, id, got["id"])
		assert.NotContains(t, got, "authority", "delegated GET of %s", id)
	}

	ctl := authorityRawObject(t, f.getAgent(t, f.session(t, f.alice), f.agentC.ID))
	assert.Contains(t, ctl, "authority", "the session GET carries the key")
}
