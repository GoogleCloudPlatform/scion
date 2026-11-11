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

package telegram

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
)

// Inbound attachment delivery specs: a Telegram file sent to an agent is
// delivered through hub uploads, and the plugin writes nothing below any
// scratchpad or downloads_path.

const (
	iaSpecProjectSlug = "my-project"
	iaSpecSender      = "user:alice@example.com"
	iaSpecCheckPath   = "/api/v1/broker/inbound/attachments/check"
	iaSpecUploadPath  = "/api/v1/broker/inbound/attachments"
	iaSpecInboundPath = "/api/v1/broker/inbound"
)

// iaSpecMaxBytes is the delivery_max_bytes the fake hub answers by default:
// the hub's clamp, the signed-body limit less its 64 KiB framing allowance.
const iaSpecMaxBytes = apiclient.MaxSignedRequestBodyBytes - 64<<10

// iaSpecMetaOrder is the order of the leading metadata fields the hub
// requires on an upload; any number of targets fields follow.
var iaSpecMetaOrder = []string{"sender", "project_id", "surface", "audience"}

// iaSpecFormPart is one multipart part received by the fake hub.
type iaSpecFormPart struct {
	Name     string
	FileName string
	Data     []byte
}

// iaSpecHubRequest is one request received by the fake hub.
type iaSpecHubRequest struct {
	Seq    int
	Method string
	Path   string
	Header http.Header
	Body   []byte // non-multipart bodies
	Status int

	// Multipart upload requests only.
	Meta           map[string][]string
	Files          []iaSpecFormPart
	FileBeforeMeta bool // a file part arrived before a metadata field
	OutOfOrder     bool // a metadata field arrived out of the required order
}

// iaSpecUpload is an upload the fake hub stored.
type iaSpecUpload struct {
	ID        string
	Owner     string
	ProjectID string
	Name      string
	Data      []byte
}

// iaSpecHub is a fake hub serving the broker routes the plugin calls: the
// agent list, the attachment check, the attachment upload and the inbound
// send. Every request is recorded.
type iaSpecHub struct {
	srv        *httptest.Server
	mode       string // "artifact", "legacy" or "unavailable"
	authorized bool
	agents     []string

	// Optional behaviour, set before the first request.
	maxBytes      int64                    // delivery_max_bytes in artifact mode; 0 = iaSpecMaxBytes
	zeroMaxBytes  bool                     // answer delivery_max_bytes 0 (delivery off)
	checkStatus   int                      // non-zero: the check answers this status
	checkEnvelope bool                     // with checkStatus: answer the JSON error envelope
	checkCode     string                   // with checkEnvelope: its code; "" = "spec_error"
	inboundStatus map[string]int           // inbound send status per message recipient
	inboundDelay  map[string]time.Duration // inbound send delay per message recipient
	uploadRefuse  map[string]int           // upload status per target slug

	mu       sync.Mutex
	seq      int
	requests []iaSpecHubRequest
	uploads  []iaSpecUpload
}

func newIASpecHub(t *testing.T, mode string, authorized bool, agents ...string) *iaSpecHub {
	t.Helper()
	h := &iaSpecHub{mode: mode, authorized: authorized, agents: agents}
	h.srv = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *iaSpecHub) record(rec iaSpecHubRequest) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	rec.Seq = h.seq
	h.requests = append(h.requests, rec)
}

func (h *iaSpecHub) requestsTo(path string) []iaSpecHubRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []iaSpecHubRequest
	for _, r := range h.requests {
		if r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func (h *iaSpecHub) storedUploads() []iaSpecUpload {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]iaSpecUpload(nil), h.uploads...)
}

func iaSpecWriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func iaSpecWriteError(w http.ResponseWriter, status int, code string) {
	iaSpecWriteJSON(w, status, map[string]any{
		"error": map[string]string{"code": code, "message": code},
	})
}

