//go:build !no_sqlite

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
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
)

// starRingReplica builds a keyring-mode replica (newCapReplica) of the star
// fixture's deployment: the same database, chat store and attachment store,
// and the starred-messages experiment on.
func starRingReplica(t *testing.T, f *starFixture, k *keyring.Keyring, clk *krClock) *Server {
	t.Helper()
	r := newCapReplica(t, f.s, k, clk)
	mirrorHubResources(r, f.srv)
	setStarsExperiment(t, r, true, true)
	return r
}

// getStarPage lists userID's stars in key on srv as an interactive session.
func getStarPage(t *testing.T, f *starFixture, srv *Server, userID, key string, extra ...string) (*httptest.ResponseRecorder, starPageResponse) {
	t.Helper()
	user, err := f.s.GetUser(context.Background(), userID)
	require.NoError(t, err)
	rec := doRequestAsUser(t, srv, user, http.MethodGet, starListPath(key, extra...), nil)
	var page starPageResponse
	if rec.Code == http.StatusOK {
		page = decodeStarPage(t, rec)
	}
	return rec, page
}

// TestCapabilityKeyring_ChatStarsCursor: the chat-stars list continuation
// (handleStarList: listCursorSealer.Seal; openStarCursor:
// openAndValidateListCursor) in keyring mode. A cursor sealed on either
// replica of a rollout is in the c2.<kid>. form and resumes on the other.
// It stays bound to its caller and conversation. Unknown, swapped and
// stripped generations, a flipped byte and a retired generation get the
// uniform invalid_cursor. At sign_until a page that needs a continuation is
// refused with 500 and no cursor (errCapabilityIssuanceClosed, as on the
// other list endpoints), while a complete first page still works. With
// emit=legacy the replicas issue ID-less c1. cursors and still resume each
// other's, and a c1. cursor sealed by a legacy-mode replica (no keyring)
// resumes after the upgrade to a versioned ring. No key material reaches the
// logs or the body.
func TestCapabilityKeyring_ChatStarsCursor(t *testing.T) {
	allowVersionedRings(t)
	f := newStarFixture(t, true)
	var ids []string
	for i := 0; i < 3; i++ {
		id := f.threadMsg(t, f.topicA, f.projA, fmt.Sprintf("starred %d", i), time.Now().Add(-time.Duration(10-i)*time.Hour))
		decodeStarPut(t, f.put(t, f.admin, id, f.topicA))
		ids = append(ids, id)
		time.Sleep(time.Millisecond)
	}
	logs := captureLogs(t) // before the replicas: their loggers bind the default at New
	clk := &krClock{t: krT0}
	ka, kb := capRollout(t, "versioned")
	a := starRingReplica(t, f, ka, clk)
	b := starRingReplica(t, f, kb, clk)

	// Seal on A (gen1), open and validate on B; seal on B (gen2), open on A.
	rec, p1 := getStarPage(t, f, a, f.admin, f.topicA, "limit=1")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, p1.Items, 1)
	require.True(t, strings.HasPrefix(p1.NextCursor, "c2.gen1."), p1.NextCursor)
	rec, p2 := getStarPage(t, f, b, f.admin, f.topicA, "limit=1", "cursor="+url.QueryEscape(p1.NextCursor))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, p2.Items, 1)
	assert.NotEqual(t, p1.Items[0].MessageID, p2.Items[0].MessageID)
	require.True(t, strings.HasPrefix(p2.NextCursor, "c2.gen2."), p2.NextCursor)
	rec, p3 := getStarPage(t, f, a, f.admin, f.topicA, "limit=1", "cursor="+url.QueryEscape(p2.NextCursor))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, p3.Items, 1)
	assert.ElementsMatch(t, ids, []string{p1.Items[0].MessageID, p2.Items[0].MessageID, p3.Items[0].MessageID}, "each star once, across replicas")
	assert.Empty(t, p3.NextCursor)
	assert.NotContains(t, rec.Body.String(), `"nextCursor"`, "the last page carries no cursor key")

	// Bound to the caller and the conversation.
	rec, _ = getStarPage(t, f, b, f.owner, f.topicA, "limit=1", "cursor="+url.QueryEscape(p1.NextCursor))
	assertInvalidCursor(t, rec)
	rec, _ = getStarPage(t, f, b, f.admin, f.topicB, "limit=1", "cursor="+url.QueryEscape(p1.NextCursor))
	assertInvalidCursor(t, rec)

	body := strings.TrimPrefix(p1.NextCursor, "c2.gen1.")
	// A flipped byte inside the sealed body (not a base64 character swap,
	// which can decode to the same bytes).
	raw, err := base64.RawURLEncoding.DecodeString(body)
	require.NoError(t, err)
	require.NotEmpty(t, raw)
	raw[len(raw)/2] ^= 0x01
	flipped := "c2.gen1." + base64.RawURLEncoding.EncodeToString(raw)
	require.NotEqual(t, p1.NextCursor, flipped)
	for name, c := range map[string]string{
		"unknown kid":     "c2.gen9." + body,
		"swapped kid":     "c2.gen2." + body,
		"stripped to c1":  "c1." + body,
		"no kid":          "c2." + body,
		"empty kid":       "c2.." + body,
		"unknown version": "c3.gen1." + body,
		"flipped byte":    flipped,
	} {
		rec, _ := getStarPage(t, f, b, f.admin, f.topicA, "limit=1", "cursor="+url.QueryEscape(c))
		require.Equal(t, http.StatusBadRequest, rec.Code, name)
		assertInvalidCursor(t, rec)
	}

	// gen1 verifies on B until its verify_until, and not from that instant.
	clk.t = krT0.Add(48*time.Hour - time.Second)
	rec, _ = getStarPage(t, f, b, f.admin, f.topicA, "limit=1", "cursor="+url.QueryEscape(p1.NextCursor))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	clk.t = krT0.Add(48 * time.Hour)
	rec, _ = getStarPage(t, f, b, f.admin, f.topicA, "limit=1", "cursor="+url.QueryEscape(p1.NextCursor))
	assertInvalidCursor(t, rec)
	rec, _ = getStarPage(t, f, b, f.admin, f.topicA, "limit=1")
	assert.Equal(t, http.StatusOK, rec.Code, "a fresh first page always works")

	// emit=legacy: mid-rollout replicas that still issue ID-less (c1.)
	// cursors resume each other's in both directions, caller-bound.
	lclk := &krClock{t: krT0}
	la, lb := capRollout(t, "legacy")
	ra := starRingReplica(t, f, la, lclk)
	rb := starRingReplica(t, f, lb, lclk)
	for _, dir := range []struct {
		name     string
		from, to *Server
	}{{"legacy A to B", ra, rb}, {"legacy B to A", rb, ra}} {
		rec, q1 := getStarPage(t, f, dir.from, f.admin, f.topicA, "limit=1")
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", dir.name, rec.Body.String())
		require.True(t, strings.HasPrefix(q1.NextCursor, listCursorPrefix), "%s: %s", dir.name, q1.NextCursor)
		rec, q2 := getStarPage(t, f, dir.to, f.admin, f.topicA, "limit=1", "cursor="+url.QueryEscape(q1.NextCursor))
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", dir.name, rec.Body.String())
		require.Len(t, q2.Items, 1, dir.name)
		assert.NotEqual(t, q1.Items[0].MessageID, q2.Items[0].MessageID, dir.name)
		rec, _ = getStarPage(t, f, dir.to, f.owner, f.topicA, "limit=1", "cursor="+url.QueryEscape(q1.NextCursor))
		assertInvalidCursor(t, rec)
	}

	// Legacy-replica upgrade: a c1. cursor sealed by a legacy-mode replica
	// (no keyring) resumes on a versioned ring replica, whose own
	// continuation is versioned.
	uclk := &krClock{t: krT0}
	_, ub := capRollout(t, "versioned")
	legacy := starRingReplica(t, f, nil, uclk)
	upgraded := starRingReplica(t, f, ub, uclk)
	rec, l1 := getStarPage(t, f, legacy, f.admin, f.topicA, "limit=1")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.True(t, strings.HasPrefix(l1.NextCursor, listCursorPrefix), l1.NextCursor)
	rec, l2 := getStarPage(t, f, upgraded, f.admin, f.topicA, "limit=1", "cursor="+url.QueryEscape(l1.NextCursor))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, l2.Items, 1)
	assert.NotEqual(t, l1.Items[0].MessageID, l2.Items[0].MessageID)
	assert.True(t, strings.HasPrefix(l2.NextCursor, "c2.gen2."), l2.NextCursor)

	// Issuance closed: no cursor is minted, and the page is refused.
	cclk := &krClock{t: krT0.Add(time.Hour)}
	c := starRingReplica(t, f, capClosing(t), cclk)
	rec, _ = getStarPage(t, f, c, f.admin, f.topicA, "limit=1")
	assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "c2.")
	out := rec.Body.String()
	rec, all := getStarPage(t, f, c, f.admin, f.topicA, "limit=10")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Len(t, all.Items, 3)
	assert.Empty(t, all.NextCursor)
	assert.NotContains(t, rec.Body.String(), `"nextCursor"`, "a complete page carries no cursor key")
	assertNoCapabilityKeyMaterial(t, logs.String()+out+rec.Body.String())
}
