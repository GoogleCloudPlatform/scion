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

package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests of --attach uploads (ptone/scion#4596, section 7.1) against a fake
// hub that answers the capability probe, stores uploads and records the
// attachment ids each send names.

type attachHub struct {
	t           *testing.T
	probeStatus int    // 0: 200
	uploads     string // the probe's "uploads" answer
	budget      *int64
	failName    string // an uploaded file name the hub refuses
	uploadDelay time.Duration

	mu        sync.Mutex
	probes    int
	uploadReq int
	uploaded  []string            // file names, in order
	sends     map[string][]string // send path -> attachment ids
	nextID    int
}

func newAttachHub(t *testing.T, uploads string) (*attachHub, *httptest.Server) {
	t.Helper()
	h := &attachHub{t: t, uploads: uploads, sends: map[string][]string{}}
	srv := httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(srv.Close)
	return h, srv
}

func (h *attachHub) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case r.URL.Path == "/healthz":
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	case r.URL.Path == "/api/v1/chat/attachments" && r.Method == http.MethodGet:
		h.probes++
		if h.probeStatus != 0 {
			w.WriteHeader(h.probeStatus)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"error": map[string]string{"code": "x", "message": "refused"}})
			return
		}
		answer := map[string]interface{}{"uploads": h.uploads}
		if h.budget != nil {
			answer["delivery_max_bytes"] = *h.budget
		}
		_ = json.NewEncoder(w).Encode(answer)
	case r.URL.Path == "/api/v1/chat/attachments" && r.Method == http.MethodPost:
		h.uploadReq++
		time.Sleep(h.uploadDelay)
		require.NoError(h.t, r.ParseMultipartForm(1<<20))
		var stored, failures []map[string]interface{}
		for _, fh := range r.MultipartForm.File["files"] {
			h.uploaded = append(h.uploaded, fh.Filename)
			if fh.Filename == h.failName {
				failures = append(failures, map[string]interface{}{"name": fh.Filename, "error": "refused by the hub"})
				continue
			}
			h.nextID++
			stored = append(stored, map[string]interface{}{"id": fmt.Sprintf("att-%d", h.nextID), "name": fh.Filename, "size": fh.Size})
		}
		status := http.StatusCreated
		if len(stored) == 0 {
			status = http.StatusBadRequest
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"attachments": stored, "failures": failures})
	case r.Method == http.MethodPost && (strings.HasSuffix(r.URL.Path, "/message") || strings.HasSuffix(r.URL.Path, "/outbound-message")):
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			AttachmentIDs     []string                    `json:"attachment_ids"`
			StructuredMessage *messages.StructuredMessage `json:"structured_message"`
			Attachments       []string                    `json:"attachments"`
		}
		require.NoError(h.t, json.Unmarshal(raw, &body))
		if body.StructuredMessage != nil {
			assert.Empty(h.t, body.StructuredMessage.Attachments, "no staged path is sent with uploads")
		}
		assert.Empty(h.t, body.Attachments, "no staged path is sent with uploads")
		h.sends[r.URL.Path] = append(h.sends[r.URL.Path], body.AttachmentIDs...)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"message_id": "msg-1", "status": "dispatched"})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (h *attachHub) counts() (probes, uploads int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.probes, h.uploadReq
}

func attachHubClient(t *testing.T, srv *httptest.Server) hubclient.Client {
	t.Helper()
	c, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	return c
}

func writeAttachFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	return p
}

// useAttachUpload sets msgAttachUpload for one test.
func useAttachUpload(t *testing.T, u *attachmentUpload) {
	t.Helper()
	orig := msgAttachUpload
	msgAttachUpload = u
	t.Cleanup(func() { msgAttachUpload = orig })
}

