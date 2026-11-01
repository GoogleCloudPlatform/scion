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

	// Issuance closed: no cursor is minted, and the page is refused. An
	// existing session, with capability issuance closed: the user JWT is
	// signed by the same ring, so it is minted just before sign_until (T0+1h)
	// on the ring clock and stays valid at T0+1h (15 min on the same clock).
	cclk := &krClock{t: krT0.Add(59 * time.Minute)}
	c := starRingReplica(t, f, capClosing(t), cclk)
	admin, err := f.s.GetUser(context.Background(), f.admin)
	require.NoError(t, err)
	session := keyringSessionToken(t, c, admin)
	cclk.t = krT0.Add(time.Hour)
	rec = requestWithToken(t, c, session, http.MethodGet, starListPath(f.topicA, "limit=1"), nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "c2.")
	out := rec.Body.String()
	rec = requestWithToken(t, c, session, http.MethodGet, starListPath(f.topicA, "limit=10"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	all := decodeStarPage(t, rec)
	assert.Len(t, all.Items, 3)
	assert.Empty(t, all.NextCursor)
	assert.NotContains(t, rec.Body.String(), `"nextCursor"`, "a complete page carries no cursor key")
	assertNoCapabilityKeyMaterial(t, logs.String()+out+rec.Body.String())
}

// getAsUser sends a GET for path to srv as userID's interactive session.
func getAsUser(t *testing.T, f *starFixture, srv *Server, userID, path string) *httptest.ResponseRecorder {
	t.Helper()
	user, err := f.s.GetUser(context.Background(), userID)
	require.NoError(t, err)
	return doRequestAsUser(t, srv, user, http.MethodGet, path, nil)
}

// TestCapabilityKeyring_StarredConversationsCursor: the Starred rail
// continuation (GET /api/v1/chat/starred-conversations;
// sealStarredConversationsCursor: SealPosition, openStarredConversationsCursor:
// OpenPosition) end to end in keyring mode. Pages alternate between the two
// replicas of a rollout, each continuation in the p2.<kid>. form of the
// replica that sealed it, and list every readable conversation once. A
// cursor stays bound to its caller and sort; a swapped generation and a
// retired one get the uniform invalid_cursor. At sign_until a page that
// needs a continuation is refused with 500 and no cursor, while a complete
// first page still works. No key material reaches the logs or the body.
func TestCapabilityKeyring_StarredConversationsCursor(t *testing.T) {
	allowVersionedRings(t)
	f := newStarredRailFixture(t, true)
	want := starredConversationRowKeys(f.rail(t, f.member).Items)
	require.Len(t, want, 4)
	logs := captureLogs(t) // before the replicas: their loggers bind the default at New
	clk := &krClock{t: krT0}
	ka, kb := capRollout(t, "versioned")
	a := starRingReplica(t, f.starFixture, ka, clk)
	b := starRingReplica(t, f.starFixture, kb, clk)
	railPage := func(srv *Server, userID string, params ...string) *httptest.ResponseRecorder {
		return getAsUser(t, f.starFixture, srv, userID, starredConversationsURL(params...))
	}

	// Page 1 on A (gen1), page 2 on B (gen2), and so on, alternating.
	replicas := []*Server{a, b}
	prefixes := []string{listPositionPrefixV2 + "gen1.", listPositionPrefixV2 + "gen2."}
	var got []string
	var first string
	cursor := ""
	for i := 0; ; i++ {
		require.Less(t, i, len(want)+1, "pagination ends")
		params := []string{"limit", "1"}
		if cursor != "" {
			params = append(params, "cursor", cursor)
		}
		page := decodeStarredConversations(t, railPage(replicas[i%2], f.member, params...))
		got = append(got, starredConversationRowKeys(page.Items)...)
		if page.NextCursor == "" {
			break
		}
		require.True(t, strings.HasPrefix(page.NextCursor, prefixes[i%2]), "page %d: %s", i, page.NextCursor)
		if first == "" {
			first = page.NextCursor
		}
		cursor = page.NextCursor
	}
	assert.Equal(t, want, got, "each conversation once and in order, across replicas")
	assert.Len(t, got, len(want))

	// Bound to the caller and the sort.
	assertInvalidCursor(t, railPage(b, f.owner, "limit", "1", "cursor", first))
	assertInvalidCursor(t, railPage(b, f.member, "limit", "1", "sort", "alpha", "cursor", first))
	body := strings.TrimPrefix(first, listPositionPrefixV2+"gen1.")
	assertInvalidCursor(t, railPage(b, f.member, "limit", "1", "cursor", listPositionPrefixV2+"gen2."+body))
	assertInvalidCursor(t, railPage(b, f.member, "limit", "1", "cursor", listCursorPrefixV2+"gen1."+body))

	// gen1 verifies on B until its verify_until, and not from that instant.
	clk.t = krT0.Add(48*time.Hour - time.Second)
	require.Equal(t, http.StatusOK, railPage(b, f.member, "limit", "1", "cursor", first).Code)
	clk.t = krT0.Add(48 * time.Hour)
	assertInvalidCursor(t, railPage(b, f.member, "limit", "1", "cursor", first))
	assert.Equal(t, http.StatusOK, railPage(b, f.member, "limit", "1").Code, "a fresh first page always works")

	// Issuance closed: the session is minted just before sign_until (T0+1h)
	// on the ring clock and stays valid at T0+1h.
	cclk := &krClock{t: krT0.Add(59 * time.Minute)}
	c := starRingReplica(t, f.starFixture, capClosing(t), cclk)
	member, err := f.s.GetUser(context.Background(), f.member)
	require.NoError(t, err)
	session := keyringSessionToken(t, c, member)
	cclk.t = krT0.Add(time.Hour)
	rec := requestWithToken(t, c, session, http.MethodGet, starredConversationsURL("limit", "1"), nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), listPositionPrefixV2)
	out := rec.Body.String()
	rec = requestWithToken(t, c, session, http.MethodGet, starredConversationsURL("limit", "10"), nil)
	all := decodeStarredConversations(t, rec)
	assert.Equal(t, want, starredConversationRowKeys(all.Items))
	assert.NotContains(t, rec.Body.String(), `"nextCursor"`, "a complete page carries no cursor key")
	assertNoCapabilityKeyMaterial(t, logs.String()+out+rec.Body.String())
}

