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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Broker plugin attachments: the check and upload routes and attachment_ids
// on the legacy broker inbound route.

// brokerAttFile is one file part of a broker upload.
type brokerAttFile struct {
	name, content string
}

// brokerAttRequest serves a request on path through the hub's mux with a
// broker identity in context (the HMAC middleware is exercised by its own
// tests).
func brokerAttRequest(t *testing.T, srv *Server, path, contentType string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, body)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Scion-Plugin-Name", "test-plugin")
	req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("test-broker")))
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	return rec
}

// brokerCheck posts a check for sender, projectID and targets.
func brokerCheck(t *testing.T, srv *Server, sender, projectID string, targets []string) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(map[string]interface{}{
		"sender": sender, "project_id": projectID, "targets": targets, "surface": "telegram",
	})
	require.NoError(t, err)
	return brokerAttRequest(t, srv, brokerAttachmentsCheckPath, "application/json", bytes.NewReader(raw))
}

// brokerCheckAnswer decodes a 200 check answer.
func brokerCheckAnswer(t *testing.T, rec *httptest.ResponseRecorder) brokerAttachmentCheckResponse {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got brokerAttachmentCheckResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	return got
}

// brokerUploadBody builds an upload body with the metadata fields in the
// required order, then the files.
func brokerUploadBody(t *testing.T, sender, projectID string, targets []string, files []brokerAttFile) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, f := range [][2]string{{"sender", sender}, {"project_id", projectID}, {"surface", "telegram"}, {"audience", "group"}} {
		require.NoError(t, mw.WriteField(f[0], f[1]))
	}
	for _, slug := range targets {
		require.NoError(t, mw.WriteField("targets", slug))
	}
	for _, f := range files {
		part, err := mw.CreateFormFile("file", f.name)
		require.NoError(t, err)
		_, err = part.Write([]byte(f.content))
		require.NoError(t, err)
	}
	require.NoError(t, mw.Close())
	return &body, mw.FormDataContentType()
}

// brokerUpload posts an upload for sender.
func brokerUpload(t *testing.T, srv *Server, sender, projectID string, targets []string, files ...brokerAttFile) *httptest.ResponseRecorder {
	t.Helper()
	body, ct := brokerUploadBody(t, sender, projectID, targets, files)
	return brokerAttRequest(t, srv, brokerAttachmentsUploadPath, ct, body)
}