// iaSpecHubConfig is a snapshot of the fake hub's behaviour fields, taken
// under h.mu at the start of each request. Tests change the fields only
// through set, and replace maps rather than mutating them.
type iaSpecHubConfig struct {
	mode, checkCode                         string
	authorized, zeroMaxBytes, checkEnvelope bool
	agents                                  []string
	maxBytes                                int64
	checkStatus                             int
	inboundStatus, uploadRefuse             map[string]int
	inboundDelay                            map[string]time.Duration
}

func (h *iaSpecHub) snapshot() iaSpecHubConfig {
	h.mu.Lock()
	defer h.mu.Unlock()
	return iaSpecHubConfig{
		mode: h.mode, checkCode: h.checkCode,
		authorized: h.authorized, zeroMaxBytes: h.zeroMaxBytes, checkEnvelope: h.checkEnvelope,
		agents: h.agents, maxBytes: h.maxBytes, checkStatus: h.checkStatus,
		inboundStatus: h.inboundStatus, uploadRefuse: h.uploadRefuse, inboundDelay: h.inboundDelay,
	}
}

// set changes the fake hub's behaviour fields under h.mu.
func (h *iaSpecHub) set(change func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	change()
}

func (h *iaSpecHub) serve(w http.ResponseWriter, r *http.Request) {
	c := h.snapshot()
	rec := iaSpecHubRequest{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone()}
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/projects/") && strings.HasSuffix(r.URL.Path, "/agents"):
		agents := make([]map[string]string, 0, len(c.agents))
		for _, slug := range c.agents {
			agents = append(agents, map[string]string{"id": "id-" + slug, "slug": slug, "phase": "running"})
		}
		rec.Status = http.StatusOK
		h.record(rec)
		iaSpecWriteJSON(w, http.StatusOK, map[string]any{"agents": agents})

	case r.Method == http.MethodPost && r.URL.Path == iaSpecCheckPath:
		rec.Body, _ = io.ReadAll(r.Body)
		if c.checkStatus != 0 {
			rec.Status = c.checkStatus
			h.record(rec)
			if c.checkEnvelope {
				code := c.checkCode
				if code == "" {
					code = "spec_error"
				}
				iaSpecWriteError(w, c.checkStatus, code)
			} else {
				http.Error(w, http.StatusText(c.checkStatus), c.checkStatus)
			}
			return
		}
		answer := map[string]any{"authorized": c.authorized, "uploads": c.mode}
		if c.mode == "artifact" {
			answer["delivery_max_bytes"] = int64(iaSpecMaxBytes)
			if c.maxBytes != 0 {
				answer["delivery_max_bytes"] = c.maxBytes
			}
			if c.zeroMaxBytes {
				answer["delivery_max_bytes"] = int64(0)
			}
		}
		rec.Status = http.StatusOK
		h.record(rec)
		iaSpecWriteJSON(w, http.StatusOK, answer)

	case r.Method == http.MethodPost && r.URL.Path == iaSpecUploadPath:
		h.serveUpload(w, r, rec, c)

	case r.Method == http.MethodPost && r.URL.Path == iaSpecInboundPath:
		rec.Body, _ = io.ReadAll(r.Body)
		var send iaSpecInboundSend
		_ = json.Unmarshal(rec.Body, &send)
		if d := c.inboundDelay[send.Message.Recipient]; d > 0 {
			select {
			case <-time.After(d):
			case <-r.Context().Done():
			}
		}
		if status := c.inboundStatus[send.Message.Recipient]; status != 0 {
			rec.Status = status
			h.record(rec)
			iaSpecWriteError(w, status, "spec_refused")
			return
		}
		rec.Status = http.StatusOK
		h.record(rec)
		iaSpecWriteJSON(w, http.StatusOK, map[string]any{})

	default:
		// A route the hub does not register: plain 404, no JSON envelope.
		rec.Body, _ = io.ReadAll(r.Body)
		rec.Status = http.StatusNotFound
		h.record(rec)
		http.NotFound(w, r)
	}
}