// TestCapabilityKeyring_StarredHistoryCursorRotation: starred-only history
// (handleStarredHistory: listCursorSealer.Seal; openStarredHistoryCursor)
// paged across a key rotation. A walk that alternates between the replica
// still on gen1 and the one that rotated to gen2 returns exactly the
// single-replica order; a gen1 continuation keeps working on the rotated
// replica until gen1's verify_until and then fails with invalid_cursor; and
// a c1. continuation from a legacy-mode replica resumes on the upgraded
// ring with a versioned continuation.
func TestCapabilityKeyring_StarredHistoryCursorRotation(t *testing.T) {
	allowVersionedRings(t)
	f := newStarFixture(t, true)
	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 4; i++ {
		id := f.threadMsg(t, f.topicA, f.projA, fmt.Sprintf("m%d", i), base.Add(time.Duration(i)*time.Minute))
		f.starAs(t, f.admin, id, f.topicA)
	}
	want := historyIDs(decodeHistory(t, f.session(t, f.admin, http.MethodGet, starredHistoryPath(f.topicA), nil)))
	require.Len(t, want, 4)
	clk := &krClock{t: krT0}
	ka, kb := capRollout(t, "versioned")
	a := starRingReplica(t, f, ka, clk)
	b := starRingReplica(t, f, kb, clk)
	page := func(srv *Server, cursor string) chatHistoryResponse {
		t.Helper()
		extra := []string{"limit=1"}
		if cursor != "" {
			extra = append(extra, "cursor="+url.QueryEscape(cursor))
		}
		return decodeHistory(t, getAsUser(t, f, srv, f.admin, starredHistoryPath(f.topicA, extra...)))
	}

	replicas := []*Server{a, b}
	prefixes := []string{listCursorPrefixV2 + "gen1.", listCursorPrefixV2 + "gen2."}
	var got []string
	var gen1Cursor string
	cursor := ""
	for i := 0; ; i++ {
		require.Less(t, i, len(want)+1, "pagination ends")
		p := page(replicas[i%2], cursor)
		got = append(got, historyIDs(p)...)
		if p.NextCursor == "" {
			break
		}
		require.True(t, strings.HasPrefix(p.NextCursor, prefixes[i%2]), "page %d: %s", i, p.NextCursor)
		if gen1Cursor == "" {
			gen1Cursor = p.NextCursor
		}
		cursor = p.NextCursor
	}
	assert.Equal(t, want, got, "the same order as one replica, each message once")

	// The rotated replica resumes a gen1 continuation until gen1 retires.
	clk.t = krT0.Add(48*time.Hour - time.Second)
	assert.Equal(t, want[1:2], historyIDs(page(b, gen1Cursor)))
	clk.t = krT0.Add(48 * time.Hour)
	assertInvalidCursor(t, getAsUser(t, f, b, f.admin, starredHistoryPath(f.topicA, "limit=1", "cursor="+url.QueryEscape(gen1Cursor))))
	assert.Equal(t, want[:1], historyIDs(page(b, "")), "a fresh first page always works")

	// Upgrade from a legacy-mode replica: its c1. continuation resumes on
	// the versioned ring, whose own continuation is versioned.
	uclk := &krClock{t: krT0}
	_, ub := capRollout(t, "versioned")
	legacy := starRingReplica(t, f, nil, uclk)
	upgraded := starRingReplica(t, f, ub, uclk)
	l1 := page(legacy, "")
	require.True(t, strings.HasPrefix(l1.NextCursor, listCursorPrefix), l1.NextCursor)
	l2 := page(upgraded, l1.NextCursor)
	assert.Equal(t, want[1:2], historyIDs(l2))
	assert.True(t, strings.HasPrefix(l2.NextCursor, listCursorPrefixV2+"gen2."), l2.NextCursor)
}

