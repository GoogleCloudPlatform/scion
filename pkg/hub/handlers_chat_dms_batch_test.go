// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build !no_sqlite

package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// dmCallCounter counts the store calls the DM list can make, per method.
type dmCallCounter struct {
	mu    sync.Mutex
	calls map[string]int
}

func (c *dmCallCounter) inc(method string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls[method]++
}

func (c *dmCallCounter) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = map[string]int{}
}

func (c *dmCallCounter) snapshot() (map[string]int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int, len(c.calls))
	total := 0
	for k, v := range c.calls {
		out[k] = v
		total += v
	}
	return out, total
}

// dmCountingStore counts both the per-DM lookups the list used to make and
// the batched lookups it makes now.
type dmCountingStore struct {
	store.Store
	c *dmCallCounter
}

func (s *dmCountingStore) ListMessages(ctx context.Context, f store.MessageFilter, o store.ListOptions) (*store.ListResult[store.Message], error) {
	s.c.inc("ListMessages")
	return s.Store.ListMessages(ctx, f, o)
}

func (s *dmCountingStore) GetConversationByExternalRef(ctx context.Context, surface, ref string) (*store.Conversation, error) {
	s.c.inc("GetConversationByExternalRef")
	return s.Store.GetConversationByExternalRef(ctx, surface, ref)
}

func (s *dmCountingStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	s.c.inc("GetUser")
	return s.Store.GetUser(ctx, id)
}

func (s *dmCountingStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	s.c.inc("GetAgent")
	return s.Store.GetAgent(ctx, id)
}

func (s *dmCountingStore) GetUsersByIDs(ctx context.Context, ids []string) (map[string]*store.User, error) {
	s.c.inc("GetUsersByIDs")
	return s.Store.GetUsersByIDs(ctx, ids)
}

func (s *dmCountingStore) GetAgentsByIDs(ctx context.Context, ids []string) (map[string]*store.Agent, error) {
	s.c.inc("GetAgentsByIDs")
	return s.Store.GetAgentsByIDs(ctx, ids)
}

func (s *dmCountingStore) GetAgentsByIDsIncludingDeleted(ctx context.Context, ids []string) (map[string]*store.Agent, error) {
	s.c.inc("GetAgentsByIDsIncludingDeleted")
	return s.Store.GetAgentsByIDsIncludingDeleted(ctx, ids)
}

func (s *dmCountingStore) GetConversationsByExternalRefs(ctx context.Context, surface string, refs []string) (map[string]*store.Conversation, error) {
	s.c.inc("GetConversationsByExternalRefs")
	return s.Store.GetConversationsByExternalRefs(ctx, surface, refs)
}

func (s *dmCountingStore) LatestMessagesByThreadIDs(ctx context.Context, ids []string, o store.LatestMessageOptions) (map[string]*store.Message, error) {
	s.c.inc("LatestMessagesByThreadIDs")
	return s.Store.LatestMessagesByThreadIDs(ctx, ids, o)
}

func (s *dmCountingStore) LatestMessagesByConversationIDs(ctx context.Context, ids []string, o store.LatestMessageOptions) (map[string]*store.Message, error) {
	s.c.inc("LatestMessagesByConversationIDs")
	return s.Store.LatestMessagesByConversationIDs(ctx, ids, o)
}

type dmCountingWebChatStore struct {
	WebChatStore
	c *dmCallCounter
}

func (w *dmCountingWebChatStore) ListDMs(ctx context.Context, participantID string) ([]WebChatDM, error) {
	w.c.inc("ListDMs")
	return w.WebChatStore.ListDMs(ctx, participantID)
}

func (w *dmCountingWebChatStore) GetReadState(ctx context.Context, userID, key string) (*WebChatReadState, error) {
	w.c.inc("GetReadState")
	return w.WebChatStore.GetReadState(ctx, userID, key)
}

func (w *dmCountingWebChatStore) GetReadStates(ctx context.Context, userID string, keys []string) ([]WebChatReadState, error) {
	w.c.inc("GetReadStates")
	return w.WebChatStore.GetReadStates(ctx, userID, keys)
}