// serveUpload stores the file parts of a multipart upload as uploads of the
// sender named in the leading metadata fields.
func (h *iaSpecHub) serveUpload(w http.ResponseWriter, r *http.Request, rec iaSpecHubRequest, c iaSpecHubConfig) {
	rec.Meta = map[string][]string{}
	fail := func(status int, code string) {
		rec.Status = status
		h.record(rec)
		iaSpecWriteError(w, status, code)
	}
	mr, err := r.MultipartReader()
	if err != nil {
		fail(http.StatusBadRequest, "bad_request")
		return
	}
	sawFile := false
	metaFields := 0
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			fail(http.StatusBadRequest, "bad_request")
			return
		}
		data, err := io.ReadAll(p)
		if err != nil {
			fail(http.StatusBadRequest, "bad_request")
			return
		}
		if p.FileName() != "" {
			if len(rec.Meta) == 0 {
				rec.FileBeforeMeta = true
			}
			sawFile = true
			rec.Files = append(rec.Files, iaSpecFormPart{Name: p.FormName(), FileName: p.FileName(), Data: data})
			continue
		}
		if sawFile {
			rec.FileBeforeMeta = true
		}
		if n := metaFields; n < len(iaSpecMetaOrder) {
			if p.FormName() != iaSpecMetaOrder[n] {
				rec.OutOfOrder = true
			}
		} else if p.FormName() != "targets" {
			rec.OutOfOrder = true
		}
		metaFields++
		rec.Meta[p.FormName()] = append(rec.Meta[p.FormName()], string(data))
	}
	first := func(name string) string {
		if v := rec.Meta[name]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	switch {
	case rec.FileBeforeMeta, rec.OutOfOrder:
		fail(http.StatusBadRequest, "bad_request")
		return
	case first("sender") == "" || first("project_id") == "" || first("surface") == "" || first("audience") == "":
		fail(http.StatusBadRequest, "validation_error")
		return
	case !iaSpecFilePartsNamedFile(rec.Files):
		fail(http.StatusBadRequest, "validation_error")
		return
	case !c.authorized:
		fail(http.StatusForbidden, "forbidden")
		return
	case c.mode != "artifact":
		fail(http.StatusConflict, "attachments_unavailable")
		return
	case len(rec.Files) == 0:
		fail(http.StatusBadRequest, "validation_error")
		return
	}
	for _, target := range rec.Meta["targets"] {
		if status := c.uploadRefuse[target]; status != 0 {
			fail(status, "message_denied")
			return
		}
	}
	// As the hub does, markup files are refused per file, with the hub's
	// wording; a request in which no file is stored answers 400 with the
	// failures.
	var refused []map[string]any
	var kept []iaSpecFormPart
	for _, f := range rec.Files {
		switch ext := strings.ToLower(filepath.Ext(f.FileName)); ext {
		case ".html", ".htm", ".svg":
			refused = append(refused, map[string]any{"name": f.FileName, "error": fmt.Sprintf("files with a %s extension are not accepted", ext)})
			continue
		}
		kept = append(kept, f)
	}
	if len(kept) == 0 {
		rec.Status = http.StatusBadRequest
		h.record(rec)
		iaSpecWriteJSON(w, http.StatusBadRequest, map[string]any{"attachments": []any{}, "failures": refused})
		return
	}
	rec.Files = kept

	results := make([]map[string]any, 0, len(rec.Files))
	h.mu.Lock()
	for _, f := range rec.Files {
		id := fmt.Sprintf("upl-%d", len(h.uploads)+1)
		h.uploads = append(h.uploads, iaSpecUpload{
			ID: id, Owner: first("sender"), ProjectID: first("project_id"), Name: f.FileName, Data: f.Data,
		})
		results = append(results, map[string]any{"id": id, "name": f.FileName, "size": len(f.Data)})
	}
	h.mu.Unlock()
	rec.Status = http.StatusCreated
	h.record(rec)
	iaSpecWriteJSON(w, http.StatusCreated, map[string]any{"attachments": results, "failures": []any{}})
}

