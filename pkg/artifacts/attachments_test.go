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
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// messageHost is fakeHost with MessageScopeHost: readers maps a message id
// to the principal refs that may read it, and noMessageRead lists the
// principal refs whose credential does not permit reading messages.
type messageHost struct {
	*fakeHost
	mmu           sync.Mutex
	readers       map[string]map[string]bool
	noMessageRead map[string]bool
	asked         []string
}

var _ MessageScopeHost = (*messageHost)(nil)

func newMessageHost() *messageHost {
	return &messageHost{fakeHost: newFakeHost(), readers: map[string]map[string]bool{}, noMessageRead: map[string]bool{}}
}

func (h *messageHost) allowMessage(p principal, messageID string) {
	h.mmu.Lock()
	defer h.mmu.Unlock()
	if h.readers[messageID] == nil {
		h.readers[messageID] = map[string]bool{}
	}
	h.readers[messageID][PrincipalRef(p.kind, p.ref)] = true
}

func (h *messageHost) revokeMessage(p principal, messageID string) {
	h.mmu.Lock()
	defer h.mmu.Unlock()
	delete(h.readers[messageID], PrincipalRef(p.kind, p.ref))
}

func (h *messageHost) PermitsMessageRead(ctx context.Context) bool {
	kind, ref, _, ok := h.Principal(ctx)
	if !ok {
		return false
	}
	h.mmu.Lock()
	defer h.mmu.Unlock()
	h.asked = append(h.asked, "permits-message-read")
	return !h.noMessageRead[PrincipalRef(kind, ref)]
}

func (h *messageHost) MayReadMessage(ctx context.Context, messageID string) bool {
	kind, ref, _, ok := h.Principal(ctx)
	if !ok {
		return false
	}
	h.mmu.Lock()
	defer h.mmu.Unlock()
	h.asked = append(h.asked, "may-read "+messageID)
	return h.readers[messageID][PrincipalRef(kind, ref)]
}

func (h *messageHost) askedCalls() []string {
	h.mmu.Lock()
	defer h.mmu.Unlock()
	return append([]string(nil), h.asked...)
}

// attachmentFixture is a fixture whose service runs over a messageHost.
type attachmentFixture struct {
	*fixture
	mh *messageHost
}

func newAttachmentFixture(t *testing.T, redirect bool) *attachmentFixture {
	t.Helper()
	f := newFixture(t, redirect)
	mh := newMessageHost()
	mh.fakeHost = f.host
	svc := NewService(mh)
	svc.SetStore(f.store)
	svc.SetBlobStorage(f.blobs, "hub-1")
	f.svc = svc
	return &attachmentFixture{fixture: f, mh: mh}
}

func ctxAs(p principal) context.Context {
	return context.WithValue(context.Background(), principalKey{}, p)
}

// upload stores body as an upload of p and returns its id.
func (f *attachmentFixture) upload(p principal, name, mediaType string, body []byte) string {
	f.t.Helper()
	info, err := f.svc.CreateAttachment(ctxAs(p), name, mediaType, bytes.NewReader(body))
	if err != nil {
		f.t.Fatalf("CreateAttachment: %v", err)
	}
	return info.ID
}

// send uploads body as p and binds it to messageID.
func (f *attachmentFixture) send(p principal, messageID string, body []byte) string {
	f.t.Helper()
	id := f.upload(p, "shot.png", "image/png", body)
	if err := f.svc.BindAttachment(context.Background(), id, p.kind, p.ref, messageID); err != nil {
		f.t.Fatalf("BindAttachment: %v", err)
	}
	return id
}

// serve runs ServeAttachment for p (nil: no caller).
func (f *attachmentFixture) serve(p *principal, target, id string) (ServeOutcome, *httptest.ResponseRecorder) {
	f.t.Helper()
	r := httptest.NewRequest(http.MethodGet, target, nil)
	if p != nil {
		r = withPrincipal(r, *p)
	}
	rec := httptest.NewRecorder()
	return f.svc.ServeAttachment(rec, r, id), rec
}

var pngBytes = []byte("\x89PNG\r\n\x1a\nnot-really-an-image")