func TestAttachUpload_SendCarriesIDs(t *testing.T) {
	setAttachWarnState(t, "")
	msgAttach = nil
	t.Setenv("SCION_AGENT_NAME", "sender")
	h, srv := newAttachHub(t, "artifact")
	dir := t.TempDir()
	a := writeAttachFile(t, dir, "a.txt", "alpha")
	b := writeAttachFile(t, dir, "b.log", "beta")
	upload, _, err := prepareAttachmentUpload(attachHubClient(t, srv), []string{a, b}, true)
	require.NoError(t, err)
	require.NotNil(t, upload)
	useAttachUpload(t, upload)
	hubCtx := &HubContext{Client: attachHubClient(t, srv), Endpoint: srv.URL, ProjectID: "proj-attach"}
	_, _ = captureStdoutStderr(t, func() {
		require.NoError(t, sendMessageViaHub(hubCtx, "builder", "see attached", false, false, false))
	})
	require.Equal(t, []string{"att-1", "att-2"}, h.sends["/api/v1/projects/proj-attach/agents/builder/message"])
	require.Equal(t, []string{"a.txt", "b.log"}, h.uploaded)
}

// TestAttachUpload_HumanSessionSendsIDs: without an agent identity, the
// human send to an agent uploads and names the ids too.
func TestAttachUpload_HumanSessionSendsIDs(t *testing.T) {
	setAttachWarnState(t, "")
	msgAttach = nil
	t.Setenv("SCION_AGENT_NAME", "")
	h, srv := newAttachHub(t, "artifact")
	a := writeAttachFile(t, t.TempDir(), "a.txt", "alpha")
	upload, _, err := prepareAttachmentUpload(attachHubClient(t, srv), []string{a}, true)
	require.NoError(t, err)
	require.NotNil(t, upload, "a session gets artifact from the probe")
	useAttachUpload(t, upload)
	hubCtx := &HubContext{Client: attachHubClient(t, srv), Endpoint: srv.URL, ProjectID: "proj-attach"}
	_, _ = captureStdoutStderr(t, func() {
		require.NoError(t, sendMessageViaHub(hubCtx, "builder", "see attached", false, false, false))
	})
	require.Equal(t, []string{"att-1"}, h.sends["/api/v1/projects/proj-attach/agents/builder/message"])
}

// TestAttachUpload_LegacyAnswerKeepsTodaysPath: with the answer "legacy",
// no guard rail applies (an HTML file is not refused) and today's path
// checks run.
func TestAttachUpload_LegacyAnswerKeepsTodaysPath(t *testing.T) {
	h, srv := newAttachHub(t, "legacy")
	page := writeAttachFile(t, t.TempDir(), "page.html", "<html></html>")
	upload, _, err := prepareAttachmentUpload(attachHubClient(t, srv), []string{page}, true)
	require.NoError(t, err)
	require.Nil(t, upload, "today's staging path")
	probes, uploads := h.counts()
	require.Equal(t, 1, probes, "the probe runs first")
	require.Zero(t, uploads)
	err = validateStagedAttachmentPaths([]string{"/etc/passwd"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "outside allowed roots")
}

// TestAttachUpload_OldHubFallsBack: a probe answered 403, 404 or 405 (a hub
// from before the probe) falls back to today's path.
func TestAttachUpload_OldHubFallsBack(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusMethodNotAllowed} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			h, srv := newAttachHub(t, "artifact")
			h.probeStatus = status
			a := writeAttachFile(t, t.TempDir(), "a.txt", "alpha")
			upload, _, err := prepareAttachmentUpload(attachHubClient(t, srv), []string{a}, true)
			require.NoError(t, err)
			require.Nil(t, upload)
		})
	}
	h, srv := newAttachHub(t, "artifact")
	h.probeStatus = http.StatusInternalServerError
	a := writeAttachFile(t, t.TempDir(), "a.txt", "alpha")
	_, _, err := prepareAttachmentUpload(attachHubClient(t, srv), []string{a}, true)
	require.Error(t, err, "any other probe error stops the send")
}