// iaSpecFilePartsNamedFile reports whether every file part is named "file".
func iaSpecFilePartsNamedFile(files []iaSpecFormPart) bool {
	for _, f := range files {
		if f.Name != "file" {
			return false
		}
	}
	return true
}

// iaSpecTelegram fronts the fake Telegram server: it serves getFile and
// the file download path itself, counting each request, and forwards
// every other Bot API call to the fake server.
type iaSpecTelegram struct {
	inner    *fakeTGServerV2
	srv      *httptest.Server
	content  []byte
	hideSize bool // getFile reports no file size

	mu            sync.Mutex
	getFileCalls  int
	downloadCalls int
}

func newIASpecTelegram(t *testing.T) *iaSpecTelegram {
	t.Helper()
	tg := &iaSpecTelegram{
		inner:   newFakeTGServerV2(t),
		content: []byte(fmt.Sprintf("inbound-attachment-spec-%d", time.Now().UnixNano())),
	}
	innerURL, err := url.Parse(tg.inner.srv.URL)
	require.NoError(t, err)
	forward := httputil.NewSingleHostReverseProxy(innerURL)
	tg.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/bottest-token/getFile":
			tg.mu.Lock()
			tg.getFileCalls++
			hideSize := tg.hideSize
			tg.mu.Unlock()
			fileID := r.URL.Query().Get("file_id")
			size := int64(len(tg.content))
			if hideSize {
				size = 0
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(apiResponse{
				OK: true,
				Result: mustJSONRawV2(t, TGFile{
					FileID:   fileID,
					FileSize: size,
					FilePath: "documents/" + fileID,
				}),
			})
		case strings.HasPrefix(r.URL.Path, "/file/"):
			tg.mu.Lock()
			tg.downloadCalls++
			tg.mu.Unlock()
			_, _ = w.Write(tg.content)
		default:
			forward.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(tg.srv.Close)
	return tg
}

func (tg *iaSpecTelegram) fileCalls() (getFile, download int) {
	tg.mu.Lock()
	defer tg.mu.Unlock()
	return tg.getFileCalls, tg.downloadCalls
}

// iaSpecEnv is a broker configured against the fake hub and fake Telegram
// server, with a temp HOME (so the project scratchpad resolves below it)
// and a downloads_path in another temp dir.
type iaSpecEnv struct {
	b          *TelegramBrokerV2
	hub        *iaSpecHub
	tg         *iaSpecTelegram
	home       string
	scratchpad string
	downloads  string
}

func newIASpecEnv(t *testing.T, hub *iaSpecHub, withDownloadsPath bool) *iaSpecEnv {
	t.Helper()
	return newIASpecEnvWith(t, hub, withDownloadsPath, nil)
}

// newIASpecEnvWith is newIASpecEnv with a hook that runs before the broker
// is configured, given the scratchpad and downloads_path directories.
func newIASpecEnvWith(t *testing.T, hub *iaSpecHub, withDownloadsPath bool, beforeStart func(scratchpad, downloads string)) *iaSpecEnv {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	stubSharedDirSettings(t, &config.VersionedSettings{})

	env := &iaSpecEnv{
		hub:       hub,
		tg:        newIASpecTelegram(t),
		home:      home,
		downloads: filepath.Join(t.TempDir(), "downloads"),
	}
	res, err := resolveSharedDir(home, iaSpecProjectSlug, sharedDirTestProjectID, projectfs.ScratchpadName)
	require.NoError(t, err)
	env.scratchpad = res.Path
	if beforeStart != nil {
		beforeStart(env.scratchpad, env.downloads)
	}

	cfg := map[string]string{
		"bot_token":    "test-token",
		"api_base_url": env.tg.srv.URL,
		"hub_url":      hub.srv.URL,
		"hmac_key":     "c3BlYy1zaWduaW5nLWtleQ==",
		"broker_id":    "broker-spec",
		"plugin_name":  "telegram",
		"db_path":      filepath.Join(t.TempDir(), "test.db"),
	}
	if withDownloadsPath {
		cfg["downloads_path"] = env.downloads
	}
	env.b = NewV2(slog.Default())
	t.Cleanup(func() { env.b.Close() })
	require.NoError(t, env.b.Configure(cfg))
	return env
}

// linkGroup links chatID to the project with "coder" as default agent.
func (env *iaSpecEnv) linkGroup(t *testing.T, chatID int64, title string) {
	t.Helper()
	require.NoError(t, env.b.store.SaveGroupLink(t.Context(), &GroupLink{
		ChatID:       chatID,
		ChatTitle:    title,
		ProjectID:    sharedDirTestProjectID,
		ProjectSlug:  iaSpecProjectSlug,
		DefaultAgent: "coder",
		LinkedAt:     time.Now().UTC(),
		Active:       true,
	}))
}

func (env *iaSpecEnv) mapUser(t *testing.T, tgUserID, email, scionUserID string) {
	t.Helper()
	require.NoError(t, env.b.store.SaveUserMapping(t.Context(), &TelegramUserMapping{
		TelegramUserID: tgUserID,
		ScionEmail:     email,
		ScionUserID:    scionUserID,
		LinkedAt:       time.Now().UTC(),
	}))
}

// sendDocument delivers a document message from sender in chat through the
// plugin's inbound entry point, addressed to the group's default agent.
func (env *iaSpecEnv) sendDocument(chat TGChat, sender *TGUser) {
	env.b.handleIncomingMessageV2(env.b.gen, &TGMessage{
		MessageID: 77,
		From:      sender,
		Chat:      chat,
		Date:      time.Now().Unix(),
		Caption:   "please review this file",
		Document: &TGDocument{
			FileID:       "spec-doc-1",
			FileUniqueID: "spec-uniq-1",
			FileName:     "notes.txt",
			MimeType:     "text/plain",
			FileSize:     int64(len(env.tg.content)),
		},
	})
}

// iaSpecFilesUnder returns every non-directory entry below root, relative
// to root. A missing root has none.
func iaSpecFilesUnder(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root && errors.Is(err, fs.ErrNotExist) {
				return fs.SkipAll
			}
			return err
		}
		if !d.IsDir() {
			rel, relErr := filepath.Rel(root, p)
			if relErr != nil {
				return relErr
			}
			out = append(out, rel)
		}
		return nil
	})
	require.NoError(t, err)
	return out
}