// TestAttachmentUploadReadableByUploaderOnly: an unsent upload is served
// to its uploader and refused to everyone else, and nothing is written for
// a refusal.
func TestAttachmentUploadReadableByUploaderOnly(t *testing.T) {
	f := newAttachmentFixture(t, false)
	id := f.upload(userU, "shot.png", "image/png", pngBytes)

	out, rec := f.serve(&userU, "/api/v1/chat/attachments/"+id, id)
	if out != ServeWritten || rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), pngBytes) {
		t.Fatalf("uploader: outcome %d, status %d, body %q", out, rec.Code, rec.Body.String())
	}
	for _, p := range []principal{outside, agentA} {
		out, rec := f.serve(&p, "/api/v1/chat/attachments/"+id, id)
		if out != ServeRefused || rec.Body.Len() != 0 {
			t.Errorf("%s: outcome %d, body %q; want ServeRefused with nothing written", p.ref, out, rec.Body.String())
		}
	}
	if out, _ := f.serve(nil, "/api/v1/chat/attachments/"+id, id); out != ServeRefused {
		t.Errorf("no caller: outcome %d, want ServeRefused", out)
	}
	if out, _ := f.serve(&userU, "/x", uuid.NewString()); out != ServeNotFound {
		t.Errorf("unknown id: outcome %d, want ServeNotFound", out)
	}
	if out, _ := f.serve(&userU, "/x", "not-a-uuid"); out != ServeNotFound {
		t.Errorf("malformed id: outcome %d, want ServeNotFound", out)
	}
}

// TestAttachmentMessageReadRule: a sent attachment is served exactly to
// the readers of its message, asked on every request, so a reader who
// loses the message loses the attachment at once.
func TestAttachmentMessageReadRule(t *testing.T) {
	f := newAttachmentFixture(t, false)
	id := f.send(userU, "msg-1", pngBytes)
	f.mh.allowMessage(outside, "msg-1")

	out, rec := f.serve(&outside, "/x", id)
	if out != ServeWritten || rec.Code != http.StatusOK {
		t.Fatalf("message reader: outcome %d, status %d", out, rec.Code)
	}
	if out, rec := f.serve(&agentA, "/x", id); out != ServeRefused || rec.Body.Len() != 0 {
		t.Errorf("non-reader: outcome %d, body %q; want ServeRefused", out, rec.Body.String())
	}
	// The uploader reads a sent attachment only as a reader of its message.
	if out, _ := f.serve(&userU, "/x", id); out != ServeRefused {
		t.Errorf("uploader without message read: outcome %d, want ServeRefused", out)
	}
	f.mh.revokeMessage(outside, "msg-1")
	if out, _ := f.serve(&outside, "/x", id); out != ServeRefused {
		t.Errorf("reader who lost the message: outcome %d, want ServeRefused", out)
	}
	found := false
	for _, c := range f.mh.askedCalls() {
		if c == "may-read msg-1" {
			found = true
		}
	}
	if !found {
		t.Errorf("MayReadMessage was never asked about msg-1: %v", f.mh.askedCalls())
	}
}

// TestAttachmentHostWithoutExtensionFailsClosed: over a host without
// MessageScopeHost, no attachment is readable, not even by its uploader.
func TestAttachmentHostWithoutExtensionFailsClosed(t *testing.T) {
	f := newAttachmentFixture(t, false)
	up := f.upload(userU, "a.txt", "text/plain", []byte("hello"))
	sent := f.send(userU, "msg-1", pngBytes)
	f.mh.allowMessage(userU, "msg-1")

	plain := NewService(f.host)
	plain.SetStore(f.store)
	plain.SetBlobStorage(f.blobs, "hub-1")
	for _, id := range []string{up, sent} {
		r := withPrincipal(httptest.NewRequest(http.MethodGet, "/x", nil), userU)
		rec := httptest.NewRecorder()
		if out := plain.ServeAttachment(rec, r, id); out != ServeRefused {
			t.Errorf("host without MessageScopeHost: outcome %d for %s, want ServeRefused", out, id)
		}
	}
}