// TestAttachUpload_HTMLAndSVGRefusedBeforeUpload: by extension and by
// content, before any upload request, as a usage error.
func TestAttachUpload_HTMLAndSVGRefusedBeforeUpload(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{
		writeAttachFile(t, dir, "page.HTM", "hello"),
		writeAttachFile(t, dir, "logo.svg", "x"),
		writeAttachFile(t, dir, "notes.txt", "  <HTML><body>"),
		writeAttachFile(t, dir, "img.dat", "<?xml version=\"1.0\"?><svg xmlns=\"x\">"),
	} {
		h, srv := newAttachHub(t, "artifact")
		_, _, err := prepareAttachmentUpload(attachHubClient(t, srv), []string{p}, false)
		require.Error(t, err, p)
		require.Contains(t, err.Error(), htmlRefusal)
		var usage *usageError
		require.True(t, errors.As(err, &usage), "exit code 2")
		_, uploads := h.counts()
		require.Zero(t, uploads)
	}
}

// TestAttachUpload_DuplicateNamesAndBudgetRefusedBeforeUpload.
func TestAttachUpload_DuplicateNamesAndBudgetRefusedBeforeUpload(t *testing.T) {
	h, srv := newAttachHub(t, "artifact")
	d1, d2 := t.TempDir(), t.TempDir()
	a := writeAttachFile(t, d1, "Report.txt", "one")
	b := writeAttachFile(t, d2, "report.TXT", "two")
	_, _, err := prepareAttachmentUpload(attachHubClient(t, srv), []string{a, b}, false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "same file name")

	budget := int64(6)
	h.budget = &budget
	c := writeAttachFile(t, d1, "c.txt", "four")
	e := writeAttachFile(t, d2, "e.txt", "four")
	_, _, err = prepareAttachmentUpload(attachHubClient(t, srv), []string{c, e}, true)
	require.Error(t, err)
	require.Contains(t, err.Error(), "over the hub's limit")
	upload, _, err := prepareAttachmentUpload(attachHubClient(t, srv), []string{c, e}, false)
	require.NoError(t, err, "the budget applies only with an agent recipient")
	require.NotNil(t, upload)
	_, uploads := h.counts()
	require.Zero(t, uploads)
}

func TestAttachUpload_MarkdownHint(t *testing.T) {
	_, srv := newAttachHub(t, "artifact")
	big := writeAttachFile(t, t.TempDir(), "notes.md", strings.Repeat("# x\n", markdownHintBytes/4+1))
	var err error
	_, stderr := captureStdoutStderr(t, func() {
		_, _, err = prepareAttachmentUpload(attachHubClient(t, srv), []string{big}, false)
	})
	require.NoError(t, err)
	require.Contains(t, stderr, "large Markdown is easier to review as an artifact: scion artifact publish")
}

// TestAttachUpload_PartialFailureSendsNothing.
func TestAttachUpload_PartialFailureSendsNothing(t *testing.T) {
	setAttachWarnState(t, "")
	msgAttach = nil
	t.Setenv("SCION_AGENT_NAME", "sender")
	h, srv := newAttachHub(t, "artifact")
	h.failName = "b.txt"
	dir := t.TempDir()
	upload, _, err := prepareAttachmentUpload(attachHubClient(t, srv),
		[]string{writeAttachFile(t, dir, "a.txt", "a"), writeAttachFile(t, dir, "b.txt", "b")}, true)
	require.NoError(t, err)
	useAttachUpload(t, upload)
	hubCtx := &HubContext{Client: attachHubClient(t, srv), Endpoint: srv.URL, ProjectID: "proj-attach"}
	var sendErr error
	_, _ = captureStdoutStderr(t, func() {
		sendErr = sendMessageViaHub(hubCtx, "builder", "see attached", false, false, false)
	})
	require.Error(t, sendErr)
	require.Contains(t, sendErr.Error(), "b.txt: refused by the hub")
	require.Empty(t, h.sends, "nothing is sent")
}