// brokerUploadedIDs requires a 201 answer with no failures and returns the
// stored ids.
func brokerUploadedIDs(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp struct {
		Attachments []attachmentUploadResult  `json:"attachments"`
		Failures    []attachmentUploadFailure `json:"failures"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Empty(t, resp.Failures)
	ids := make([]string, 0, len(resp.Attachments))
	for _, a := range resp.Attachments {
		ids = append(ids, a.ID)
	}
	return ids
}

// requireNoUploadsAtAll asserts that the artifact store holds no upload of
// any principal.
func requireNoUploadsAtAll(t *testing.T, st artifacts.Store) {
	t.Helper()
	refs, err := st.ListLiveScopeRefs(context.Background(), artifacts.ScopeKindUpload, "", 1000)
	require.NoError(t, err)
	require.Empty(t, refs, "an upload was stored")
}

// brokerInbound posts a legacy broker inbound message from sender to agent
// a, naming ids.
func brokerInbound(t *testing.T, srv *Server, sender string, a *store.Agent, ids []string) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(inboundMessageRequest{
		Topic: "scion.project." + a.ProjectID + ".agent." + a.Slug + ".messages",
		Message: &messages.StructuredMessage{
			Version: messages.Version, Timestamp: time.Now().UTC().Format(time.RFC3339),
			Channel: "telegram", Sender: sender, Recipient: "agent:" + a.Slug,
			Msg: "see the file", Type: messages.TypeInstruction,
		},
		AttachmentIDs: ids,
	})
	require.NoError(t, err)
	return brokerAttRequest(t, srv, "/api/v1/broker/inbound", "application/json", bytes.NewReader(raw))
}

// brokerInboundRouted posts a routed broker inbound message from sender
// with agent a as the default agent.
func brokerInboundRouted(t *testing.T, srv *Server, sender string, a *store.Agent) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(routedInboundRequest{
		ProjectID: a.ProjectID, DefaultAgent: a.Slug,
		Message: &messages.StructuredMessage{Version: messages.Version, Channel: "telegram", Sender: sender,
			Msg: "hello", Type: messages.TypeInstruction},
	})
	require.NoError(t, err)
	return brokerAttRequest(t, srv, "/api/v1/broker/inbound/routed", "application/json", bytes.NewReader(raw))
}

// newBrokerAttFixture is a sendFixture (artifacts on, agents snd and tgt
// of project A owned by ua) with an inactive user.
func newBrokerAttFixture(t *testing.T) (*sendFixture, *store.User) {
	t.Helper()
	f := newSendFixture(t)
	inactive := &store.User{ID: uuid.NewString(), Email: "inactive@test.com", DisplayName: "Inactive",
		Role: store.UserRoleMember, Status: "suspended", Created: time.Now()}
	require.NoError(t, f.st.CreateUser(context.Background(), inactive))
	ensureHubMembership(context.Background(), f.st, inactive.ID)
	msgAuthzAddProjectMember(t, f.st, inactive.ID, f.projA.ID, f.projA.Slug, store.GroupMemberRoleMember)
	return f, inactive
}

// TestBrokerAttachmentCheck_ModeAnswers: the check answers "artifact" with
// the delivery limit when the experiment is on and the artifact service is
// ready, "legacy" while the experiment is registered but off, and
// "unavailable" once it is not registered; it writes nothing.
func TestBrokerAttachmentCheck_ModeAnswers(t *testing.T) {
	f := newSendFixture(t)
	sender := "user:" + f.ua.Email

	got := brokerCheckAnswer(t, brokerCheck(t, f.srv, sender, f.projA.ID, []string{f.target.Slug}))
	assert.True(t, got.Authorized)
	assert.Equal(t, brokerUploadsArtifact, got.Uploads)
	require.NotNil(t, got.DeliveryMaxBytes)
	assert.Equal(t, f.srv.brokerAttachmentDeliveryMaxBytes(), *got.DeliveryMaxBytes)

	setAttachmentExperiments(t, f.srv, false, false)
	got = brokerCheckAnswer(t, brokerCheck(t, f.srv, sender, f.projA.ID, []string{f.target.Slug}))
	assert.True(t, got.Authorized)
	assert.Equal(t, brokerUploadsLegacy, got.Uploads)
	assert.Nil(t, got.DeliveryMaxBytes)

	var active []experiments.Experiment
	for _, e := range experiments.Default().All() {
		if e.Name != experiments.AttachmentArtifacts {
			active = append(active, e)
		}
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	f.srv.experiments = reg
	got = brokerCheckAnswer(t, brokerCheck(t, f.srv, sender, f.projA.ID, nil))
	assert.True(t, got.Authorized)
	assert.Equal(t, brokerUploadsUnavailable, got.Uploads)
	assert.Nil(t, got.DeliveryMaxBytes)

	requireNoUploadsAtAll(t, f.art)
}

// TestBrokerAttachmentDeliveryMaxBytes_ClampedToSignedBodyCap: the check's
// delivery_max_bytes is the delivery budget clamped to the signed-body cap
// less the framing allowance, and the upload enforces the same cap.
func TestBrokerAttachmentDeliveryMaxBytes_ClampedToSignedBodyCap(t *testing.T) {
	f := newSendFixture(t)
	capped := apiclient.MaxSignedRequestBodyBytes - brokerAttachmentEnvelopeBytes
	require.Equal(t, apiclient.MaxSignedRequestBodyBytes, brokerAttachmentBodyCap())

	setDeliveryBudget(t, f.srv, 64<<20)
	got := brokerCheckAnswer(t, brokerCheck(t, f.srv, "user:"+f.ua.Email, f.projA.ID, nil))
	require.NotNil(t, got.DeliveryMaxBytes)
	assert.Equal(t, capped, *got.DeliveryMaxBytes, "a budget above the cap is clamped")

	setDeliveryBudget(t, f.srv, 1000)
	got = brokerCheckAnswer(t, brokerCheck(t, f.srv, "user:"+f.ua.Email, f.projA.ID, nil))
	require.NotNil(t, got.DeliveryMaxBytes)
	assert.Equal(t, int64(1000), *got.DeliveryMaxBytes, "a budget below the cap is kept")

	setDeliveryBudget(t, f.srv, 0)
	got = brokerCheckAnswer(t, brokerCheck(t, f.srv, "user:"+f.ua.Email, f.projA.ID, nil))
	require.NotNil(t, got.DeliveryMaxBytes)
	assert.Equal(t, int64(0), *got.DeliveryMaxBytes, "delivery off")
}

// TestBrokerAttachmentUpload_BodyOverCapRefused: a client that ignores
// delivery_max_bytes and sends a file over the limit gets 413 and nothing is
// stored. With the length declared, the body cap refuses the request before
// any read; without it, the file is refused while it is read, because it
// passes the total for one message before the body reaches the cap
// (TestBrokerAttachmentUpload_BodyCapWhileReading exercises the cap itself).
func TestBrokerAttachmentUpload_BodyOverCapRefused(t *testing.T) {
	f := newSendFixture(t)
	big := strings.Repeat("a", int(brokerAttachmentBodyCap()))
	body, ct := brokerUploadBody(t, "user:"+f.ua.Email, f.projA.ID, nil, []brokerAttFile{{"big.txt", big}})
	raw := body.Bytes()

	declared := brokerAttRequest(t, f.srv, brokerAttachmentsUploadPath, ct, bytes.NewReader(raw))
	assert.Equal(t, http.StatusRequestEntityTooLarge, declared.Code, declared.Body.String())
	requireNoUploadsAtAll(t, f.art)

	req := httptest.NewRequest(http.MethodPost, brokerAttachmentsUploadPath, io.NopCloser(bytes.NewReader(raw)))
	req.ContentLength = -1
	req.Header.Set("Content-Type", ct)
	req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("test-broker")))
	rec := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
	requireNoUploadsAtAll(t, f.art)
}

// TestBrokerAttachmentCheck_NeverNotFound: the check answers 200
// authorized false for an unknown user, project or target, 400 with the
// error envelope for a malformed request, and never 404.
func TestBrokerAttachmentCheck_NeverNotFound(t *testing.T) {
	f := newSendFixture(t)
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"unknown user":    brokerCheck(t, f.srv, "user:nobody@test.com", f.projA.ID, nil),
		"unknown project": brokerCheck(t, f.srv, "user:"+f.ua.Email, uuid.NewString(), nil),
		"unknown target":  brokerCheck(t, f.srv, "user:"+f.ua.Email, f.projA.ID, []string{"no-such-agent"}),
	} {
		got := brokerCheckAnswer(t, rec)
		assert.False(t, got.Authorized, name)
	}
	for _, path := range []string{brokerAttachmentsCheckPath, brokerAttachmentsUploadPath} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("test-broker")))
		rec := httptest.NewRecorder()
		f.srv.mux.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, path)
	}
	for name, body := range map[string]string{
		"not json":      "{",
		"no sender":     `{"project_id":"p","surface":"telegram"}`,
		"agent sender":  `{"sender":"agent:x","project_id":"p","surface":"telegram"}`,
		"no project":    `{"sender":"user:a@test.com","surface":"telegram"}`,
		"bad surface":   `{"sender":"user:a@test.com","project_id":"p","surface":"web"}`,
		"empty targets": `{"sender":"user:a@test.com","project_id":"p","surface":"telegram","targets":[""]}`,
	} {
		rec := brokerAttRequest(t, f.srv, brokerAttachmentsCheckPath, "application/json", strings.NewReader(body))
		require.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", name, rec.Body.String())
		var env struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env), name)
		assert.NotEmpty(t, env.Error.Code, name)
	}
}

// TestBrokerAttachments_RefusedSenders: an inactive user, a test
// identity and a sender authorizeAgentMessage refuses for a named target
// get authorized false from the check, 403 from the upload, and nothing is
// stored; in legacy mode the check refuses them too.
func TestBrokerAttachments_RefusedSenders(t *testing.T) {
	f, inactive := newBrokerAttFixture(t)
	refused := map[string]string{
		"inactive":      "user:" + inactive.Email,
		"test identity": "user:nobody@" + store.TestFixtureEmailDomain,
		"message deny":  "user:" + f.ub.Email,
	}
	for name, sender := range refused {
		got := brokerCheckAnswer(t, brokerCheck(t, f.srv, sender, f.projA.ID, []string{f.target.Slug}))
		assert.False(t, got.Authorized, name)
		assert.Equal(t, brokerUploadsArtifact, got.Uploads, name)

		rec := brokerUpload(t, f.srv, sender, f.projA.ID, []string{f.target.Slug}, brokerAttFile{"a.txt", "hello"})
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", name, rec.Body.String())
	}
	requireNoUploadsAtAll(t, f.art)

	setAttachmentExperiments(t, f.srv, false, false)
	for name, sender := range refused {
		got := brokerCheckAnswer(t, brokerCheck(t, f.srv, sender, f.projA.ID, []string{f.target.Slug}))
		assert.False(t, got.Authorized, "legacy "+name)
		assert.Equal(t, brokerUploadsLegacy, got.Uploads, name)
	}
}

// TestBrokerAttachments_NoProjectReadRefused: without targets, a
// sender without read on the project is refused by the check and the
// upload, in artifact and legacy mode, and stores nothing; a project member
// is authorized.
func TestBrokerAttachments_NoProjectReadRefused(t *testing.T) {
	f := newSendFixture(t)
	for _, u := range []*store.User{f.ub, f.uc} {
		got := brokerCheckAnswer(t, brokerCheck(t, f.srv, "user:"+u.Email, f.projA.ID, nil))
		assert.False(t, got.Authorized, u.Email)
		rec := brokerUpload(t, f.srv, "user:"+u.Email, f.projA.ID, nil, brokerAttFile{"a.txt", "hello"})
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		requireNoUploads(t, f.art, artifacts.PrincipalKindUser, u.ID)
	}
	requireNoUploadsAtAll(t, f.art)
	assert.True(t, brokerCheckAnswer(t, brokerCheck(t, f.srv, "user:"+f.ua.Email, f.projA.ID, nil)).Authorized)

	setAttachmentExperiments(t, f.srv, false, false)
	got := brokerCheckAnswer(t, brokerCheck(t, f.srv, "user:"+f.ub.Email, f.projA.ID, nil))
	assert.False(t, got.Authorized)
	assert.Equal(t, brokerUploadsLegacy, got.Uploads)
	rec := brokerUpload(t, f.srv, "user:"+f.ub.Email, f.projA.ID, nil, brokerAttFile{"a.txt", "hello"})
	assert.Equal(t, http.StatusForbidden, rec.Code, "the refusal comes before the mode answer: %s", rec.Body.String())
}

// TestBrokerAttachmentUpload_StoredAsSenderUpload: an authorized upload is
// stored as an unsent upload owned by the sender user, not the broker.
func TestBrokerAttachmentUpload_StoredAsSenderUpload(t *testing.T) {
	f := newSendFixture(t)
	ids := brokerUploadedIDs(t, brokerUpload(t, f.srv, "user:"+f.ua.Email, f.projA.ID, []string{f.target.Slug},
		brokerAttFile{"notes.txt", "hello"}, brokerAttFile{"more.txt", "world"}))
	require.Len(t, ids, 2)
	for _, id := range ids {
		info, state, err := f.srv.attachmentService().UsableAttachment(context.Background(), id, artifacts.PrincipalKindUser, f.ua.ID)
		require.NoError(t, err)
		assert.Equal(t, artifacts.AttachmentUsable, state)
		assert.Equal(t, id, info.ID)
	}
}

// TestBrokerAttachmentUpload_ExperimentOff409: with uploads not stored as
// artifacts, an authorized upload answers 409 attachments_unavailable and
// stores nothing.
func TestBrokerAttachmentUpload_ExperimentOff409(t *testing.T) {
	f := newSendFixture(t)
	setAttachmentExperiments(t, f.srv, false, false)
	rec := brokerUpload(t, f.srv, "user:"+f.ua.Email, f.projA.ID, nil, brokerAttFile{"a.txt", "hello"})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), codeAttachmentsUnavailable)
	requireNoUploadsAtAll(t, f.art)
}

// TestBrokerAttachmentUpload_FileBeforeMetadata400: a file part before the
// metadata is complete, missing metadata, and fields out of order are 400
// and store nothing.
func TestBrokerAttachmentUpload_FileBeforeMetadata400(t *testing.T) {
	f := newSendFixture(t)
	build := func(write func(mw *multipart.Writer)) (*bytes.Buffer, string) {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		write(mw)
		require.NoError(t, mw.Close())
		return &body, mw.FormDataContentType()
	}
	file := func(mw *multipart.Writer) {
		part, err := mw.CreateFormFile("file", "a.txt")
		require.NoError(t, err)
		_, _ = part.Write([]byte("hello"))
	}
	cases := map[string]func(mw *multipart.Writer){
		"file first": func(mw *multipart.Writer) {
			file(mw)
			_ = mw.WriteField("sender", "user:"+f.ua.Email)
		},
		"file after sender": func(mw *multipart.Writer) {
			_ = mw.WriteField("sender", "user:"+f.ua.Email)
			file(mw)
		},
		"no metadata no files": func(mw *multipart.Writer) {},
		"out of order": func(mw *multipart.Writer) {
			_ = mw.WriteField("project_id", f.projA.ID)
			_ = mw.WriteField("sender", "user:"+f.ua.Email)
			_ = mw.WriteField("surface", "telegram")
			_ = mw.WriteField("audience", "group")
			file(mw)
		},
		"unknown field": func(mw *multipart.Writer) {
			_ = mw.WriteField("sender", "user:"+f.ua.Email)
			_ = mw.WriteField("project_id", f.projA.ID)
			_ = mw.WriteField("surface", "telegram")
			_ = mw.WriteField("audience", "group")
			_ = mw.WriteField("path", "/etc/passwd")
			file(mw)
		},
		"bad audience": func(mw *multipart.Writer) {
			_ = mw.WriteField("sender", "user:"+f.ua.Email)
			_ = mw.WriteField("project_id", f.projA.ID)
			_ = mw.WriteField("surface", "telegram")
			_ = mw.WriteField("audience", "everyone")
			file(mw)
		},
	}
	for name, write := range cases {
		body, ct := build(write)
		rec := brokerAttRequest(t, f.srv, brokerAttachmentsUploadPath, ct, body)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", name, rec.Body.String())
	}
	requireNoUploadsAtAll(t, f.art)

	// Metadata only, no file: 400 too.
	body, ct := brokerUploadBody(t, "user:"+f.ua.Email, f.projA.ID, nil, nil)
	rec := brokerAttRequest(t, f.srv, brokerAttachmentsUploadPath, ct, body)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

// errFileContentRead is returned by poisonedBody once the handler asks for
// bytes past the first file part's header.
var errFileContentRead = errors.New("file content was read")

// poisonedBody serves prefix on its first read and fails every later read,
// recording that it was asked for more.
type poisonedBody struct {
	prefix []byte
	served bool
	poked  bool
}

func (p *poisonedBody) Read(b []byte) (int, error) {
	if !p.served {
		p.served = true
		return copy(b, p.prefix), nil
	}
	p.poked = true
	return 0, errFileContentRead
}

// TestBrokerAttachmentUpload_RefusedSenderFileNeverRead: for a refused
// sender the handler answers 403 without reading past the first file
// part's header. (The broker HMAC middleware buffers and hashes the whole
// body before any handler runs; this pins the handler's own order.)
func TestBrokerAttachmentUpload_RefusedSenderFileNeverRead(t *testing.T) {
	f := newSendFixture(t)
	body, ct := brokerUploadBody(t, "user:"+f.ub.Email, f.projA.ID, []string{f.target.Slug},
		[]brokerAttFile{{"a.txt", strings.Repeat("x", 64<<10)}})
	raw := body.Bytes()
	headerEnd := bytes.Index(raw, []byte("filename=\"a.txt\""))
	require.Positive(t, headerEnd)
	headerEnd += bytes.Index(raw[headerEnd:], []byte("\r\n\r\n")) + len("\r\n\r\n")
	poison := &poisonedBody{prefix: raw[:headerEnd]}

	req := httptest.NewRequest(http.MethodPost, brokerAttachmentsUploadPath, poison)
	req.ContentLength = int64(len(raw))
	req.Header.Set("Content-Type", ct)
	req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("test-broker")))
	rec := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.False(t, poison.poked, "the handler read past the first file part's header")
	requireNoUploadsAtAll(t, f.art)
}

// TestBrokerAttachmentUpload_FileChecks: HTML and SVG files,
// files over the total budget, and two files under one name or one
// delivered name are refused and not stored.
func TestBrokerAttachmentUpload_FileChecks(t *testing.T) {
	f := newSendFixture(t)
	sender := "user:" + f.ua.Email

	failures := func(rec *httptest.ResponseRecorder) (stored int, failed []string) {
		t.Helper()
		var resp struct {
			Attachments []attachmentUploadResult  `json:"attachments"`
			Failures    []attachmentUploadFailure `json:"failures"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
		for _, fl := range resp.Failures {
			failed = append(failed, fl.Name+": "+fl.Error)
		}
		return len(resp.Attachments), failed
	}

	for _, file := range []brokerAttFile{
		{"page.html", "<p>hi</p>"},
		{"icon.svg", "<svg></svg>"},
		{"notes.txt", "<svg xmlns=\"http://www.w3.org/2000/svg\"></svg>"},
		{"data.bin", "<html><body>sniffed by content</body></html>"},
	} {
		rec := brokerUpload(t, f.srv, sender, f.projA.ID, nil, file)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", file.name, rec.Body.String())
		stored, failed := failures(rec)
		assert.Zero(t, stored, file.name)
		assert.Len(t, failed, 1, file.name)
	}
	requireNoUploadsAtAll(t, f.art)

	rec := brokerUpload(t, f.srv, sender, f.projA.ID, nil,
		brokerAttFile{"Report.txt", "one"}, brokerAttFile{"report.txt", "two"}, brokerAttFile{"a b.txt", "three"}, brokerAttFile{"a_b.txt", "four"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	stored, failed := failures(rec)
	assert.Equal(t, 2, stored)
	require.Len(t, failed, 2)
	assert.Contains(t, failed[0], "report.txt")
	assert.Contains(t, failed[1], "a_b.txt")

	f2 := newSendFixture(t)
	setDeliveryBudget(t, f2.srv, 10)
	rec = brokerUpload(t, f2.srv, "user:"+f2.ua.Email, f2.projA.ID, nil, brokerAttFile{"big.txt", "0123456789AB"})
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "total of 10 bytes")
	requireNoUploadsAtAll(t, f2.art)
}

// TestBrokerInbound_AttachmentIDsDelivered: ids uploaded on the broker
// route and named on the legacy inbound route are bound to the stored
// message and planned for the recipient's inbox, with no scratchpad
// projection: a plugin conversation is not a project audience.
func TestBrokerInbound_AttachmentIDsDelivered(t *testing.T) {
	f := newSendFixture(t)
	f.enableBrokerDelivery(t)
	sender := "user:" + f.ua.Email
	ids := brokerUploadedIDs(t, brokerUpload(t, f.srv, sender, f.projA.ID, []string{f.target.Slug}, brokerAttFile{"notes.txt", "hello"}))

	rec := brokerInbound(t, f.srv, sender, f.target, ids)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	calls := f.disp.getCalls()
	require.Len(t, calls, 1)
	sm := calls[0].StructuredMessage
	require.Len(t, sm.AttachmentPlan, 1)
	entry := sm.AttachmentPlan[0]
	assert.Equal(t, messages.AttachmentDeliverable, entry.State)
	assert.Equal(t, ids[0], entry.Delivery.ArtifactID)
	assert.True(t, strings.HasPrefix(entry.InboxPath, attachmentInboxDir), entry.InboxPath)
	assert.Empty(t, entry.ProjectedPath, "no scratchpad projection for a plugin conversation")
	assert.Empty(t, entry.ProjectedMount)

	// The id is now bound to the stored message, which was persisted.
	_, state, err := f.srv.attachmentService().UsableAttachment(context.Background(), ids[0], artifacts.PrincipalKindUser, f.ua.ID)
	require.NoError(t, err)
	assert.Equal(t, artifacts.AttachmentSentBySender, state)
	stored, err := f.st.GetMessage(context.Background(), entry.Delivery.MessageID)
	require.NoError(t, err)
	assert.Equal(t, f.target.ID, stored.AgentID)

	// At the classifier: a plugin conversation key is never a project
	// audience.
	for _, key := range []string{"telegram:chat:-100123", stored.ConversationID, stored.ThreadID} {
		assert.False(t, f.srv.projectionAudienceIsProject(context.Background(), key, f.projA.ID), "key %q", key)
	}
}

// TestBrokerInbound_AttachmentIDsRefused: another user's upload, an
// upload already sent, unknown ids and ids from a non-user sender are
// refused, and nothing is dispatched.
func TestBrokerInbound_AttachmentIDsRefused(t *testing.T) {
	f := newSendFixture(t)
	msgAuthzAddProjectMember(t, f.st, f.ub.ID, f.projA.ID, f.projA.Slug, store.GroupMemberRoleMember)
	bobs := brokerUploadedIDs(t, brokerUpload(t, f.srv, "user:"+f.ub.Email, f.projA.ID, nil, brokerAttFile{"bob.txt", "bob"}))
	alices := brokerUploadedIDs(t, brokerUpload(t, f.srv, "user:"+f.ua.Email, f.projA.ID, nil, brokerAttFile{"alice.txt", "alice"}))

	rec := brokerInbound(t, f.srv, "user:"+f.ua.Email, f.target, bobs)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	rec = brokerInbound(t, f.srv, "user:"+f.ua.Email, f.target, []string{uuid.NewString()})
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	rec = brokerInbound(t, f.srv, "agent:"+f.sender.Slug, f.target, alices)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Empty(t, f.disp.getCalls(), "nothing is dispatched for a refused send")

	rec = brokerInbound(t, f.srv, "user:"+f.ua.Email, f.target, alices)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, f.disp.getCalls(), 1)
	rec = brokerInbound(t, f.srv, "user:"+f.ua.Email, f.target, alices)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "an id already sent is refused: %s", rec.Body.String())
	assert.Len(t, f.disp.getCalls(), 1, "nothing more is dispatched")

	// Bob's upload is still his and unsent.
	_, state, err := f.srv.attachmentService().UsableAttachment(context.Background(), bobs[0], artifacts.PrincipalKindUser, f.ub.ID)
	require.NoError(t, err)
	assert.Equal(t, artifacts.AttachmentUsable, state)
}