// TestAttachmentCredentialRefusedBeforeMessageCheck: a caller whose
// credential does not permit reading messages is refused, and the message
// is never asked about.
func TestAttachmentCredentialRefusedBeforeMessageCheck(t *testing.T) {
	f := newAttachmentFixture(t, false)
	id := f.send(userU, "msg-1", pngBytes)
	f.mh.allowMessage(outside, "msg-1")
	f.mh.mmu.Lock()
	f.mh.noMessageRead[PrincipalRef(outside.kind, outside.ref)] = true
	f.mh.asked = nil
	f.mh.mmu.Unlock()

	if out, rec := f.serve(&outside, "/x", id); out != ServeRefused || rec.Body.Len() != 0 {
		t.Fatalf("credential without message read: outcome %d, body %q; want ServeRefused", out, rec.Body.String())
	}
	for _, c := range f.mh.askedCalls() {
		if strings.HasPrefix(c, "may-read") {
			t.Errorf("MayReadMessage asked before the credential check refused: %v", f.mh.askedCalls())
		}
	}
	// The uploader's own upload is refused the same way.
	up := f.upload(outside, "a.txt", "text/plain", []byte("x"))
	if out, _ := f.serve(&outside, "/x", up); out != ServeRefused {
		t.Errorf("upload, credential without message read: outcome %d, want ServeRefused", out)
	}
}

// TestAttachmentArtifactRoutesAnswerNotFound: on every artifact route an
// attachment id answers exactly like an unknown id, also to its uploader
// and to a reader of its message (grants and links included).
func TestAttachmentArtifactRoutesAnswerNotFound(t *testing.T) {
	f := newAttachmentFixture(t, false)
	sent := f.send(userU, "msg-1", pngBytes)
	up := f.upload(userU, "b.png", "image/png", pngBytes)
	f.mh.allowMessage(userU, "msg-1")
	f.mh.allowMessage(outside, "msg-1")
	unknown := uuid.NewString()

	type call struct{ method, path, body string }
	calls := func(id string) []call {
		base := "/api/v1/artifacts/" + id
		return []call{
			{http.MethodGet, base, ""},
			{http.MethodGet, base + "/files/shot.png", ""},
			{http.MethodGet, base + "/versions", ""},
			{http.MethodGet, base + "/versions/1", ""},
			{http.MethodGet, base + "/versions/1/files/shot.png", ""},
			{http.MethodPost, base + "/versions", `{"entry":"x.md","files":[{"path":"x.md","size":1,"sha256":"` + sha([]byte("x")) + `"}]}`},
			{http.MethodPost, base + "/versions/1/view", ""},
			{http.MethodPatch, base, `{"expiresAt":"2099-01-01T00:00:00Z"}`},
			{http.MethodDelete, base, ""},
			{http.MethodGet, base + "/grants", ""},
			{http.MethodPost, base + "/grants", `{"subjectKind":"principal","subjectRef":"user:user-2","permission":"read"}`},
			{http.MethodGet, base + "/links", ""},
			{http.MethodPost, base + "/links", `{}`},
		}
	}
	for _, p := range []principal{userU, outside} {
		for _, id := range []string{sent, up} {
			want := calls(unknown)
			for i, c := range calls(id) {
				got := f.do(&p, c.method, c.path, []byte(c.body), map[string]string{"Content-Type": "application/json"})
				ref := f.do(&p, want[i].method, want[i].path, []byte(want[i].body), map[string]string{"Content-Type": "application/json"})
				if got.Code != http.StatusNotFound || got.Code != ref.Code || got.Body.String() != ref.Body.String() {
					t.Errorf("%s %s %s as %s: %d %q; unknown id: %d %q", c.method, c.path, id, p.ref,
						got.Code, got.Body.String(), ref.Code, ref.Body.String())
				}
			}
		}
	}
	// The rows are still there and still readable through their own path.
	if out, _ := f.serve(&outside, "/x", sent); out != ServeWritten {
		t.Errorf("message reader after the route calls: outcome %d, want ServeWritten", out)
	}
	if out, _ := f.serve(&userU, "/x", up); out != ServeWritten {
		t.Errorf("uploader after the route calls: outcome %d, want ServeWritten", out)
	}
}

// TestAttachmentNotListed: the artifact list never shows an attachment,
// not even to its owner.
func TestAttachmentNotListed(t *testing.T) {
	f := newAttachmentFixture(t, false)
	f.send(userU, "msg-1", pngBytes)
	f.mh.allowMessage(userU, "msg-1")
	f.upload(userU, "b.png", "image/png", pngBytes)
	pub := f.publish(userU, "notes.md", []byte("# n"), "scope=project-1")

	rec := f.do(&userU, http.MethodGet, "/api/v1/artifacts?mine=1", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var resp ArtifactListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Artifacts) != 1 || resp.Artifacts[0].ID != pub.Artifact.ID {
		t.Errorf("list = %+v; want only the published artifact", resp.Artifacts)
	}
}