// TestAttachUpload_GroupUploadsOncePerRecipient.
func TestAttachUpload_GroupUploadsOncePerRecipient(t *testing.T) {
	setAttachWarnState(t, "")
	msgAttach = nil
	t.Setenv("SCION_AGENT_NAME", "sender")
	h, srv := newAttachHub(t, "artifact")
	upload, _, err := prepareAttachmentUpload(attachHubClient(t, srv), []string{writeAttachFile(t, t.TempDir(), "a.txt", "a")}, true)
	require.NoError(t, err)
	useAttachUpload(t, upload)
	hubCtx := &HubContext{Client: attachHubClient(t, srv), Endpoint: srv.URL, ProjectID: "proj-attach"}
	_, _ = captureStdoutStderr(t, func() {
		_ = sendGroupMessageViaHub(hubCtx, agentRecipients("agent-a", "agent-b"), "hi", false)
	})
	_, uploads := h.counts()
	require.Equal(t, 2, uploads, "one upload per recipient")
	h.mu.Lock()
	defer h.mu.Unlock()
	require.Len(t, h.sends["/api/v1/projects/proj-attach/agents/agent-a/message"], 1)
	require.Len(t, h.sends["/api/v1/projects/proj-attach/agents/agent-b/message"], 1)
	require.NotEqual(t, h.sends["/api/v1/projects/proj-attach/agents/agent-a/message"][0],
		h.sends["/api/v1/projects/proj-attach/agents/agent-b/message"][0], "each recipient send names its own upload")
}

// TestAttachUpload_SlowUploadKeepsSendTime: the send's own timeout starts
// after the upload, so an upload slower than that timeout does not fail the
// send that names it.
func TestAttachUpload_SlowUploadKeepsSendTime(t *testing.T) {
	setAttachWarnState(t, "")
	msgAttach = nil
	t.Setenv("SCION_AGENT_NAME", "sender")
	orig := msgSendTimeout
	msgSendTimeout = 2 * time.Second
	t.Cleanup(func() { msgSendTimeout = orig })
	h, srv := newAttachHub(t, "artifact")
	h.uploadDelay = 3 * time.Second
	upload, _, err := prepareAttachmentUpload(attachHubClient(t, srv), []string{writeAttachFile(t, t.TempDir(), "a.txt", "a")}, true)
	require.NoError(t, err)
	useAttachUpload(t, upload)
	hubCtx := &HubContext{Client: attachHubClient(t, srv), Endpoint: srv.URL, ProjectID: "proj-attach"}
	var sendErr error
	_, _ = captureStdoutStderr(t, func() {
		sendErr = sendMessageViaHub(hubCtx, "builder", "see attached", false, false, false)
	})
	require.NoError(t, sendErr)
	h.mu.Lock()
	defer h.mu.Unlock()
	require.Equal(t, []string{"att-1"}, h.sends["/api/v1/projects/proj-attach/agents/builder/message"])
}

// TestResolveAttachMode: the probe decides; with "legacy" or a refused
// probe, today's path checks run and the paths are staged unchanged; with
// "artifact", an upload is set and nothing is staged.
func TestResolveAttachMode(t *testing.T) {
	// The allowed roots point at a temporary directory, so the legacy cases
	// run on any machine.
	// Resolved, because resolveAttachmentPath resolves symlinks (on macOS the
	// temporary directory lies under a symlinked /var).
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	origRoots := attachmentRoots
	attachmentRoots = []string{root}
	t.Cleanup(func() { attachmentRoots = origRoots })
	inRoot := writeAttachFile(t, root, "notes.txt", "notes")
	for _, tc := range []struct {
		name    string
		uploads string
		status  int
	}{
		{"legacy", "legacy", 0},
		{"old hub 403", "artifact", http.StatusForbidden},
		{"old hub 404", "artifact", http.StatusNotFound},
		{"old hub 405", "artifact", http.StatusMethodNotAllowed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, srv := newAttachHub(t, tc.uploads)
			h.probeStatus = tc.status
			c := attachHubClient(t, srv)
			_, _, _, err := resolveAttachMode(c, []string{"/etc/passwd"}, true)
			require.Error(t, err)
			require.Contains(t, err.Error(), "outside allowed roots")
			_, _, _, err = resolveAttachMode(c, []string{root}, true)
			require.Error(t, err)
			require.Contains(t, err.Error(), "is a directory, not a regular file")
			upload, staged, _, err := resolveAttachMode(c, []string{inRoot}, true)
			require.NoError(t, err)
			require.Nil(t, upload)
			require.Equal(t, []string{inRoot}, staged, "today's staging gets the paths unchanged")
			_, uploads := h.counts()
			require.Zero(t, uploads)
		})
	}
	h, srv := newAttachHub(t, "artifact")
	upload, staged, _, err := resolveAttachMode(attachHubClient(t, srv), []string{writeAttachFile(t, t.TempDir(), "a.txt", "a")}, true)
	require.NoError(t, err)
	require.NotNil(t, upload)
	require.Nil(t, staged, "nothing is staged when the files are uploaded")
	_, uploads := h.counts()
	require.Zero(t, uploads, "nothing is uploaded before the send")
}