// iaSpecFilesHolding returns the regular files below root whose content
// contains data.
func iaSpecFilesHolding(t *testing.T, root string, data []byte) []string {
	t.Helper()
	var out []string
	for _, rel := range iaSpecFilesUnder(t, root) {
		p := filepath.Join(root, rel)
		info, err := os.Lstat(p)
		require.NoError(t, err)
		if !info.Mode().IsRegular() {
			continue
		}
		content, err := os.ReadFile(p)
		require.NoError(t, err)
		if bytes.Contains(content, data) {
			out = append(out, rel)
		}
	}
	return out
}

// assertNoAttachmentWrites walks the scratchpad, downloads_path and HOME
// and asserts that the plugin wrote no attachment entry or file bytes.
func (env *iaSpecEnv) assertNoAttachmentWrites(t *testing.T) {
	t.Helper()
	attachmentsRoot := filepath.Join(env.scratchpad, ".attachments")
	telegramEntries, err := os.ReadDir(filepath.Join(attachmentsRoot, projectfs.AttachmentChannelTelegram))
	if err == nil {
		names := make([]string, 0, len(telegramEntries))
		for _, e := range telegramEntries {
			names = append(names, e.Name())
		}
		assert.Empty(t, names, "no entry may exist under the scratchpad's .attachments/%s", projectfs.AttachmentChannelTelegram)
	} else {
		assert.True(t, errors.Is(err, fs.ErrNotExist), "reading .attachments/%s: %v", projectfs.AttachmentChannelTelegram, err)
	}
	assert.Empty(t, iaSpecFilesUnder(t, attachmentsRoot), "no file may exist under the scratchpad's .attachments")
	assert.Empty(t, iaSpecFilesUnder(t, env.downloads), "no file may exist under downloads_path")
	assert.Empty(t, iaSpecFilesHolding(t, env.home, env.tg.content), "no file under HOME may hold the attachment bytes")
	assert.Empty(t, iaSpecFilesHolding(t, env.downloads, env.tg.content), "no file under downloads_path may hold the attachment bytes")
}