// TestAttachmentDelivery: a readable attachment goes through the artifact
// file delivery: on an object store a 302 to a signed URL, streamed with
// ?stream=1, with the safety headers; inline only for a safe type.
func TestAttachmentDelivery(t *testing.T) {
	f := newAttachmentFixture(t, true)
	img := f.upload(userU, "shot.png", "image/png", pngBytes)
	doc := f.upload(userU, "report.pdf", "application/pdf", []byte("%PDF-1.4 x"))

	out, rec := f.serve(&userU, "/api/v1/chat/attachments/"+img, img)
	if out != ServeWritten || rec.Code != http.StatusFound {
		t.Fatalf("object store: outcome %d, status %d; want 302", out, rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "https://objects.example/") {
		t.Errorf("Location = %q", loc)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}

	out, rec = f.serve(&userU, "/api/v1/chat/attachments/"+img+"?stream=1", img)
	if out != ServeWritten || rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), pngBytes) {
		t.Fatalf("stream: outcome %d, status %d", out, rec.Code)
	}
	if got := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "inline") {
		t.Errorf("image Content-Disposition = %q, want inline", got)
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Error("streamed attachment has no Content-Security-Policy")
	}
	_, rec = f.serve(&userU, "/api/v1/chat/attachments/"+doc+"?stream=1", doc)
	if got := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "attachment") {
		t.Errorf("pdf Content-Disposition = %q, want attachment", got)
	}
}

// TestUsableAttachment: send validation accepts only an unsent upload of
// the sender, and reports an id it does not know as absent.
func TestUsableAttachment(t *testing.T) {
	f := newAttachmentFixture(t, false)
	ctx := context.Background()
	up := f.upload(userU, "a.txt", "text/plain", []byte("hello"))
	sent := f.send(userU, "msg-1", pngBytes)

	info, state, err := f.svc.UsableAttachment(ctx, up, userU.kind, userU.ref)
	if err != nil || state != AttachmentUsable || info.ID != up || info.Name != "a.txt" || info.MediaType != "text/plain" || info.Size != 5 {
		t.Errorf("own upload = %+v, %d, %v", info, state, err)
	}
	if _, state, err := f.svc.UsableAttachment(ctx, up, outside.kind, outside.ref); err != nil || state != AttachmentUnusable {
		t.Errorf("another user's upload = %d, %v; want AttachmentUnusable", state, err)
	}
	if _, state, err := f.svc.UsableAttachment(ctx, sent, userU.kind, userU.ref); err != nil || state != AttachmentUnusable {
		t.Errorf("a sent attachment = %d, %v; want AttachmentUnusable", state, err)
	}
	if _, state, err := f.svc.UsableAttachment(ctx, uuid.NewString(), userU.kind, userU.ref); err != nil || state != AttachmentAbsent {
		t.Errorf("unknown id = %d, %v; want AttachmentAbsent", state, err)
	}
	if err := f.svc.BindAttachment(ctx, up, outside.kind, outside.ref, "msg-2"); !errors.Is(err, ErrAttachmentNotOwned) {
		t.Errorf("bind by another user = %v, want ErrAttachmentNotOwned", err)
	}
	if err := f.svc.BindAttachment(ctx, sent, userU.kind, userU.ref, "msg-2"); !errors.Is(err, ErrAttachmentBound) {
		t.Errorf("second bind = %v, want ErrAttachmentBound", err)
	}
}

// TestCreateAttachmentLimits: the artifact file size limit applies, and a
// name that cannot name an artifact file is refused.
func TestCreateAttachmentLimits(t *testing.T) {
	f := newAttachmentFixture(t, false)
	f.svc.SetLimits(func(context.Context) Limits { return Limits{MaxFileBytes: 4} })
	if _, err := f.svc.CreateAttachment(ctxAs(userU), "a.txt", "text/plain", strings.NewReader("12345")); !errors.Is(err, ErrAttachmentTooLarge) {
		t.Errorf("oversized = %v, want ErrAttachmentTooLarge", err)
	}
	if _, err := f.svc.CreateAttachment(ctxAs(userU), "a.txt", "text/plain", strings.NewReader("1234")); err != nil {
		t.Errorf("at the limit = %v", err)
	}
	for _, name := range []string{"", "dir/a.txt", "../a.txt", RemotePrefix + "x.png"} {
		if _, err := f.svc.CreateAttachment(ctxAs(userU), name, "text/plain", strings.NewReader("1")); !errors.Is(err, ErrAttachmentName) {
			t.Errorf("name %q = %v, want ErrAttachmentName", name, err)
		}
	}
	if _, err := f.svc.CreateAttachment(context.Background(), "a.txt", "text/plain", strings.NewReader("1")); err == nil {
		t.Error("an upload without a caller was stored")
	}
}