// noUploadClient is a hub client that cannot upload attachments: embedding
// the interface hides hubclient.AttachmentUploader.
type noUploadClient struct{ hubclient.Client }

// TestResolveAttachMode_FallbackReason: when the --attach files fall back
// to staging, resolveAttachMode names the reason; an upload, a send without
// --attach files or an invalid path gives none and prints nothing
// (ptone/scion#6549).
func TestResolveAttachMode_FallbackReason(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	origRoots := attachmentRoots
	attachmentRoots = []string{root}
	t.Cleanup(func() { attachmentRoots = origRoots })
	inRoot := writeAttachFile(t, root, "notes.txt", "notes")
	for _, tc := range []struct {
		name    string
		uploads string
		status  int
		noUp    bool
		want    string
	}{
		{"legacy", "legacy", 0, false, `the hub answered "legacy"`},
		{"empty answer", "", 0, false, "the hub did not offer uploads"},
		{"status 403", "artifact", http.StatusForbidden, false, "the hub refused the upload probe with status 403"},
		{"status 404", "artifact", http.StatusNotFound, false, "the hub refused the upload probe with status 404"},
		{"status 405", "artifact", http.StatusMethodNotAllowed, false, "the hub refused the upload probe with status 405"},
		{"client cannot upload", "artifact", 0, true, "this client cannot upload attachments"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, srv := newAttachHub(t, tc.uploads)
			h.probeStatus = tc.status
			c := attachHubClient(t, srv)
			if tc.noUp {
				c = noUploadClient{c}
			}
			var staged []string
			var fallback string
			stderr := captureStderr(t, func() {
				var err error
				_, staged, fallback, err = resolveAttachMode(c, []string{inRoot}, true)
				require.NoError(t, err)
			})
			require.Equal(t, []string{inRoot}, staged)
			require.Equal(t, tc.want, fallback)
			require.Empty(t, stderr, "the warning waits for staging")
		})
	}
	t.Run("invalid path on a legacy hub", func(t *testing.T) {
		_, srv := newAttachHub(t, "legacy")
		stderr := captureStderr(t, func() {
			_, _, fallback, err := resolveAttachMode(attachHubClient(t, srv), []string{"/etc/passwd"}, true)
			require.Error(t, err)
			require.Contains(t, err.Error(), "outside allowed roots")
			require.Empty(t, fallback)
		})
		// resolveAttachmentPath's own outside-roots warning may print; the
		// fallback line does not.
		require.NotContains(t, stderr, "attachments are staged")
	})
	t.Run("artifact", func(t *testing.T) {
		_, srv := newAttachHub(t, "artifact")
		stderr := captureStderr(t, func() {
			upload, _, fallback, err := resolveAttachMode(attachHubClient(t, srv), []string{writeAttachFile(t, t.TempDir(), "a.txt", "a")}, true)
			require.NoError(t, err)
			require.NotNil(t, upload)
			require.Empty(t, fallback)
		})
		require.Empty(t, stderr)
	})
}