// iaSpecInboundSend is the part of a broker inbound request the specs read.
type iaSpecInboundSend struct {
	Topic         string   `json:"topic"`
	AttachmentIDs []string `json:"attachment_ids"`
	Message       struct {
		Sender        string   `json:"sender"`
		Recipient     string   `json:"recipient"`
		Attachments   []string `json:"attachments"`
		AttachmentIDs []string `json:"attachment_ids"`
	} `json:"message"`
}

// assertDeliveredThroughUpload asserts that the attachment went to the
// hub as exactly one upload owned by sender, and that the single inbound
// send names that upload by id and carries no path attachment.
func (env *iaSpecEnv) assertDeliveredThroughUpload(t *testing.T, sender string) {
	t.Helper()
	checks := env.hub.requestsTo(iaSpecCheckPath)
	uploadReqs := env.hub.requestsTo(iaSpecUploadPath)
	sends := env.hub.requestsTo(iaSpecInboundPath)

	require.NotEmpty(t, checks, "the plugin must ask the hub's attachment check before uploading")
	var check map[string]any
	require.NoError(t, json.Unmarshal(checks[0].Body, &check), "check body: %s", checks[0].Body)
	assert.Equal(t, sender, check["sender"], "check sender")
	assert.Equal(t, sharedDirTestProjectID, check["project_id"], "check project_id")
	assert.Equal(t, "telegram", check["surface"], "check surface")
	assert.Equal(t, []any{"coder"}, check["targets"], "check targets name the resolved agent")

	require.Len(t, uploadReqs, 1, "exactly one attachment upload request")
	up := uploadReqs[0]
	assert.Equal(t, http.StatusCreated, up.Status, "upload status")
	assert.False(t, up.FileBeforeMeta, "metadata fields must precede file parts")
	assert.Equal(t, []string{sender}, up.Meta["sender"], "upload sender")
	assert.Equal(t, []string{sharedDirTestProjectID}, up.Meta["project_id"], "upload project_id")
	assert.Equal(t, []string{"telegram"}, up.Meta["surface"], "upload surface")
	assert.Equal(t, []string{"group"}, up.Meta["audience"], "upload audience attestation")
	assert.Equal(t, []string{"coder"}, up.Meta["targets"], "upload targets, one field per slug")
	require.Len(t, up.Files, 1, "one file part")
	assert.Equal(t, "file", up.Files[0].Name, "file parts are named file")
	assert.Equal(t, env.tg.content, up.Files[0].Data, "uploaded bytes are the Telegram file")
	assert.Less(t, checks[0].Seq, up.Seq, "the check precedes the upload")

	stored := env.hub.storedUploads()
	require.Len(t, stored, 1, "exactly one stored upload")
	assert.Equal(t, sender, stored[0].Owner, "the upload is attributed to the sender")

	require.Len(t, sends, 1, "exactly one inbound send")
	var send iaSpecInboundSend
	require.NoError(t, json.Unmarshal(sends[0].Body, &send), "inbound body: %s", sends[0].Body)
	assert.Equal(t, []string{stored[0].ID}, send.AttachmentIDs, "the inbound send names the upload by id, at the top level")
	assert.Empty(t, send.Message.AttachmentIDs, "the ids are not inside the message")
	assert.Empty(t, send.Message.Attachments, "the inbound send carries no path attachment")
	assert.Equal(t, "agent:coder", send.Message.Recipient, "inbound recipient")
	assert.Less(t, up.Seq, sends[0].Seq, "the upload precedes the send")
}

