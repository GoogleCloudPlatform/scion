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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Phase 3 of starred messages: the starred=true variant of conversation
// history (the starred-only center).

func starredHistoryPath(key string, extra ...string) string {
	p := chatConversationPath(key, "messages") + "?starred=true"
	for _, e := range extra {
		p += "&" + e
	}
	return p
}

func decodeHistory(t *testing.T, rec *httptest.ResponseRecorder) chatHistoryResponse {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp chatHistoryResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

func historyIDs(resp chatHistoryResponse) []string {
	ids := []string{}
	for _, m := range resp.Messages {
		ids = append(ids, m.ID)
	}
	return ids
}

// starAs stars msg for userID through the API.
func (f *starFixture) starAs(t *testing.T, userID, msg, key string) {
	t.Helper()
	decodeStarPut(t, f.put(t, userID, msg, key))
}

// newerID returns the two IDs of a timestamp tie in history order (the
// greater ID first).
func newerID(a, b string) (string, string) {
	if a > b {
		return a, b
	}
	return b, a
}

func TestChatStarredHistory_GateAndValidation(t *testing.T) {
	f := newStarFixture(t, true)
	msg := f.threadMsg(t, f.topicA, f.projA, "starred", time.Now().Add(-time.Minute))
	plain := f.threadMsg(t, f.topicA, f.projA, "plain", time.Now())
	f.starAs(t, f.admin, msg, f.topicA)

	// Off: the filter is a 404 whatever its value; it never falls back to
	// unfiltered history.
	setStarsExperiment(t, f.srv, false, true)
	for _, p := range []string{
		starredHistoryPath(f.topicA),
		chatConversationPath(f.topicA, "messages") + "?starred=false",
		chatConversationPath(f.topicA, "messages") + "?starred=",
		// The semicolon-separated form is the starred variant too.
		chatConversationPath(f.topicA, "messages") + "?x=1;starred=true",
		chatConversationPath(f.topicA, "messages") + "?starred=true;x=1",
	} {
		rec := f.session(t, f.admin, http.MethodGet, p, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, p)
		assert.NotContains(t, rec.Body.String(), plain, p)
		assert.NotContains(t, rec.Body.String(), "messages\"", p)
	}
	// Ordinary history is unaffected.
	assert.Equal(t, []string{plain, msg}, historyIDs(decodeHistory(t, f.session(t, f.admin, http.MethodGet, chatConversationPath(f.topicA, "messages"), nil))))
	setStarsExperiment(t, f.srv, true, true)
	assert.Equal(t, []string{plain, msg}, historyIDs(decodeHistory(t, f.session(t, f.admin, http.MethodGet, chatConversationPath(f.topicA, "messages"), nil))))

	base := chatConversationPath(f.topicA, "messages")
	for _, bad := range []string{
		base + "?starred=false",
		base + "?starred=1",
		base + "?starred=TRUE",
		base + "?starred=",
		base + "?starred",
		// A value URL.Query cannot decode is still the starred variant.
		base + "?starred=%zz",
		base + "?x=1&starred=%",
		base + "?x=1;starred=true",
		starredHistoryPath(f.topicA, "starred=true"),
		starredHistoryPath(f.topicA, "unknown=1"),
		starredHistoryPath(f.topicA, "limit=0"),
		starredHistoryPath(f.topicA, "limit=201"),
		starredHistoryPath(f.topicA, "limit=x"),
		starredHistoryPath(f.topicA, "limit=1", "limit=2"),
		starredHistoryPath(f.topicA, "around=nope"),
		starredHistoryPath(f.topicA, "around="+msg, "cursor=abc"),
		starredHistoryPath(f.topicA, "cursor=abc"),
		starredHistoryPath("dm:user:a:user:b"),
	} {
		rec := f.session(t, f.admin, http.MethodGet, bad, nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", bad, rec.Body.String())
	}
	assert.Equal(t, http.StatusOK, f.session(t, f.admin, http.MethodGet, starredHistoryPath(f.topicA, "limit=200"), nil).Code)

	// Missing and unreadable threads share the 404.
	assert.Equal(t, http.StatusNotFound, f.session(t, f.member, http.MethodGet, starredHistoryPath(f.topicB), nil).Code)
	assert.Equal(t, http.StatusNotFound, f.session(t, f.admin, http.MethodGet, starredHistoryPath(uuid.NewString()), nil).Code)

	// No capable chat store: 503, not unfiltered history.
	f.srv.SetWebChatStore(starlessWebChatStore{f.wcs})
	assert.Equal(t, http.StatusServiceUnavailable, f.session(t, f.admin, http.MethodGet, starredHistoryPath(f.topicA), nil).Code)
	f.srv.SetWebChatStore(f.wcs)
}

func TestChatStarredHistory_ListsOnlyStarred(t *testing.T) {
	for _, readSwitch := range []bool{true, false} {
		t.Run(fmt.Sprintf("readSwitch=%t", readSwitch), func(t *testing.T) {
			f := newStarFixture(t, readSwitch)
			ctx := context.Background()
			base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
			at := func(m int) time.Time { return base.Add(time.Duration(m) * time.Minute) }
			parent := f.threadMsg(t, f.topicA, f.projA, "the unstarred parent", at(1))
			s1 := f.threadMsg(t, f.topicA, f.projA, "first starred", at(2))
			gap := f.threadMsg(t, f.topicA, f.projA, "an omitted message", at(3))
			s2 := f.threadMsg(t, f.topicA, f.projA, "tied a", at(4))
			s3 := f.threadMsg(t, f.topicA, f.projA, "tied b", at(4))
			otherUsers := f.threadMsg(t, f.topicA, f.projA, "the owner's star", at(5))
			telegram := f.threadMsgOn(t, f.topicA, f.projA, "telegram", "telegram", at(6))
			for _, id := range []string{s1, s2, s3} {
				f.starAs(t, f.admin, id, f.topicA)
			}
			f.starAs(t, f.owner, otherUsers, f.topicA)
			_, err := f.db.Exec(`INSERT INTO webchat_message_star (user_id, message_id, conversation_key, project_id, starred_at) VALUES (?, ?, ?, ?, ?)`,
				f.admin, telegram, f.topicA, f.projA, sqliteStarTime(time.Now()))
			require.NoError(t, err)

			// s3 replies to the unstarred parent and carries an attachment.
			require.NoError(t, f.wcs.SetMessageReplyTo(ctx, s3, parent))
			att := uuid.NewString()
			require.NoError(t, f.wcs.CreateAttachment(ctx, AttachmentMeta{ID: att, ProjectID: f.projA, Filename: "plan.txt",
				MimeType: "text/plain", Size: 4, UploadedBy: f.admin, CreatedAt: time.Now()}))
			require.NoError(t, f.wcs.LinkAttachmentToMessage(ctx, s3, att))
			require.NoError(t, f.wcs.SetMessageEdited(ctx, s1, time.Now()))

			tieNew, tieOld := newerID(s2, s3)
			resp := decodeHistory(t, f.session(t, f.admin, http.MethodGet, starredHistoryPath(f.topicA), nil))
			assert.Equal(t, []string{tieNew, tieOld, s1}, historyIDs(resp), "history order, ties by ID, starred web messages only")
			assert.Equal(t, 3, resp.TotalCount, "the live starred messages of the conversation")
			assert.Nil(t, resp.CanPost, "the filtered center has no composer")
			assert.Empty(t, resp.NextCursor)
			for _, m := range resp.Messages {
				assert.NotEqual(t, gap, m.ID)
				assert.NotEmpty(t, m.Msg, "full bodies, not excerpts")
			}
			assert.Equal(t, "first starred", resp.Messages[2].Msg)

			// The shared enrichment: the reply preview of an unstarred parent,
			// the attachment and the edit marker, as ordinary history has them.
			normal := decodeHistory(t, f.session(t, f.admin, http.MethodGet, chatConversationPath(f.topicA, "messages"), nil))
			require.Contains(t, resp.ReplyPreviews, parent)
			assert.Equal(t, normal.ReplyPreviews[parent], resp.ReplyPreviews[parent])
			assert.Equal(t, "the unstarred parent", resp.ReplyPreviews[parent].Content)
			require.Len(t, resp.MessageAttachments[s3], 1)
			assert.Equal(t, normal.MessageAttachments[s3], resp.MessageAttachments[s3])
			require.Contains(t, resp.MessageExtensions, s1)
			assert.NotNil(t, resp.MessageExtensions[s1].EditedAt)
			assert.Equal(t, parent, resp.MessageExtensions[s3].ReplyToID)

			// Pages of one, through the tie, with sealed cursors.
			var got []string
			cursor := ""
			for i := 0; i < 5; i++ {
				p := starredHistoryPath(f.topicA, "limit=1")
				if cursor != "" {
					p += "&cursor=" + url.QueryEscape(cursor)
				}
				page := decodeHistory(t, f.session(t, f.admin, http.MethodGet, p, nil))
				assert.Equal(t, 3, page.TotalCount)
				got = append(got, historyIDs(page)...)
				if page.NextCursor == "" {
					break
				}
				assert.True(t, strings.HasPrefix(page.NextCursor, listCursorPrefix), "sealed cursor")
				cursor = page.NextCursor
			}
			assert.Equal(t, []string{tieNew, tieOld, s1}, got)

			// The other user sees only their own star.
			owner := decodeHistory(t, f.session(t, f.owner, http.MethodGet, starredHistoryPath(f.topicA), nil))
			assert.Equal(t, []string{otherUsers}, historyIDs(owner))
			assert.Equal(t, 1, owner.TotalCount)

			// A deleted message and an unstarred one leave the view.
			require.NoError(t, f.wcs.SetMessageDeleted(ctx, s1, time.Now()))
			assert.Equal(t, http.StatusNoContent, f.session(t, f.admin, http.MethodDelete, starPath(tieNew), nil).Code)
			resp = decodeHistory(t, f.session(t, f.admin, http.MethodGet, starredHistoryPath(f.topicA), nil))
			assert.Equal(t, []string{tieOld}, historyIDs(resp))
			assert.Equal(t, 1, resp.TotalCount)

			// No stars left: an empty page.
			assert.Equal(t, http.StatusNoContent, f.session(t, f.admin, http.MethodDelete, starPath(tieOld), nil).Code)
			resp = decodeHistory(t, f.session(t, f.admin, http.MethodGet, starredHistoryPath(f.topicA), nil))
			assert.NotNil(t, resp.Messages)
			assert.Empty(t, resp.Messages)
			assert.Zero(t, resp.TotalCount)
		})
	}
}

func TestChatStarredHistory_CursorBinding(t *testing.T) {
	f := newStarFixture(t, true)
	base := time.Now().UTC().Add(-time.Hour)
	var ids []string
	for i := 0; i < 3; i++ {
		id := f.threadMsg(t, f.topicA, f.projA, fmt.Sprintf("m%d", i), base.Add(time.Duration(i)*time.Minute))
		f.starAs(t, f.admin, id, f.topicA)
		f.starAs(t, f.owner, id, f.topicA)
		ids = append(ids, id)
	}
	bMsg := f.threadMsg(t, f.topicB, f.projB, "b", base)
	f.starAs(t, f.admin, bMsg, f.topicB)

	first := decodeHistory(t, f.session(t, f.admin, http.MethodGet, starredHistoryPath(f.topicA, "limit=1"), nil))
	require.NotEmpty(t, first.NextCursor)
	c := url.QueryEscape(first.NextCursor)
	normal := decodeHistory(t, f.session(t, f.admin, http.MethodGet, chatConversationPath(f.topicA, "messages")+"?limit=1", nil))
	require.NotEmpty(t, normal.NextCursor)
	nc := url.QueryEscape(normal.NextCursor)

	// The page size may change between pages.
	next := decodeHistory(t, f.session(t, f.admin, http.MethodGet, starredHistoryPath(f.topicA, "limit=5", "cursor="+c), nil))
	assert.Equal(t, []string{ids[1], ids[0]}, historyIDs(next))

	for _, c := range []struct {
		name, user, path string
	}{
		{"another caller", f.owner, starredHistoryPath(f.topicA, "cursor="+c)},
		{"another conversation", f.admin, starredHistoryPath(f.topicB, "cursor="+c)},
		{"an ordinary history cursor", f.admin, starredHistoryPath(f.topicA, "cursor="+nc)},
		{"a tampered cursor", f.admin, starredHistoryPath(f.topicA, "cursor="+url.QueryEscape(first.NextCursor[:len(first.NextCursor)-2]+"AA"))},
	} {
		rec := f.session(t, c.user, http.MethodGet, c.path, nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", c.name, rec.Body.String())
	}
	// A filtered cursor does not continue ordinary history either.
	rec := f.session(t, f.admin, http.MethodGet, chatConversationPath(f.topicA, "messages")+"?cursor="+c, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	// A token caller's cursor is bound to the token, not just the user.
	reader := f.mint(t, f.admin, hubBoundary(), "inbox:read", "project:read")
	assert.Equal(t, http.StatusBadRequest, f.call(t, reader, http.MethodGet, starredHistoryPath(f.topicA, "cursor="+c), nil).Code)
}

func TestChatStarredHistory_Around(t *testing.T) {
	for _, readSwitch := range []bool{true, false} {
		t.Run(fmt.Sprintf("readSwitch=%t", readSwitch), func(t *testing.T) {
			f := newStarFixture(t, readSwitch)
			ctx := context.Background()
			base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
			var starred, unstarred []string
			for i := 0; i < 7; i++ {
				s := f.threadMsg(t, f.topicA, f.projA, fmt.Sprintf("s%d", i), base.Add(time.Duration(2*i)*time.Minute))
				u := f.threadMsg(t, f.topicA, f.projA, fmt.Sprintf("u%d", i), base.Add(time.Duration(2*i+1)*time.Minute))
				f.starAs(t, f.admin, s, f.topicA)
				starred = append(starred, s)
				unstarred = append(unstarred, u)
			}
			// limit 5 around s3: two older, the anchor, two newer, newest first.
			resp := decodeHistory(t, f.session(t, f.admin, http.MethodGet, starredHistoryPath(f.topicA, "around="+starred[3], "limit=5"), nil))
			assert.Equal(t, []string{starred[5], starred[4], starred[3], starred[2], starred[1]}, historyIDs(resp))
			assert.Equal(t, 7, resp.TotalCount)
			require.NotEmpty(t, resp.NextCursor, "older starred messages remain")
			older := decodeHistory(t, f.session(t, f.admin, http.MethodGet, starredHistoryPath(f.topicA, "cursor="+url.QueryEscape(resp.NextCursor)), nil))
			assert.Equal(t, []string{starred[0]}, historyIDs(older))

			// A page of one is the anchor, resuming past it.
			one := decodeHistory(t, f.session(t, f.admin, http.MethodGet, starredHistoryPath(f.topicA, "around="+starred[3], "limit=1"), nil))
			assert.Equal(t, []string{starred[3]}, historyIDs(one))
			require.NotEmpty(t, one.NextCursor)
			rest := decodeHistory(t, f.session(t, f.admin, http.MethodGet, starredHistoryPath(f.topicA, "cursor="+url.QueryEscape(one.NextCursor)), nil))
			assert.Equal(t, []string{starred[2], starred[1], starred[0]}, historyIDs(rest))
			// The oldest anchor has nothing older.
			oldest := decodeHistory(t, f.session(t, f.admin, http.MethodGet, starredHistoryPath(f.topicA, "around="+starred[0], "limit=1"), nil))
			assert.Empty(t, oldest.NextCursor)

			// The anchor must itself be one of the caller's live starred
			// messages of this conversation.
			bMsg := f.threadMsg(t, f.topicB, f.projB, "b", base)
			f.starAs(t, f.admin, bMsg, f.topicB)
			f.starAs(t, f.owner, unstarred[2], f.topicA)
			for _, id := range []string{unstarred[3], unstarred[2], bMsg, uuid.NewString()} {
				rec := f.session(t, f.admin, http.MethodGet, starredHistoryPath(f.topicA, "around="+id), nil)
				assert.Equal(t, http.StatusNotFound, rec.Code, id)
				assert.NotContains(t, rec.Body.String(), "u2", id)
			}
			require.NoError(t, f.wcs.SetMessageDeleted(ctx, starred[6], time.Now()))
			assert.Equal(t, http.StatusNotFound, f.session(t, f.admin, http.MethodGet, starredHistoryPath(f.topicA, "around="+starred[6]), nil).Code)
			// Ordinary history still opens the unstarred message in context.
			normal := decodeHistory(t, f.session(t, f.admin, http.MethodGet, chatConversationPath(f.topicA, "messages")+"?around="+unstarred[3], nil))
			assert.Contains(t, historyIDs(normal), unstarred[3])
		})
	}
}

// TestChatStarredHistory_AroundKeepsTiedStars: a starred message sharing
// the anchor's instant is in the around page (by its ID), and paging older
// from the page loses no starred message.
func TestChatStarredHistory_AroundKeepsTiedStars(t *testing.T) {
	for _, readSwitch := range []bool{true, false} {
		t.Run(fmt.Sprintf("readSwitch=%t", readSwitch), func(t *testing.T) {
			f := newStarFixture(t, readSwitch)
			base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
			oldest := f.threadMsg(t, f.topicA, f.projA, "oldest", base)
			a := f.threadMsg(t, f.topicA, f.projA, "tie a", base.Add(time.Minute))
			b := f.threadMsg(t, f.topicA, f.projA, "tie b", base.Add(time.Minute))
			newest := f.threadMsg(t, f.topicA, f.projA, "newest", base.Add(2*time.Minute))
			for _, id := range []string{oldest, a, b, newest} {
				f.starAs(t, f.admin, id, f.topicA)
			}
			tieNew, tieOld := newerID(a, b)
			order := []string{newest, tieNew, tieOld, oldest}

			for _, anchor := range []string{a, b} {
				page := decodeHistory(t, f.session(t, f.admin, http.MethodGet, starredHistoryPath(f.topicA, "around="+anchor, "limit=5"), nil))
				assert.Equal(t, order, historyIDs(page), "around %s keeps its tied sibling", anchor)

				// Smallest page: the anchor, then everything older through the
				// cursor, with nothing lost or repeated.
				got := []string{}
				one := decodeHistory(t, f.session(t, f.admin, http.MethodGet, starredHistoryPath(f.topicA, "around="+anchor, "limit=1"), nil))
				got = append(got, historyIDs(one)...)
				cursor := one.NextCursor
				for i := 0; cursor != "" && i < 5; i++ {
					p := decodeHistory(t, f.session(t, f.admin, http.MethodGet, starredHistoryPath(f.topicA, "limit=1", "cursor="+url.QueryEscape(cursor)), nil))
					got = append(got, historyIDs(p)...)
					cursor = p.NextCursor
				}
				at := 0
				for i, id := range order {
					if id == anchor {
						at = i
					}
				}
				assert.Equal(t, order[at:], got, "anchor %s then every older starred message", anchor)
			}
		})
	}
}

func TestChatStarredHistory_DMs(t *testing.T) {
	for _, readSwitch := range []bool{true, false} {
		t.Run(fmt.Sprintf("readSwitch=%t", readSwitch), func(t *testing.T) {
			f := newStarFixture(t, readSwitch)
			d := f.memberDMs(t)
			now := time.Now()
			agentStar := f.dmMsg(t, d.agentKey, d.agentConv, f.projA, "agent:"+f.agentA.Slug, f.agentA.ID, "agent starred", "web", now.Add(-time.Hour))
			agentPlain := f.dmMsg(t, d.agentKey, d.agentConv, f.projA, "agent:"+f.agentA.Slug, f.agentA.ID, "agent plain", "web", now.Add(-50*time.Minute))
			humanStar := f.dmMsg(t, d.humanKey, d.humanConv, uuid.Nil.String(), "user:"+f.owner+"@test.com", f.owner, "human starred", "web", now.Add(-time.Minute))
			humanPlain := f.dmMsg(t, d.humanKey, d.humanConv, uuid.Nil.String(), "user:"+f.owner+"@test.com", f.owner, "human plain", "web", now)
			f.starAs(t, f.member, agentStar, d.agentKey)
			f.starAs(t, f.member, humanStar, d.humanKey)

			assert.Equal(t, []string{agentStar}, historyIDs(decodeHistory(t, f.session(t, f.member, http.MethodGet, starredHistoryPath(d.agentKey), nil))))
			human := decodeHistory(t, f.session(t, f.member, http.MethodGet, starredHistoryPath(d.humanKey), nil))
			assert.Equal(t, []string{humanStar}, historyIDs(human))
			assert.Nil(t, human.CanPost)
			_ = agentPlain
			_ = humanPlain

			// The other participant has no stars there; a non-participant
			// gets the shared 404.
			assert.Empty(t, decodeHistory(t, f.session(t, f.owner, http.MethodGet, starredHistoryPath(d.humanKey), nil)).Messages)
			assert.Equal(t, http.StatusNotFound, f.session(t, f.admin, http.MethodGet, starredHistoryPath(d.humanKey), nil).Code)

			// A never-used DM has no starred messages, and no around target.
			unused := f.dm(t, f.member, "agent", f.agentB.ID)
			assert.Empty(t, decodeHistory(t, f.session(t, f.member, http.MethodGet, starredHistoryPath(unused), nil)).Messages)
			assert.Equal(t, http.StatusNotFound,
				f.session(t, f.member, http.MethodGet, starredHistoryPath(unused, "around="+uuid.NewString()), nil).Code)
		})
	}
}

func TestChatStarredHistory_TokenSelectors(t *testing.T) {
	f := newStarFixture(t, true)
	msg := f.threadMsg(t, f.topicA, f.projA, "token", time.Now())
	f.starAs(t, f.admin, msg, f.topicA)

	reader := f.mint(t, f.admin, hubBoundary(), "inbox:read", "project:read")
	assert.Equal(t, []string{msg}, historyIDs(decodeHistory(t, f.call(t, reader, http.MethodGet, starredHistoryPath(f.topicA), nil))))

	// Ordinary topic history needs no inbox:read; the filtered variant does.
	projectReadOnly := f.mint(t, f.admin, hubBoundary(), "project:read")
	assert.Equal(t, http.StatusOK, f.call(t, projectReadOnly, http.MethodGet, chatConversationPath(f.topicA, "messages"), nil).Code)
	assert.Equal(t, http.StatusForbidden, f.call(t, projectReadOnly, http.MethodGet, starredHistoryPath(f.topicA), nil).Code)
	writeOnly := f.mint(t, f.admin, hubBoundary(), "inbox:write", "project:read")
	assert.Equal(t, http.StatusForbidden, f.call(t, writeOnly, http.MethodGet, starredHistoryPath(f.topicA), nil).Code)
	noProjectRead := f.mint(t, f.admin, hubBoundary(), "inbox:read")
	assert.Equal(t, http.StatusNotFound, f.call(t, noProjectRead, http.MethodGet, starredHistoryPath(f.topicA), nil).Code)

	readerA := f.mint(t, f.admin, projectBoundary(f.projA), "inbox:read", "project:read")
	assert.Equal(t, http.StatusOK, f.call(t, readerA, http.MethodGet, starredHistoryPath(f.topicA), nil).Code)
	readerB := f.mint(t, f.admin, projectBoundary(f.projB), "inbox:read", "project:read")
	assert.Equal(t, http.StatusNotFound, f.call(t, readerB, http.MethodGet, starredHistoryPath(f.topicA), nil).Code)

	// DMs follow the DM token rule: the peer agent's project inside a
	// project boundary, a hub boundary for a DM between users.
	d := f.memberDMs(t)
	agentMsg := f.dmMsg(t, d.agentKey, d.agentConv, f.projA, "user:"+f.member, f.member, "a", "web", time.Now())
	humanMsg := f.dmMsg(t, d.humanKey, d.humanConv, uuid.Nil.String(), "user:"+f.member, f.member, "h", "web", time.Now())
	f.starAs(t, f.member, agentMsg, d.agentKey)
	f.starAs(t, f.member, humanMsg, d.humanKey)
	projAReader := f.mint(t, f.member, projectBoundary(f.projA), "inbox:read", "agent:read")
	// A project-boundary token can be minted only for a project its holder belongs to: give the member a role
	// in project B, whose boundary does not hold agent A's project.
	createTestUserWithProjectRole(t, f.s, f.member, f.member+"@test.com", f.projB, store.ProjectRoleMember)
	projBReader := f.mint(t, f.member, projectBoundary(f.projB), "inbox:read", "agent:read")
	hubReader := f.mint(t, f.member, hubBoundary(), "inbox:read", "agent:read")
	assert.Equal(t, []string{agentMsg}, historyIDs(decodeHistory(t, f.call(t, projAReader, http.MethodGet, starredHistoryPath(d.agentKey), nil))))
	assert.Equal(t, http.StatusNotFound, f.call(t, projBReader, http.MethodGet, starredHistoryPath(d.agentKey), nil).Code)
	assert.Equal(t, http.StatusNotFound, f.call(t, projAReader, http.MethodGet, starredHistoryPath(d.humanKey), nil).Code)
	assert.Equal(t, []string{humanMsg}, historyIDs(decodeHistory(t, f.call(t, hubReader, http.MethodGet, starredHistoryPath(d.humanKey), nil))))
}

// failingStarHistoryStore is a chat store whose ListStarredHistory fails.
type failingStarHistoryStore struct {
	WebChatStore
	MessageStarStore
}

func (s failingStarHistoryStore) ListStarredHistory(context.Context, StarHistoryQuery) (StarHistoryPage, error) {
	return StarHistoryPage{}, errFailingStarStore
}

func TestChatStarredHistory_StoreFailureIs500(t *testing.T) {
	f := newStarFixture(t, true)
	msg := f.threadMsg(t, f.topicA, f.projA, "m", time.Now())
	f.starAs(t, f.admin, msg, f.topicA)
	f.srv.SetWebChatStore(failingStarHistoryStore{WebChatStore: f.wcs, MessageStarStore: f.stars})
	for _, p := range []string{starredHistoryPath(f.topicA), starredHistoryPath(f.topicA, "around="+msg)} {
		rec := f.session(t, f.admin, http.MethodGet, p, nil)
		assert.Equal(t, http.StatusInternalServerError, rec.Code, p)
		assert.NotContains(t, rec.Body.String(), "do-not-disclose", p)
		assert.NotContains(t, rec.Body.String(), msg, "no unfiltered fallback")
	}
	f.srv.SetWebChatStore(f.wcs)
}