// TestBrokerInbound_AttachmentDispatchFailureMarksMessage: a message with
// attachments is persisted before dispatch, and a failed dispatch marks it
// failed.
func TestBrokerInbound_AttachmentDispatchFailureMarksMessage(t *testing.T) {
	f := newSendFixture(t)
	sender := "user:" + f.ua.Email
	ids := brokerUploadedIDs(t, brokerUpload(t, f.srv, sender, f.projA.ID, []string{f.target.Slug}, brokerAttFile{"notes.txt", "hello"}))
	f.disp.returnErr = errors.New("broker refused the dispatch")

	rec := brokerInbound(t, f.srv, sender, f.target, ids)
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	calls := f.disp.getCalls()
	require.Len(t, calls, 1)
	msgID := calls[0].StructuredMessage.AttachmentPlan[0].Delivery.MessageID
	stored, err := f.st.GetMessage(context.Background(), msgID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchFailed, stored.DispatchState)
}

// TestBrokerInbound_AttachmentsNotReadableByOtherUsersAgent (artifact
// mode, multi-user): user B's agent in the same project cannot read user
// A's upload by any path: not as an unsent upload, not by naming its id,
// not once it is sent to A's agent, and it is not projected.
func TestBrokerInbound_AttachmentsNotReadableByOtherUsersAgent(t *testing.T) {
	f := newSendFixture(t)
	ctx := context.Background()
	msgAuthzAddProjectMember(t, f.st, f.ub.ID, f.projA.ID, f.projA.Slug, store.GroupMemberRoleMember)
	bAgent := &store.Agent{ID: uuid.NewString(), ProjectID: f.projA.ID, Name: "bee", Slug: "bee", Phase: "running",
		OwnerID: f.ub.ID, CreatedBy: f.ub.ID, RuntimeBrokerID: f.brokerID,
		MessageMode: store.MessageModeProject, Ancestry: []string{f.ub.ID}}
	require.NoError(t, f.st.CreateAgent(ctx, bAgent))
	svc := f.srv.attachmentService()

	ids := brokerUploadedIDs(t, brokerUpload(t, f.srv, "user:"+f.ua.Email, f.projA.ID, []string{f.target.Slug}, brokerAttFile{"secret.txt", "for tgt only"}))
	id := ids[0]

	// Unsent: B's agent may not name it, and it has no by-id read route.
	_, state, err := svc.UsableAttachment(ctx, id, artifacts.PrincipalKindAgent, bAgent.ID)
	require.NoError(t, err)
	assert.NotEqual(t, artifacts.AttachmentUsable, state)
	rec := f.agentRequest(t, bAgent, httptest.NewRequest(http.MethodGet, "/api/v1/chat/attachments/"+id, nil))
	assert.NotEqual(t, http.StatusOK, rec.Code, rec.Body.String())

	// Sent to A's agent: B's agent does not read the message's attachments.
	rec = brokerInbound(t, f.srv, "user:"+f.ua.Email, f.target, ids)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	calls := f.disp.getCalls()
	require.Len(t, calls, 1)
	del := calls[0].StructuredMessage.AttachmentPlan[0].Delivery
	assert.Empty(t, del.ProjectedPath)
	project := f.projA.ID
	assert.False(t, svc.MayReadAttachments(attachmentDecisionForRecipient(bAgent), del.MessageID, &project))
	plan := f.srv.attachmentDelivery().Plan(ctx, bAgent, []messages.AttachmentDelivery{del})
	require.Len(t, plan, 1)
	assert.Equal(t, messages.AttachmentWithheld, plan[0].State)
	rec = f.agentRequest(t, bAgent, httptest.NewRequest(http.MethodGet, "/api/v1/chat/attachments/"+id, nil))
	assert.NotEqual(t, http.StatusOK, rec.Code, rec.Body.String())
}