// A sender in a one-member group gets the file into the agent's inbox
// through an upload, and nothing is written to disk.
func TestInboundAttachmentSpecNarrowGroup(t *testing.T) {
	for _, withDownloadsPath := range []bool{true, false} {
		t.Run(fmt.Sprintf("downloads_path=%t", withDownloadsPath), func(t *testing.T) {
			hub := newIASpecHub(t, "artifact", true, "coder")
			env := newIASpecEnv(t, hub, withDownloadsPath)
			env.mapUser(t, "456", "alice@example.com", "uid-alice")
			chat := TGChat{ID: -1001, Type: "group", Title: "alice and bot"}
			env.linkGroup(t, chat.ID, chat.Title)
			env.tg.inner.setChatMember(chat.ID, 456, "creator")
			env.tg.inner.setChatMember(chat.ID, 100, "administrator")

			env.sendDocument(chat, &TGUser{ID: 456, Username: "alice"})

			env.assertNoAttachmentWrites(t)
			env.assertDeliveredThroughUpload(t, iaSpecSender)
		})
	}
}

// A group with several people is handled like a narrow group: inbox
// delivery through an upload, nothing written to disk.
func TestInboundAttachmentSpecSharedGroup(t *testing.T) {
	for _, withDownloadsPath := range []bool{true, false} {
		t.Run(fmt.Sprintf("downloads_path=%t", withDownloadsPath), func(t *testing.T) {
			hub := newIASpecHub(t, "artifact", true, "coder")
			env := newIASpecEnv(t, hub, withDownloadsPath)
			env.mapUser(t, "456", "alice@example.com", "uid-alice")
			env.mapUser(t, "457", "bob@example.com", "uid-bob")
			env.mapUser(t, "458", "carol@example.com", "uid-carol")
			chat := TGChat{ID: -1002, Type: "supergroup", Title: "project team"}
			env.linkGroup(t, chat.ID, chat.Title)
			env.tg.inner.setChatMember(chat.ID, 456, "member")
			env.tg.inner.setChatMember(chat.ID, 457, "administrator")
			env.tg.inner.setChatMember(chat.ID, 458, "member")
			env.tg.inner.setChatMember(chat.ID, 100, "administrator")

			env.sendDocument(chat, &TGUser{ID: 456, Username: "alice"})

			env.assertNoAttachmentWrites(t)
			env.assertDeliveredThroughUpload(t, iaSpecSender)
		})
	}
}

// A sender who is not linked, or not resolved by the hub, causes no
// download and no write, whatever the hub answers.
func TestInboundAttachmentSpecUnresolvedSender(t *testing.T) {
	senders := []struct {
		name  string
		setup func(t *testing.T, env *iaSpecEnv)
	}{
		{name: "not registered", setup: func(*testing.T, *iaSpecEnv) {}},
		{name: "link without email", setup: func(t *testing.T, env *iaSpecEnv) {
			env.mapUser(t, "456", "", "")
		}},
	}
	for _, mode := range []string{"artifact", "legacy", "unavailable"} {
		for _, s := range senders {
			t.Run(mode+"/"+s.name, func(t *testing.T) {
				hub := newIASpecHub(t, mode, false, "coder")
				env := newIASpecEnv(t, hub, true)
				chat := TGChat{ID: -1003, Type: "group", Title: "linked group"}
				env.linkGroup(t, chat.ID, chat.Title)
				s.setup(t, env)

				env.sendDocument(chat, &TGUser{ID: 456, Username: "alice"})

				getFile, download := env.tg.fileCalls()
				assert.Zero(t, getFile, "getFile must not be called")
				assert.Zero(t, download, "the file download path must not be requested")
				assert.Empty(t, hub.requestsTo(iaSpecUploadPath), "no attachment upload request")
				assert.Empty(t, hub.storedUploads(), "no stored upload")
				env.assertNoAttachmentWrites(t)
			})
		}
	}
}