// perDMReferenceEntries builds the DM list the way the handler did before
// its lookups were batched: one last-message read (nativeDMLastMessage),
// one peer read and one read-state read per DM. The batched handler must
// return exactly this.
func perDMReferenceEntries(t *testing.T, srv *Server, wcs WebChatStore, userID string) []chatDMEntry {
	t.Helper()
	ctx := context.Background()
	dms, err := wcs.ListDMs(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	entries := make([]chatDMEntry, 0, len(dms))
	for _, dm := range dms {
		entry := chatDMEntry{
			ConversationKey: dm.ConversationKey,
			PeerID:          dm.PeerID,
			PeerKind:        dm.PeerKind,
			LastActivityAt:  dm.LastActivityAt,
		}
		if last, err := srv.nativeDMLastMessage(ctx, dm.ConversationKey); err == nil && last != nil {
			entry.LastMessageID = last.ID
			entry.LastMessagePreview = truncatePreview(last.Msg, 120)
			entry.LastMessageSender = last.Sender
		}
		switch dm.PeerKind {
		case "user":
			if u, err := srv.store.GetUser(ctx, dm.PeerID); err == nil {
				entry.PeerName = u.DisplayName
				entry.PeerEmail = u.Email
				entry.PeerAvatar = u.AvatarURL
			}
		case "agent":
			if a, err := srv.store.GetAgent(ctx, dm.PeerID); err == nil {
				entry.PeerName = a.Name
				entry.PeerSlug = a.Slug
			}
		}
		rs, err := wcs.GetReadState(ctx, userID, dm.ConversationKey)
		if err != nil {
			t.Fatal(err)
		}
		if rs != nil {
			entry.LastReadMessageID = rs.LastReadMessageID
			entry.Muted = rs.Muted
		}
		entry.HasUnread = entry.LastMessageID != "" && entry.LastMessageID != entry.LastReadMessageID
		entries = append(entries, entry)
	}
	return entries
}

// dmMixFixture seeds a caller's DM list covering every case the list
// distinguishes, plus filler DMs to scale the list.
type dmMixFixture struct {
	t        *testing.T
	s        store.Store
	wcs      WebChatStore
	project  *store.Project
	envelope bool
	base     time.Time
	seq      int
}

func (f *dmMixFixture) at() time.Time {
	f.seq++
	return f.base.Add(time.Duration(f.seq) * time.Second)
}

// dm registers a DM with peer and returns its key and conversation ID
// (empty when withConversation is false).
func (f *dmMixFixture) dm(peerKind, peerID string, withConversation bool, watermark string) (string, string) {
	f.t.Helper()
	key, err := messages.DMConversationKey(peerKind, peerID, "user", DevUserID)
	if err != nil {
		f.t.Fatal(err)
	}
	convID := ""
	if withConversation {
		convID = seedConversation(f.t, f.s, "native", key, "direct")
	}
	if err := f.wcs.UpsertDM(context.Background(), WebChatDM{ConversationKey: key, ParticipantID: DevUserID,
		PeerID: peerID, PeerKind: peerKind, LastMessageID: watermark, LastActivityAt: f.at()}); err != nil {
		f.t.Fatal(err)
	}
	return key, convID
}

func (f *dmMixFixture) msg(name, sender, channel, kind, thread, conv string, at time.Time) string {
	f.t.Helper()
	m := &store.Message{ID: tid(name), ProjectID: f.project.ID, Sender: sender, Recipient: "user:dev@localhost",
		RecipientID: DevUserID, Msg: "body of " + name, Type: kind, Channel: channel, ThreadID: thread,
		ConversationID: conv, CreatedAt: at}
	if err := f.s.CreateMessage(context.Background(), m); err != nil {
		f.t.Fatal(err)
	}
	return m.ID
}

func (f *dmMixFixture) agent(name string) string {
	return rsAgent(f.t, f.s, name, f.project.ID)
}

// seedCases creates one DM per case. Every message carries both the thread
// key and the conversation ID unless the case is about one of them.
func (f *dmMixFixture) seedCases() {
	t, ctx := f.t, context.Background()
	chat := messages.TypeChat

	// Read up to its latest message: no unread.
	a := f.agent("dmmix-read")
	key, conv := f.dm("agent", a, true, "")
	last := f.msg("dmmix-read-1", "agent:dmmix-read", "web", chat, key, conv, f.at())
	if err := f.wcs.SetReadState(ctx, DevUserID, key, last); err != nil {
		t.Fatal(err)
	}

	// Muted with an unread message.
	a = f.agent("dmmix-muted")
	key, conv = f.dm("agent", a, true, "")
	f.msg("dmmix-muted-1", "agent:dmmix-muted", "web", chat, key, conv, f.at())
	if err := f.wcs.SetMuted(ctx, DevUserID, key, true); err != nil {
		t.Fatal(err)
	}

	// Newest row is a mention fan-out copy, which must not be the last
	// message; read up to the visible one, so no unread either.
	a = f.agent("dmmix-mention")
	key, conv = f.dm("agent", a, true, "")
	visible := f.msg("dmmix-mention-visible", "agent:dmmix-mention", "web", chat, key, conv, f.at())
	f.msg("dmmix-mention-copy", "agent:dmmix-mention", "web", messages.TypeMention, key, conv, f.at())
	if err := f.wcs.SetReadState(ctx, DevUserID, key, visible); err != nil {
		t.Fatal(err)
	}

	// Never used: no conversation, no messages.
	a = f.agent("dmmix-empty")
	f.dm("agent", a, false, "")

	// Activity watermark names a deleted message; an older visible one is
	// the last message, and the read state still names the deleted one.
	a = f.agent("dmmix-deleted")
	key, conv = f.dm("agent", a, true, tid("dmmix-deleted-gone"))
	f.msg("dmmix-deleted-older", "agent:dmmix-deleted", "web", chat, key, conv, f.at())
	if err := f.wcs.SetReadState(ctx, DevUserID, key, tid("dmmix-deleted-gone")); err != nil {
		t.Fatal(err)
	}

	// Only an external-channel message: nothing visible.
	a = f.agent("dmmix-external")
	key, conv = f.dm("agent", a, true, "")
	f.msg("dmmix-external-1", "agent:dmmix-external", "discord", chat, key, conv, f.at())

	// Conversation-only message (no thread key): visible in envelope mode
	// only, which the reference reproduces for each mode.
	a = f.agent("dmmix-convonly")
	key, conv = f.dm("agent", a, true, "")
	f.msg("dmmix-convonly-thread", "agent:dmmix-convonly", "web", chat, key, conv, f.at())
	f.msg("dmmix-convonly-conv", "agent:dmmix-convonly", "web", chat, "", conv, f.at())

	// Two visible messages at the same instant: the greater ID wins.
	a = f.agent("dmmix-tie")
	key, conv = f.dm("agent", a, true, "")
	tie := f.at()
	f.msg("dmmix-tie-a", "agent:dmmix-tie", "web", chat, key, conv, tie)
	f.msg("dmmix-tie-b", "agent:dmmix-tie", "web", chat, key, conv, tie)

	// Soft-deleted agent peer keeps its name.
	a = f.agent("dmmix-gone-agent")
	markAgentSoftDeleted(t, f.s, a)
	key, conv = f.dm("agent", a, true, "")
	f.msg("dmmix-gone-agent-1", "agent:dmmix-gone-agent", "web", chat, key, conv, f.at())

	// Agent peer that does not exist at all.
	key, conv = f.dm("agent", tid("dmmix-missing-agent"), true, "")
	f.msg("dmmix-missing-agent-1", "agent:dmmix-missing-agent", "web", chat, key, conv, f.at())

	// Human peer with a long message (preview truncation).
	peer := &store.User{ID: tid("dmmix-human"), Email: "dmmix-human@example.com", DisplayName: "DM Mix Human",
		AvatarURL: "https://example.com/a.png", Role: "member", Status: "active", Created: time.Now()}
	if err := f.s.CreateUser(ctx, peer); err != nil {
		t.Fatal(err)
	}
	key, conv = f.dm("user", peer.ID, true, "")
	long := &store.Message{ID: tid("dmmix-human-1"), ProjectID: f.project.ID, Sender: "user:dmmix-human@example.com",
		SenderID: peer.ID, Recipient: "user:dev@localhost", RecipientID: DevUserID,
		Msg: fmt.Sprintf("%0300d", 7), Type: chat, Channel: "web", ThreadID: key, ConversationID: conv, CreatedAt: f.at()}
	if err := f.s.CreateMessage(ctx, long); err != nil {
		t.Fatal(err)
	}
}

// seedFiller adds n plain agent DMs with one unread message each.
func (f *dmMixFixture) seedFiller(n int) {
	for i := 0; i < n; i++ {
		name := "dmmix-filler-" + strconv.Itoa(i)
		a := f.agent(name)
		key, conv := f.dm("agent", a, true, "")
		f.msg(name+"-1", "agent:"+name, "web", messages.TypeChat, key, conv, f.at())
	}
}

func newDMMixServer(t *testing.T, envelope bool) (*Server, *dmMixFixture, *dmCallCounter) {
	t.Helper()
	srv, s, wcs, project, _ := setupSendTest(t)
	ctx := context.Background()
	settings := newFakeHubSettingStore()
	ops := NewOperationalSettings(settings, emptyKoanf(), emptyKoanf())
	settings.seed("messaging", json.RawMessage(`{"conversation_envelope_switch":`+strconv.FormatBool(envelope)+`}`))
	if _, err := ops.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	srv.SetOperationalSettings(ops)

	counter := &dmCallCounter{calls: map[string]int{}}
	srv.store = &dmCountingStore{Store: s, c: counter}
	countingWCS := &dmCountingWebChatStore{WebChatStore: wcs, c: counter}
	srv.SetWebChatStore(countingWCS)

	f := &dmMixFixture{t: t, s: s, wcs: wcs, project: project, envelope: envelope,
		base: time.Now().UTC().Add(-time.Hour)}
	return srv, f, counter
}

func listDMs(t *testing.T, srv *Server) []chatDMEntry {
	t.Helper()
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/dms", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var resp chatDMListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.DMs
}

// normalizeDMEntries round-trips entries through JSON so timestamps compare
// the way clients see them.
func normalizeDMEntries(t *testing.T, entries []chatDMEntry) []chatDMEntry {
	t.Helper()
	raw, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	var out []chatDMEntry
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestChatDMs_BatchedMatchesPerDMLookups pins the batched DM list to the
// response the per-DM lookups produced, for a mix of DMs, in both legacy
// and conversation-envelope mode.
func TestChatDMs_BatchedMatchesPerDMLookups(t *testing.T) {
	for _, envelope := range []bool{false, true} {
		t.Run("envelope="+strconv.FormatBool(envelope), func(t *testing.T) {
			srv, f, _ := newDMMixServer(t, envelope)
			f.seedCases()

			got := listDMs(t, srv)
			want := normalizeDMEntries(t, perDMReferenceEntries(t, srv, f.wcs, DevUserID))
			if !reflect.DeepEqual(got, want) {
				for i := range want {
					if i < len(got) && !reflect.DeepEqual(got[i], want[i]) {
						t.Errorf("entry %d:\n got %+v\nwant %+v", i, got[i], want[i])
					}
				}
				t.Fatalf("batched list differs from per-DM lookups (got %d entries, want %d)", len(got), len(want))
			}

			// Spot-check the cases themselves so the reference cannot
			// silently drift with the handler.
			byPeer := map[string]chatDMEntry{}
			for _, e := range got {
				byPeer[e.PeerName] = e
			}
			check := func(peer string, wantLast string, unread, muted bool) {
				t.Helper()
				e, ok := byPeer[peer]
				if !ok {
					t.Fatalf("no DM with peer %q in %+v", peer, got)
				}
				if e.LastMessageID != wantLast || e.HasUnread != unread || e.Muted != muted {
					t.Errorf("%s: last=%q unread=%v muted=%v; want last=%q unread=%v muted=%v",
						peer, e.LastMessageID, e.HasUnread, e.Muted, wantLast, unread, muted)
				}
			}
			check("dmmix-read", tid("dmmix-read-1"), false, false)
			check("dmmix-muted", tid("dmmix-muted-1"), true, true)
			check("dmmix-mention", tid("dmmix-mention-visible"), false, false)
			check("dmmix-empty", "", false, false)
			check("dmmix-deleted", tid("dmmix-deleted-older"), true, false)
			check("dmmix-external", "", false, false)
			if envelope {
				check("dmmix-convonly", tid("dmmix-convonly-conv"), true, false)
			} else {
				check("dmmix-convonly", tid("dmmix-convonly-thread"), true, false)
			}
			tieWant := tid("dmmix-tie-a")
			if tid("dmmix-tie-b") > tieWant {
				tieWant = tid("dmmix-tie-b")
			}
			check("dmmix-tie", tieWant, true, false)
			check("dmmix-gone-agent", tid("dmmix-gone-agent-1"), true, false)
			check("DM Mix Human", tid("dmmix-human-1"), true, false)
			if e := byPeer["DM Mix Human"]; e.PeerEmail != "dmmix-human@example.com" || len(e.LastMessagePreview) > 125 {
				t.Errorf("human peer entry: %+v", e)
			}
			// The missing agent has no name; it is the only unnamed DM.
			if e, ok := byPeer[""]; !ok || e.LastMessageID != tid("dmmix-missing-agent-1") {
				t.Errorf("missing-agent entry: %+v (present=%v)", e, ok)
			}

			// Ordering is the ListDMs order: newest activity first.
			for i := 1; i < len(got); i++ {
				if got[i].LastActivityAt.After(got[i-1].LastActivityAt) {
					t.Fatalf("entries out of activity order at %d: %v after %v",
						i, got[i].LastActivityAt, got[i-1].LastActivityAt)
				}
			}
		})
	}
}

// TestChatDMs_StoreCallsDoNotScaleWithDMs counts the store calls one DM
// list request makes, against the per-DM lookups it replaced, for a small
// and an 80-DM list.
func TestChatDMs_StoreCallsDoNotScaleWithDMs(t *testing.T) {
	for _, envelope := range []bool{false, true} {
		t.Run("envelope="+strconv.FormatBool(envelope), func(t *testing.T) {
			srv, f, counter := newDMMixServer(t, envelope)
			f.seedCases()

			measure := func() (batched map[string]int, batchedTotal, perDMTotal, dms int) {
				t.Helper()
				counter.reset()
				dms = len(listDMs(t, srv))
				batched, batchedTotal = counter.snapshot()
				counter.reset()
				perDMReferenceEntries(t, srv, srv.webChatStore, DevUserID)
				_, perDMTotal = counter.snapshot()
				return batched, batchedTotal, perDMTotal, dms
			}

			smallCalls, smallTotal, smallPerDM, smallN := measure()
			f.seedFiller(80 - smallN)
			largeCalls, largeTotal, largePerDM, largeN := measure()
			if largeN != 80 {
				t.Fatalf("large list has %d DMs, want 80", largeN)
			}

			want := map[string]int{
				"ListDMs":                        1,
				"LatestMessagesByThreadIDs":      1,
				"GetUsersByIDs":                  1,
				"GetAgentsByIDsIncludingDeleted": 1,
				"GetReadStates":                  1,
			}
			if envelope {
				delete(want, "LatestMessagesByThreadIDs")
				want["GetConversationsByExternalRefs"] = 1
				want["LatestMessagesByConversationIDs"] = 1
			}
			for _, calls := range []map[string]int{smallCalls, largeCalls} {
				if !reflect.DeepEqual(calls, want) {
					t.Fatalf("batched store calls = %v; want %v", calls, want)
				}
			}
			if smallTotal != largeTotal {
				t.Fatalf("batched calls grew with DMs: %d for %d DMs, %d for %d", smallTotal, smallN, largeTotal, largeN)
			}
			if largePerDM <= largeTotal {
				t.Fatalf("per-DM reference made %d calls, batched %d", largePerDM, largeTotal)
			}
			t.Logf("store calls: %d DMs per-DM=%d batched=%d; %d DMs per-DM=%d batched=%d",
				smallN, smallPerDM, smallTotal, largeN, largePerDM, largeTotal)
		})
	}
}