// TestStageAttachmentsWithWarning: one stderr line naming the reason is
// printed only after the files are staged; a staging failure or no files
// prints nothing.
func TestStageAttachmentsWithWarning(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	origRoots := attachmentRoots
	attachmentRoots = []string{root}
	t.Cleanup(func() { attachmentRoots = origRoots })
	t.Setenv("SCION_AGENT_NAME", "sender")
	file := writeAttachFile(t, root, "notes.txt", "notes")
	const reason = `the hub answered "legacy"`

	t.Run("staged", func(t *testing.T) {
		withScratchpadTargets(t, t.TempDir())
		var staged []string
		stderr := captureStderr(t, func() {
			var err error
			staged, err = stageAttachmentsWithWarning([]string{file}, reason)
			require.NoError(t, err)
		})
		require.Len(t, staged, 1)
		require.Equal(t, 1, strings.Count(stderr, "\n"), "one line: %q", stderr)
		require.Contains(t, stderr, "Warning: attachments are staged on the local scratchpad")
		require.Contains(t, stderr, reason)
		require.Contains(t, stderr, "they may not reach the hub or recipients on another broker")
	})
	t.Run("staging fails", func(t *testing.T) {
		withScratchpadTargets(t, filepath.Join(t.TempDir(), "missing"))
		stderr := captureStderr(t, func() {
			_, err := stageAttachmentsWithWarning([]string{file}, reason)
			require.Error(t, err)
			require.Contains(t, err.Error(), "scratchpad volume not available")
		})
		require.Empty(t, stderr)
	})
	t.Run("no files", func(t *testing.T) {
		withScratchpadTargets(t, filepath.Join(t.TempDir(), "missing"))
		stderr := captureStderr(t, func() {
			staged, err := stageAttachmentsWithWarning(nil, reason)
			require.NoError(t, err)
			require.Empty(t, staged)
		})
		require.Empty(t, stderr)
	})
}

// TestAttachUpload_ReducedNameCollisionRefusedBeforeUpload: two files whose
// names would be delivered under one name are refused before any upload.
func TestAttachUpload_ReducedNameCollisionRefusedBeforeUpload(t *testing.T) {
	h, srv := newAttachHub(t, "artifact")
	a := writeAttachFile(t, t.TempDir(), "a b.txt", "one")
	b := writeAttachFile(t, t.TempDir(), "a_b.txt", "two")
	_, _, err := prepareAttachmentUpload(attachHubClient(t, srv), []string{a, b}, false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "would be delivered under one file name")
	var usage *usageError
	require.True(t, errors.As(err, &usage))
	_, uploads := h.counts()
	require.Zero(t, uploads)
}

// TestAttachUpload_OutboundAndConversationRefSendIDs: the outbound send to
// a user and a conversation-reference send name the uploads too.
func TestAttachUpload_OutboundAndConversationRefSendIDs(t *testing.T) {
	setAttachWarnState(t, "")
	msgAttach = nil
	t.Setenv("SCION_AGENT_NAME", "sender")
	h, srv := newAttachHub(t, "artifact")
	upload, _, err := prepareAttachmentUpload(attachHubClient(t, srv), []string{writeAttachFile(t, t.TempDir(), "a.txt", "a")}, false)
	require.NoError(t, err)
	useAttachUpload(t, upload)
	hubCtx := &HubContext{Client: attachHubClient(t, srv), Endpoint: srv.URL, ProjectID: "proj-attach"}
	ref, err := messaging.ParseReference("conv:6f1d6a5e-0c2b-4f7a-9d3e-2b8c4a1e5f60")
	require.NoError(t, err)
	_, _ = captureStdoutStderr(t, func() {
		require.NoError(t, sendOutboundMessageViaHub(hubCtx, "user:someone@example.com", "see attached", false))
		require.NoError(t, sendMessageViaConversation(hubCtx, ref, "see attached", false, false, nil))
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	require.Equal(t, []string{"att-1", "att-2"}, h.sends["/api/v1/projects/proj-attach/agents/sender/outbound-message"],
		"one upload for each send, named on the outbound route")
}