// TestOpenBoundAttachment: the delivery opener reads only an attachment
// bound to the named message.
func TestOpenBoundAttachment(t *testing.T) {
	f := newAttachmentFixture(t, false)
	ctx := context.Background()
	sent := f.send(userU, "msg-1", pngBytes)
	up := f.upload(userU, "a.txt", "text/plain", []byte("x"))

	rc, info, err := f.svc.OpenBoundAttachment(ctx, sent, "msg-1")
	if err != nil {
		t.Fatalf("bound attachment: %v", err)
	}
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(b, pngBytes) || info.Name != "shot.png" {
		t.Errorf("read %q, info %+v", b, info)
	}
	if _, _, err := f.svc.OpenBoundAttachment(ctx, sent, "msg-2"); !errors.Is(err, ErrNotFound) {
		t.Errorf("other message = %v, want ErrNotFound", err)
	}
	if _, _, err := f.svc.OpenBoundAttachment(ctx, up, "msg-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unsent upload = %v, want ErrNotFound", err)
	}
}

// TestMessageAttachmentsAndRetireScope: history lists a message's
// attachments until the message is retired.
func TestMessageAttachmentsAndRetireScope(t *testing.T) {
	f := newAttachmentFixture(t, false)
	ctx := context.Background()
	sent := f.send(userU, "msg-1", pngBytes)
	f.mh.allowMessage(userU, "msg-1")

	got, err := f.svc.MessageAttachments(ctx, []string{"msg-1", "msg-2"})
	if err != nil || len(got) != 1 || len(got["msg-1"]) != 1 || got["msg-1"][0].ID != sent {
		t.Fatalf("MessageAttachments = %+v, %v", got, err)
	}
	if _, err := f.svc.RetireScope(ctx, ScopeKindUpload, "user:user-1"); err == nil {
		t.Error("RetireScope accepted an upload scope")
	}
	if n, err := f.svc.RetireScope(ctx, ScopeKindMessage, "msg-1"); err != nil || n != 1 {
		t.Fatalf("RetireScope = %d, %v", n, err)
	}
	if out, _ := f.serve(&userU, "/x", sent); out != ServeNotFound {
		t.Errorf("after retire: outcome %d, want ServeNotFound", out)
	}
	if got, _ := f.svc.MessageAttachments(ctx, []string{"msg-1"}); len(got) != 0 {
		t.Errorf("after retire MessageAttachments = %+v", got)
	}
}

// fakeScopeChecker answers ScopesExist from a table; err fails it.
type fakeScopeChecker struct {
	exists map[string]bool
	err    error
	asked  [][]string
}

func (c *fakeScopeChecker) ScopesExist(_ context.Context, refs []string) (map[string]bool, error) {
	c.asked = append(c.asked, refs)
	if c.err != nil {
		return nil, c.err
	}
	out := map[string]bool{}
	for _, r := range refs {
		if e, ok := c.exists[r]; ok {
			out[r] = e
		}
	}
	return out, nil
}