// TestBrokerInbound_SenderRefusalsUnchanged: the shared sender resolution
// keeps the refusal answers of both broker inbound routes.
func TestBrokerInbound_SenderRefusalsUnchanged(t *testing.T) {
	f, inactive := newBrokerAttFixture(t)
	type want struct {
		code, message string
		details       map[string]interface{}
	}
	cases := map[string]struct {
		sender string
		want   want
	}{
		"unknown": {"user:nobody@test.com", want{ErrCodeForbidden, "sender identity could not be resolved",
			map[string]interface{}{"sender": "user:nobody@test.com"}}},
		"inactive": {"user:" + inactive.Email, want{ErrCodeForbidden, "sender identity is not active",
			map[string]interface{}{"sender": "user:" + inactive.Email, "status": "suspended"}}},
		"test identity": {"user:x@" + store.TestFixtureEmailDomain, want{ErrCodeForbidden, "sender identity not eligible",
			map[string]interface{}{"sender": "user:x@" + store.TestFixtureEmailDomain}}},
	}
	for name, c := range cases {
		for route, rec := range map[string]*httptest.ResponseRecorder{
			"legacy": brokerInbound(t, f.srv, c.sender, f.target, nil),
			"routed": brokerInboundRouted(t, f.srv, c.sender, f.target),
		} {
			require.Equal(t, http.StatusForbidden, rec.Code, "%s %s: %s", route, name, rec.Body.String())
			var env struct {
				Error struct {
					Code    string                 `json:"code"`
					Message string                 `json:"message"`
					Details map[string]interface{} `json:"details"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
			assert.Equal(t, c.want.code, env.Error.Code, "%s %s", route, name)
			assert.Equal(t, c.want.message, env.Error.Message, "%s %s", route, name)
			assert.Equal(t, c.want.details, env.Error.Details, "%s %s", route, name)
		}
	}
	require.Empty(t, f.disp.getCalls())
}

// TestBrokerAttachmentUpload_WholeRequestRefusalsStoreNothing: a refusal of
// the whole request stores none of its files, also those before the
// refused part: a later file over the remaining total (413), more files
// than one message takes (400), a non-file part after a file (400), and,
// without a declared length, a large file after a first one (413; it passes
// the total for one message before the body reaches the cap).
func TestBrokerAttachmentUpload_WholeRequestRefusalsStoreNothing(t *testing.T) {
	f := newSendFixture(t)
	sender := "user:" + f.ua.Email

	setDeliveryBudget(t, f.srv, 10)
	rec := brokerUpload(t, f.srv, sender, f.projA.ID, nil, brokerAttFile{"a.txt", "123456"}, brokerAttFile{"b.txt", "789012"})
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
	requireNoUploadsAtAll(t, f.art)

	f = newSendFixture(t)
	var files []brokerAttFile
	for i := 0; i <= MaxAttachmentsPerMessage; i++ {
		files = append(files, brokerAttFile{fmt.Sprintf("f%02d.txt", i), "x"})
	}
	rec = brokerUpload(t, f.srv, sender, f.projA.ID, nil, files...)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	requireNoUploadsAtAll(t, f.art)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, kv := range [][2]string{{"sender", sender}, {"project_id", f.projA.ID}, {"surface", "telegram"}, {"audience", "group"}} {
		require.NoError(t, mw.WriteField(kv[0], kv[1]))
	}
	part, err := mw.CreateFormFile("file", "a.txt")
	require.NoError(t, err)
	_, err = part.Write([]byte("hello"))
	require.NoError(t, err)
	require.NoError(t, mw.WriteField("targets", f.target.Slug))
	require.NoError(t, mw.Close())
	rec = brokerAttRequest(t, f.srv, brokerAttachmentsUploadPath, mw.FormDataContentType(), &body)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	requireNoUploadsAtAll(t, f.art)

	big, ct := brokerUploadBody(t, sender, f.projA.ID, nil, []brokerAttFile{
		{"first.txt", "small"}, {"big.txt", strings.Repeat("a", int(brokerAttachmentBodyCap()))},
	})
	req := httptest.NewRequest(http.MethodPost, brokerAttachmentsUploadPath, io.NopCloser(bytes.NewReader(big.Bytes())))
	req.ContentLength = -1
	req.Header.Set("Content-Type", ct)
	req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("test-broker")))
	rec = httptest.NewRecorder()
	f.srv.mux.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
	requireNoUploadsAtAll(t, f.art)
}

// TestBrokerAttachments_TargetBound: the check and the upload accept
// maxBrokerAttachmentTargets targets and refuse one more with 400
// too_many_targets, storing nothing.
func TestBrokerAttachments_TargetBound(t *testing.T) {
	f := newSendFixture(t)
	sender := "user:" + f.ua.Email
	targets := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("agent-%03d", i)
		}
		return out
	}
	got := brokerCheckAnswer(t, brokerCheck(t, f.srv, sender, f.projA.ID, targets(maxBrokerAttachmentTargets)))
	assert.False(t, got.Authorized, "unknown slugs are refused, not malformed")

	for name, rec := range map[string]*httptest.ResponseRecorder{
		"check":  brokerCheck(t, f.srv, sender, f.projA.ID, targets(maxBrokerAttachmentTargets+1)),
		"upload": brokerUpload(t, f.srv, sender, f.projA.ID, targets(maxBrokerAttachmentTargets+1), brokerAttFile{"a.txt", "hello"}),
	} {
		require.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", name, rec.Body.String())
		assert.Contains(t, rec.Body.String(), `"code":"`+codeTooManyTargets+`"`, name)
	}
	requireNoUploadsAtAll(t, f.art)
}

// TestBrokerAttachmentCheck_DeliveryMaxBytesHonoursPerFileLimit: the
// check's delivery_max_bytes is the per-file artifact limit when that is
// below the total for one message.
func TestBrokerAttachmentCheck_DeliveryMaxBytesHonoursPerFileLimit(t *testing.T) {
	f := newSendFixture(t)
	f.srv.SetOperationalSettings(artifactsOpsWith(t, `{"enabled":true,"max_file_bytes":1000}`, ""))
	require.Equal(t, int64(1000), f.srv.attachmentMaxBytes(context.Background(), true))
	got := brokerCheckAnswer(t, brokerCheck(t, f.srv, "user:"+f.ua.Email, f.projA.ID, nil))
	require.Equal(t, brokerUploadsArtifact, got.Uploads)
	require.NotNil(t, got.DeliveryMaxBytes)
	assert.Equal(t, int64(1000), *got.DeliveryMaxBytes)
}

// TestBrokerInbound_AttachmentIDsOnBroadcastRefused: attachment ids on a
// broadcast are refused with 400 and nothing is dispatched.
func TestBrokerInbound_AttachmentIDsOnBroadcastRefused(t *testing.T) {
	f := newSendFixture(t)
	sender := "user:" + f.ua.Email
	ids := brokerUploadedIDs(t, brokerUpload(t, f.srv, sender, f.projA.ID, nil, brokerAttFile{"notes.txt", "hello"}))
	raw, err := json.Marshal(inboundMessageRequest{
		Topic: "scion.project." + f.target.ProjectID + ".agent." + f.target.Slug + ".messages",
		Message: &messages.StructuredMessage{
			Version: messages.Version, Timestamp: time.Now().UTC().Format(time.RFC3339),
			Channel: "telegram", Sender: sender, Recipient: "agent:" + f.target.Slug,
			Msg: "to everyone", Type: messages.TypeInstruction, Broadcasted: true,
		},
		AttachmentIDs: ids,
	})
	require.NoError(t, err)
	rec := brokerAttRequest(t, f.srv, "/api/v1/broker/inbound", "application/json", bytes.NewReader(raw))
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Empty(t, f.disp.getCalls())
}

// TestBrokerAttachmentUpload_BodyCapWhileReading: a body that reaches the
// cap while a file is read, with that file under the total for one message,
// is refused with 413 by the body cap itself, and nothing is stored (a first
// small file included). The framing is pushed past the 64 KiB allowance
// with a long file name in the part header: metadata fields cannot carry
// it, because every target must be a real agent (a long unknown slug is
// refused with 403 before any file is read) and the other fields are short.
func TestBrokerAttachmentUpload_BodyCapWhileReading(t *testing.T) {
	f := newSendFixture(t)
	total := f.srv.brokerAttachmentDeliveryMaxBytes()
	longName := strings.Repeat("n", int(brokerAttachmentEnvelopeBytes)+8<<10) + ".txt"
	body, ct := brokerUploadBody(t, "user:"+f.ua.Email, f.projA.ID, nil, []brokerAttFile{
		{"first.txt", "small"},
		{longName, strings.Repeat("a", int(total)-64)},
	})
	require.Greater(t, int64(body.Len()), brokerAttachmentBodyCap(), "the body is over the cap")

	req := httptest.NewRequest(http.MethodPost, brokerAttachmentsUploadPath, io.NopCloser(bytes.NewReader(body.Bytes())))
	req.ContentLength = -1
	req.Header.Set("Content-Type", ct)
	req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("test-broker")))
	rec := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
	requireNoUploadsAtAll(t, f.art)
}

// TestBrokerAttachmentUpload_MetadataBudget: upload metadata (field names
// plus values) up to maxBrokerAttachmentMetadataBytes passes the framing
// check; one byte more is refused with 400 metadata_too_large before any
// sender check, and nothing is stored.
func TestBrokerAttachmentUpload_MetadataBudget(t *testing.T) {
	f := newSendFixture(t)
	sender := "user:" + f.ua.Email
	base := 0
	for _, kv := range [][2]string{{"sender", sender}, {"project_id", f.projA.ID}, {"surface", "telegram"}, {"audience", "group"}} {
		base += len(kv[0]) + len(kv[1])
	}
	// targets fills the rest of the budget with slugs of at most 4000 bytes,
	// plus extra bytes.
	targets := func(extra int) []string {
		remaining := maxBrokerAttachmentMetadataBytes - base + extra
		var out []string
		for remaining > 0 {
			n := min(4000, remaining-len("targets"))
			if n < 1 {
				n = 1
			}
			out = append(out, strings.Repeat("t", n))
			remaining -= len("targets") + n
		}
		return out
	}
	atBudget := targets(0)
	sum := base
	for _, v := range atBudget {
		sum += len("targets") + len(v)
	}
	require.Equal(t, maxBrokerAttachmentMetadataBytes, sum, "the test builds metadata exactly at the budget")

	rec := brokerUpload(t, f.srv, sender, f.projA.ID, atBudget, brokerAttFile{"a.txt", "hello"})
	assert.Equal(t, http.StatusForbidden, rec.Code, "at the budget the request reaches the target check: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), codeMetadataTooLarge)

	rec = brokerUpload(t, f.srv, sender, f.projA.ID, targets(1), brokerAttFile{"a.txt", "hello"})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"code":"`+codeMetadataTooLarge+`"`)
	requireNoUploadsAtAll(t, f.art)
}

// TestBrokerInbound_AttachmentIDsRefusedAfterValidation: valid ids on a
// broker inbound message refused after they were validated (the agent is
// not running) leave no message row and no bind; the upload stays usable.
func TestBrokerInbound_AttachmentIDsRefusedAfterValidation(t *testing.T) {
	f := newSendFixture(t)
	ctx := context.Background()
	stopped := &store.Agent{ID: uuid.NewString(), ProjectID: f.projA.ID, Name: "stopped", Slug: "stopped", Phase: "stopped",
		OwnerID: f.ua.ID, CreatedBy: f.ua.ID, RuntimeBrokerID: f.brokerID,
		MessageMode: store.MessageModeProject, Ancestry: []string{f.ua.ID}}
	require.NoError(t, f.st.CreateAgent(ctx, stopped))
	sender := "user:" + f.ua.Email
	ids := brokerUploadedIDs(t, brokerUpload(t, f.srv, sender, f.projA.ID, []string{stopped.Slug}, brokerAttFile{"notes.txt", "hello"}))

	rec := brokerInbound(t, f.srv, sender, stopped, ids)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Empty(t, f.disp.getCalls())
	msgs, err := f.st.ListMessages(ctx, store.MessageFilter{AgentID: stopped.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, msgs.Items, "no message row")
	_, state, err := f.srv.attachmentService().UsableAttachment(ctx, ids[0], artifacts.PrincipalKindUser, f.ua.ID)
	require.NoError(t, err)
	assert.Equal(t, artifacts.AttachmentUsable, state, "the upload is not bound and stays usable")
}

// TestBrokerAttachments_AuditLinesCarryNoFileNames: the check and upload
// audit lines name the decision, counts and bytes, never a file name.
func TestBrokerAttachments_AuditLinesCarryNoFileNames(t *testing.T) {
	f := newSendFixture(t)
	var buf bytes.Buffer
	f.srv.messageLog = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	sender := "user:" + f.ua.Email
	const marker = "zq-private-report-name"

	brokerCheckAnswer(t, brokerCheck(t, f.srv, sender, f.projA.ID, []string{f.target.Slug}))
	brokerUploadedIDs(t, brokerUpload(t, f.srv, sender, f.projA.ID, []string{f.target.Slug}, brokerAttFile{marker + ".txt", "hello"}))
	rec := brokerUpload(t, f.srv, sender, f.projA.ID, nil, brokerAttFile{marker + ".html", "<p>x</p>"})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	rec = brokerUpload(t, f.srv, "user:"+f.ub.Email, f.projA.ID, nil, brokerAttFile{marker + ".txt", "hello"})
	require.Equal(t, http.StatusForbidden, rec.Code)

	out := buf.String()
	assert.Contains(t, out, `"msg":"broker attachment check"`)
	assert.Contains(t, out, `"msg":"broker attachment upload"`)
	assert.Contains(t, out, `"decision":"stored"`)
	assert.Contains(t, out, `"decision":"refused: files"`)
	assert.NotContains(t, out, marker, "no audit line names a file")
}

// TestRequireNoUploadsAtAll_SeesAStoredUpload: the listing behind
// requireNoUploadsAtAll reports a stored unsent upload, so the "nothing
// stored" assertions cannot pass vacuously.
func TestRequireNoUploadsAtAll_SeesAStoredUpload(t *testing.T) {
	f := newSendFixture(t)
	brokerUploadedIDs(t, brokerUpload(t, f.srv, "user:"+f.ua.Email, f.projA.ID, nil, brokerAttFile{"notes.txt", "hello"}))
	refs, err := f.art.ListLiveScopeRefs(context.Background(), artifacts.ScopeKindUpload, "", 1000)
	require.NoError(t, err)
	assert.Contains(t, refs, artifacts.PrincipalRef(artifacts.PrincipalKindUser, f.ua.ID))
}

// TestBrokerInboundRouted_AttachmentIDsRefused: attachment_ids on the
// routed route are refused with 400 and nothing is dispatched; a request
// without them is handled as before.
func TestBrokerInboundRouted_AttachmentIDsRefused(t *testing.T) {
	f := newSendFixture(t)
	sender := "user:" + f.ua.Email
	ids := brokerUploadedIDs(t, brokerUpload(t, f.srv, sender, f.projA.ID, nil, brokerAttFile{"notes.txt", "hello"}))
	raw, err := json.Marshal(routedInboundRequest{
		ProjectID: f.projA.ID, DefaultAgent: f.target.Slug,
		Message: &messages.StructuredMessage{Version: messages.Version, Channel: "telegram", Sender: sender,
			Msg: "hello", Type: messages.TypeInstruction},
		AttachmentIDs: ids,
	})
	require.NoError(t, err)
	rec := brokerAttRequest(t, f.srv, "/api/v1/broker/inbound/routed", "application/json", bytes.NewReader(raw))
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"code":"`+codeAttachmentsUnsupportedOnRoute+`"`)
	assert.Empty(t, f.disp.getCalls(), "nothing is dispatched")

	rec = brokerInboundRouted(t, f.srv, sender, f.target)
	assert.NotEqual(t, http.StatusBadRequest, rec.Code, "without attachment_ids the route is unchanged: %s", rec.Body.String())
}

// TestResolveBrokerInboundSender_IdentityFields: the resolved broker
// inbound sender is the stored user as an AuthenticatedUser with client
// type "integration", exactly as the broker inbound routes have always
// built it, so checks that read the identity behave the same.
func TestResolveBrokerInboundSender_IdentityFields(t *testing.T) {
	f := newSendFixture(t)
	got, refusal := f.srv.resolveBrokerInboundSender(context.Background(), "user:"+f.ua.Email)
	require.Nil(t, refusal)
	require.NotNil(t, got)
	assert.Equal(t, f.ua.ID, got.ID())
	assert.Equal(t, f.ua.Email, got.Email())
	assert.Equal(t, f.ua.DisplayName, got.DisplayName())
	assert.Equal(t, f.ua.Role, got.Role())
	assert.Equal(t, "user", got.Type())
	assert.Equal(t, "integration", got.ClientType())
}

// brokerSenderContextRefs counts, per enclosing top-level function, the
// identifiers named name (either first-letter case) in files.
func brokerSenderContextRefs(files map[string]*ast.File, name string) map[string]int {
	out := map[string]int{}
	lower := strings.ToLower(name[:1]) + name[1:]
	upper := strings.ToUpper(name[:1]) + name[1:]
	for path, f := range files {
		for _, d := range f.Decls {
			key := path + ":<package scope>"
			var node ast.Node = d
			if fn, ok := d.(*ast.FuncDecl); ok {
				if fn.Body == nil {
					continue
				}
				key = path + ":" + fn.Name.Name
				node = fn.Body
			}
			ast.Inspect(node, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && (id.Name == lower || id.Name == upper) {
					out[key]++
				}
				return true
			})
		}
	}
	return out
}

// TestBrokerSenderContext_SingleIdentitySetter pins the condition behind
// the allow-list entry for broker_inbound_sender.go in
// principalContextSetterFiles: that file names contextWithIdentity exactly
// once, inside brokerSenderContext, and brokerSenderContext is called only
// by the two broker routes, after the sender was resolved and authorized.
func TestBrokerSenderContext_SingleIdentitySetter(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		require.NoError(t, err, p)
		files[p] = f
	}
	require.Contains(t, files, "broker_inbound_sender.go")

	setters := brokerSenderContextRefs(map[string]*ast.File{"broker_inbound_sender.go": files["broker_inbound_sender.go"]}, "contextWithIdentity")
	assert.Equal(t, map[string]int{"broker_inbound_sender.go:brokerSenderContext": 1}, setters,
		"broker_inbound_sender.go must name contextWithIdentity once, in brokerSenderContext")

	callers := brokerSenderContextRefs(files, "brokerSenderContext")
	assert.Equal(t, map[string]int{
		"handlers_broker_inbound_attachments.go:handleBrokerAttachmentsUpload": 1,
		"handlers_broker_inbound.go:handleBrokerInbound":                       1,
	}, callers, "brokerSenderContext must be called only by the two broker routes")

	// Control: a planted second setter and a planted caller are counted.
	planted, err := parser.ParseFile(token.NewFileSet(), "broker_inbound_sender.go",
		"package hub\nfunc plantedSetter(ctx context.Context, u *AuthenticatedUser) { _ = contextWithIdentity(ctx, u); _ = brokerSenderContext(ctx, u) }\n", 0)
	require.NoError(t, err)
	got := brokerSenderContextRefs(map[string]*ast.File{"broker_inbound_sender.go": planted}, "contextWithIdentity")
	assert.Equal(t, map[string]int{"broker_inbound_sender.go:plantedSetter": 1}, got)
	got = brokerSenderContextRefs(map[string]*ast.File{"zz_planted.go": planted}, "brokerSenderContext")
	assert.Equal(t, map[string]int{"zz_planted.go:plantedSetter": 1}, got)
}