// TestCapabilityKeyring_ChatStarsCursorLegacyReplicaRefusesVersioned: the
// reverse of TestCapabilityKeyring_ChatStarsCursor's legacy-replica upgrade
// case. A chat-stars cursor sealed by a versioned ring replica (c2.<kid>.)
// is refused with invalid_cursor by a replica of the same deployment still
// in legacy mode (no keyring), which reads only the ID-less c1. form and
// holds no generation key. The same cursor resumes on the ring replica, so
// the refusal comes from the legacy replica, not from a broken cursor, and
// a fresh first page on the legacy replica still works.
func TestCapabilityKeyring_ChatStarsCursorLegacyReplicaRefusesVersioned(t *testing.T) {
	allowVersionedRings(t)
	f := newStarFixture(t, true)
	for i := 0; i < 3; i++ {
		id := f.threadMsg(t, f.topicA, f.projA, fmt.Sprintf("starred %d", i), time.Now().Add(-time.Duration(10-i)*time.Hour))
		decodeStarPut(t, f.put(t, f.admin, id, f.topicA))
		time.Sleep(time.Millisecond)
	}
	clk := &krClock{t: krT0}
	_, ub := capRollout(t, "versioned")
	legacy := starRingReplica(t, f, nil, clk)
	upgraded := starRingReplica(t, f, ub, clk)

	rec, v1 := getStarPage(t, f, upgraded, f.admin, f.topicA, "limit=1")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, v1.Items, 1)
	require.True(t, strings.HasPrefix(v1.NextCursor, listCursorPrefixV2+"gen2."), v1.NextCursor)

	// Control: the cursor resumes on the ring replica that sealed it.
	rec, v2 := getStarPage(t, f, upgraded, f.admin, f.topicA, "limit=1", "cursor="+url.QueryEscape(v1.NextCursor))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, v2.Items, 1)
	assert.NotEqual(t, v1.Items[0].MessageID, v2.Items[0].MessageID)

	// The legacy-mode replica fails closed on it.
	rec, _ = getStarPage(t, f, legacy, f.admin, f.topicA, "limit=1", "cursor="+url.QueryEscape(v1.NextCursor))
	assertInvalidCursor(t, rec)
	assert.NotContains(t, rec.Body.String(), `"items"`, "a refused cursor returns no page")

	rec, l1 := getStarPage(t, f, legacy, f.admin, f.topicA, "limit=1")
	require.Equal(t, http.StatusOK, rec.Code, "a fresh first page always works: %s", rec.Body.String())
	require.Len(t, l1.Items, 1)
	assert.True(t, strings.HasPrefix(l1.NextCursor, listCursorPrefix), l1.NextCursor)
}