// TestRetireAttachments: the maintenance pass retires uploads older than
// UploadTTL and the attachments of messages the checker reports gone,
// keeps those of messages that exist or are not answered, and stops on a
// checker error.
func TestRetireAttachments(t *testing.T) {
	forEachBackend(t, func(t *testing.T, _ *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		now := time.Now()
		stale := seedAttachment(t, st, PrincipalKindUser, "u", ScopeKindUpload, "user:u", now.Add(-UploadTTL-time.Minute))
		fresh := seedAttachment(t, st, PrincipalKindUser, "u", ScopeKindUpload, "user:u", now)
		gone := seedAttachment(t, st, PrincipalKindUser, "u", ScopeKindMessage, "msg-gone", now)
		live := seedAttachment(t, st, PrincipalKindUser, "u", ScopeKindMessage, "msg-live", now)
		unanswered := seedAttachment(t, st, PrincipalKindUser, "u", ScopeKindMessage, "msg-unknown", now)

		failing := &fakeScopeChecker{err: errors.New("down")}
		res, err := RetireAttachments(ctx, st, failing, now)
		if err == nil || res.Unsent != 1 || res.Orphaned != 0 {
			t.Fatalf("with a failing checker = %+v, %v; want the stale upload only and an error", res, err)
		}
		if _, err := st.GetAttachment(ctx, gone.ID); err != nil {
			t.Errorf("a failing checker retired a message attachment: %v", err)
		}

		checker := &fakeScopeChecker{exists: map[string]bool{
			MessageScopeRef("msg-gone"): false, MessageScopeRef("msg-live"): true,
		}}
		res, err = RetireAttachments(ctx, st, checker, now)
		if err != nil || res.Orphaned != 1 || res.Unsent != 0 {
			t.Fatalf("RetireAttachments = %+v, %v; want one orphaned", res, err)
		}
		for _, a := range []*Artifact{stale, gone} {
			if _, err := st.GetAttachment(ctx, a.ID); !errors.Is(err, ErrNotFound) {
				t.Errorf("%s %s not retired (%v)", a.ScopeKind, a.ScopeRef, err)
			}
		}
		for _, a := range []*Artifact{fresh, live, unanswered} {
			if _, err := st.GetAttachment(ctx, a.ID); err != nil {
				t.Errorf("%s %s retired: %v", a.ScopeKind, a.ScopeRef, err)
			}
		}
		for _, refs := range checker.asked {
			for _, r := range refs {
				if _, ok := ParseMessageScopeRef(r); !ok {
					t.Errorf("checker asked about %q, not a message ref", r)
				}
			}
		}
		if res, err := RetireAttachments(ctx, st, nil, now); err != nil || res.Orphaned != 0 {
			t.Errorf("without a checker = %+v, %v", res, err)
		}
	})
}

// staleStore returns attachment rows from GetArtifact as if they were
// project artifacts, standing in for a row that became an attachment after
// a route read it: the store's own refusal under the lock must then hold.
type staleStore struct{ Store }

func (s staleStore) GetArtifact(ctx context.Context, id string) (*Artifact, error) {
	a, err := s.Store.GetAttachment(ctx, id)
	if err != nil {
		return s.Store.GetArtifact(ctx, id)
	}
	c := *a
	c.ScopeKind, c.ScopeRef = ScopeKindProject, "project-1"
	return &c, nil
}

// TestAttachmentRefusedUnderLockAnswers409: when a share or change reaches
// the store for an attachment, the store refuses it under the lock and the
// route answers 409 attachment_not_shareable, writing nothing.
func TestAttachmentRefusedUnderLockAnswers409(t *testing.T) {
	f := newAttachmentFixture(t, false)
	id := f.send(userU, "msg-1", pngBytes)
	f.svc.SetStore(staleStore{f.store})

	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/artifacts/" + id + "/grants", `{"subjectKind":"principal","subjectRef":"user:user-2","permission":"read"}`},
		{http.MethodPost, "/api/v1/artifacts/" + id + "/links", `{}`},
		{http.MethodPatch, "/api/v1/artifacts/" + id, `{"expiresAt":"2099-01-01T00:00:00Z"}`},
	} {
		rec := f.do(&userU, c.method, c.path, []byte(c.body), map[string]string{"Content-Type": "application/json"})
		if rec.Code != http.StatusConflict || errCode(t, rec) != CodeAttachmentNotShareable {
			t.Errorf("%s %s = %d %s; want 409 %s", c.method, c.path, rec.Code, rec.Body.String(), CodeAttachmentNotShareable)
		}
	}
	grants, err := f.store.ListGrants(context.Background(), id)
	if err != nil || len(grants) != 0 {
		t.Errorf("grants after refusals = %+v, %v", grants, err)
	}
	a, err := f.store.GetAttachment(context.Background(), id)
	if err != nil || a.ExpiresAt != nil || a.ScopeKind != ScopeKindMessage {
		t.Errorf("attachment after refusals = %+v, %v", a, err)
	}
}
