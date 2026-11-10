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
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
)

// serveAttachmentCheck points b's hub URL at a server whose attachment
// check answers answer. Other routes are not served.
func serveAttachmentCheck(t *testing.T, b *TelegramBrokerV2, answer attachmentCheckAnswer) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != inboundAttachmentCheckPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(answer)
	}))
	t.Cleanup(srv.Close)
	b.mu.Lock()
	b.hubURL = srv.URL
	b.mu.Unlock()
}

// useLegacyAttachmentCheck makes the hub answer "legacy" for b and points
// the scratchpad at a temp HOME, so attachments are staged there.
func useLegacyAttachmentCheck(t *testing.T, b *TelegramBrokerV2) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	stubSharedDirSettings(t, &config.VersionedSettings{})
	serveAttachmentCheck(t, b, attachmentCheckAnswer{Authorized: true, Uploads: attachmentUploadsLegacy})
}

// countLegacyStages counts legacyStage calls for the rest of the test.
func countLegacyStages(t *testing.T) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	orig := onLegacyStage
	onLegacyStage = func() { n.Add(1) }
	t.Cleanup(func() { onLegacyStage = orig })
	return &n
}

// iaSendCaption delivers a document with caption from alice (mapped and
// authorized by the plugin) to the linked group.
func (env *iaSpecEnv) iaSendCaption(t *testing.T, caption string) {
	t.Helper()
	env.b.handleIncomingMessageV2(env.b.gen, &TGMessage{
		MessageID: 77,
		From:      &TGUser{ID: 456, Username: "alice"},
		Chat:      TGChat{ID: -1001, Type: "group", Title: "group"},
		Date:      time.Now().Unix(),
		Caption:   caption,
		Document: &TGDocument{
			FileID:       "spec-doc-1",
			FileUniqueID: "spec-uniq-1",
			FileName:     "notes.txt",
			MimeType:     "text/plain",
			FileSize:     int64(len(env.tg.content)),
		},
	})
}

// newIAEnv returns a spec env with alice mapped and group -1001 linked.
func newIAEnv(t *testing.T, hub *iaSpecHub) *iaSpecEnv {
	t.Helper()
	env := newIASpecEnv(t, hub, true)
	env.mapUser(t, "456", "alice@example.com", "uid-alice")
	env.linkGroup(t, -1001, "group")
	return env
}

// chatTexts returns the texts the bot sent to chats.
func (env *iaSpecEnv) chatTexts() []string {
	var out []string
	for _, m := range env.tg.inner.getSentMessages() {
		out = append(out, m.Text)
	}
	return out
}

// sends decodes every inbound send the hub received.
func (env *iaSpecEnv) sends(t *testing.T) []iaSpecInboundSend {
	t.Helper()
	var out []iaSpecInboundSend
	for _, r := range env.hub.requestsTo(iaSpecInboundPath) {
		var s iaSpecInboundSend
		require.NoError(t, json.Unmarshal(r.Body, &s), "inbound body: %s", r.Body)
		out = append(out, s)
	}
	return out
}

// telegramEntries returns the entries below the scratchpad's
// .attachments/_telegram.
func (env *iaSpecEnv) telegramEntries(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(env.scratchpad, ".attachments", projectfs.AttachmentChannelTelegram))
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func anyContains(texts []string, sub string) bool {
	for _, s := range texts {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// A sender the hub refuses causes no download and no write.
func TestInboundAttachment_RefusedSender(t *testing.T) {
	for _, mode := range []string{"artifact", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			stages := countLegacyStages(t)
			hub := newIASpecHub(t, mode, false, "coder")
			env := newIAEnv(t, hub)

			env.iaSendCaption(t, "please review this file")

			getFile, download := env.tg.fileCalls()
			assert.Zero(t, getFile, "getFile must not be called")
			assert.Zero(t, download, "the file must not be downloaded")
			assert.Len(t, hub.requestsTo(iaSpecCheckPath), 1, "the hub was asked")
			assert.Empty(t, hub.requestsTo(iaSpecUploadPath), "no upload")
			assert.Empty(t, hub.storedUploads(), "nothing stored")
			assert.Empty(t, hub.requestsTo(iaSpecInboundPath), "nothing sent")
			assert.Zero(t, stages.Load(), "nothing staged")
			assert.True(t, anyContains(env.chatTexts(), attachmentsRefusedText), "the sender is told: %q", env.chatTexts())
			env.assertNoAttachmentWrites(t)
		})
	}
}

// In artifact mode a send the hub refuses after the
// upload leaves only the upload; nothing is staged.
func TestInboundAttachment_ArtifactSendRefused(t *testing.T) {
	for _, status := range []int{http.StatusConflict, http.StatusNotFound, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			stages := countLegacyStages(t)
			hub := newIASpecHub(t, "artifact", true, "coder")
			hub.set(func() { hub.inboundStatus = map[string]int{"agent:coder": status} })
			env := newIAEnv(t, hub)

			env.iaSendCaption(t, "please review this file")

			stored := hub.storedUploads()
			require.Len(t, stored, 1, "the upload exists")
			assert.Equal(t, iaSpecSender, stored[0].Owner)
			sends := env.sends(t)
			require.Len(t, sends, 1)
			assert.Equal(t, []string{stored[0].ID}, sends[0].AttachmentIDs)
			assert.Zero(t, stages.Load(), "nothing staged")
			assert.True(t, anyContains(env.chatTexts(), "Message delivery failed"), "the failure is shown: %q", env.chatTexts())
			env.assertNoAttachmentWrites(t)
		})
	}
}

// In legacy mode a refused send removes the entry it
// staged.
func TestInboundAttachment_LegacySendRefusedRemovesEntry(t *testing.T) {
	for _, status := range []int{http.StatusConflict, http.StatusNotFound, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			stages := countLegacyStages(t)
			hub := newIASpecHub(t, "legacy", true, "coder")
			hub.set(func() { hub.inboundStatus = map[string]int{"agent:coder": status} })
			env := newIAEnv(t, hub)

			env.iaSendCaption(t, "please review this file")

			assert.Equal(t, int32(1), stages.Load(), "the entry was staged")
			sends := env.sends(t)
			require.Len(t, sends, 1)
			require.Len(t, sends[0].Message.Attachments, 1, "the send named the staged path")
			assert.True(t, strings.HasPrefix(sends[0].Message.Attachments[0], "/scion-volumes/scratchpad/.attachments/_telegram/tg_"))
			assert.Empty(t, env.telegramEntries(t), "the refused message's entry is removed")
			assert.True(t, anyContains(env.chatTexts(), "Message delivery failed"), "the failure is shown: %q", env.chatTexts())
			env.assertNoAttachmentWrites(t)
		})
	}
}

// With several recipients, the staged entry is kept if one
// accepted and removed if none did.
func TestInboundAttachment_LegacyMultipleRecipients(t *testing.T) {
	cases := []struct {
		name    string
		refuse  map[string]int
		wantKep bool
	}{
		{"one accepts", map[string]int{"agent:coder": http.StatusConflict}, true},
		{"none accepts", map[string]int{"agent:coder": http.StatusConflict, "agent:reviewer": http.StatusNotFound}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub := newIASpecHub(t, "legacy", true, "coder", "reviewer")
			hub.set(func() { hub.inboundStatus = tc.refuse })
			env := newIAEnv(t, hub)

			env.iaSendCaption(t, "@coder @reviewer please review this file")

			require.Len(t, env.sends(t), 2, "one send per recipient")
			if tc.wantKep {
				require.Len(t, env.telegramEntries(t), 1, "the entry stays for the recipient that accepted")
				assert.NotEmpty(t, iaSpecFilesHolding(t, env.scratchpad, env.tg.content))
			} else {
				assert.Empty(t, env.telegramEntries(t), "no recipient accepted: the entry is removed")
				env.assertNoAttachmentWrites(t)
			}
		})
	}
}

// In legacy mode the file is staged only after the hub
// authorized the sender, in a per-message entry whose path is delivered,
// and never under downloads_path.
func TestInboundAttachment_LegacyStagesAfterCheck(t *testing.T) {
	hub := newIASpecHub(t, "legacy", true, "coder")
	env := newIAEnv(t, hub)
	var checksAtStage []int
	orig := onLegacyStage
	onLegacyStage = func() { checksAtStage = append(checksAtStage, len(hub.requestsTo(iaSpecCheckPath))) }
	t.Cleanup(func() { onLegacyStage = orig })

	env.iaSendCaption(t, "please review this file")

	assert.Equal(t, []int{1}, checksAtStage, "staged once, after the check")
	entries := env.telegramEntries(t)
	require.Len(t, entries, 1)
	assert.Regexp(t, `^tg_[0-9]+$`, entries[0], "per-message entry")
	sends := env.sends(t)
	require.Len(t, sends, 1)
	assert.Equal(t, []string{"/scion-volumes/scratchpad/.attachments/_telegram/" + entries[0] + "/notes.txt"}, sends[0].Message.Attachments)
	assert.Empty(t, sends[0].AttachmentIDs)
	assert.Empty(t, hub.requestsTo(iaSpecUploadPath), "no upload in legacy mode")
	assert.Empty(t, iaSpecFilesUnder(t, env.downloads), "nothing under downloads_path")
	assert.Equal(t, []string{filepath.Join(".attachments", "_telegram", entries[0], "notes.txt")}, iaSpecFilesHolding(t, env.scratchpad, env.tg.content),
		"the bytes are in the per-message entry")
	assert.Len(t, iaSpecFilesHolding(t, env.home, env.tg.content), 1, "and nowhere else under HOME")
}

// When the hub takes no attachments, the text is delivered
// with a notice and nothing is downloaded or written. Unknown answers are
// treated the same way.
func TestInboundAttachment_UnavailableDeliversTextWithNotice(t *testing.T) {
	for _, mode := range []string{"unavailable", "some-future-mode"} {
		t.Run(mode, func(t *testing.T) {
			stages := countLegacyStages(t)
			hub := newIASpecHub(t, mode, true, "coder")
			env := newIAEnv(t, hub)

			env.iaSendCaption(t, "please review this file")

			getFile, download := env.tg.fileCalls()
			assert.Zero(t, getFile)
			assert.Zero(t, download)
			sends := env.sends(t)
			require.Len(t, sends, 1, "the text is delivered")
			assert.Equal(t, "please review this file", sends[0].messageText(t, hub))
			assert.Empty(t, sends[0].Message.Attachments)
			assert.Empty(t, sends[0].AttachmentIDs)
			assert.Empty(t, hub.requestsTo(iaSpecUploadPath))
			assert.Zero(t, stages.Load())
			assert.Contains(t, env.chatTexts(), attachmentsWithheldNotice)
			env.assertNoAttachmentWrites(t)
		})
	}
}

// messageText returns the msg field of the inbound send at the same index
// in the hub's records as s.
func (s iaSpecInboundSend) messageText(t *testing.T, hub *iaSpecHub) string {
	t.Helper()
	for _, r := range hub.requestsTo(iaSpecInboundPath) {
		var raw struct {
			Message struct {
				Recipient string `json:"recipient"`
				Msg       string `json:"msg"`
			} `json:"message"`
		}
		require.NoError(t, json.Unmarshal(r.Body, &raw))
		if raw.Message.Recipient == s.Message.Recipient {
			return raw.Message.Msg
		}
	}
	return ""
}

// An attachment over the hub's delivery limit is refused
// with a notice, before download when Telegram gives its size; nothing is
// uploaded, staged or written, and the text is still delivered.
func TestInboundAttachment_OverDeliveryLimit(t *testing.T) {
	const limit = iaSpecMaxBytes // the hub's current answer
	for _, sizeKnown := range []bool{true, false} {
		t.Run(fmt.Sprintf("size_known=%t", sizeKnown), func(t *testing.T) {
			stages := countLegacyStages(t)
			hub := newIASpecHub(t, "artifact", true, "coder")
			hub.set(func() { hub.maxBytes = limit })
			env := newIAEnv(t, hub)
			env.tg.content = bytes.Repeat([]byte("x"), int(limit)+1)
			env.tg.hideSize = !sizeKnown

			msg := &TGMessage{
				MessageID: 77,
				From:      &TGUser{ID: 456, Username: "alice"},
				Chat:      TGChat{ID: -1001, Type: "group"},
				Date:      time.Now().Unix(),
				Caption:   "please review this file",
				Document:  &TGDocument{FileID: "big", FileUniqueID: "big-u", FileName: "big.bin"},
			}
			if sizeKnown {
				msg.Document.FileSize = limit + 1
			}
			env.b.handleIncomingMessageV2(env.b.gen, msg)

			_, download := env.tg.fileCalls()
			if sizeKnown {
				assert.Zero(t, download, "a file known to be too large is not downloaded")
			}
			assert.Empty(t, hub.requestsTo(iaSpecUploadPath), "no upload")
			assert.Empty(t, hub.storedUploads())
			assert.Zero(t, stages.Load(), "never staged while the hub answered artifact")
			sends := env.sends(t)
			require.Len(t, sends, 1, "the text is still delivered")
			assert.Empty(t, sends[0].AttachmentIDs)
			assert.Empty(t, sends[0].Message.Attachments)
			assert.True(t, anyContains(env.chatTexts(), "larger than agents on this hub accept"), "the sender is told: %q", env.chatTexts())
			assert.True(t, anyContains(env.chatTexts(), fmt.Sprint(limit)), "the notice names the hub's limit")
			env.assertNoAttachmentWrites(t)
		})
	}
}

// Targets plus mentioned agents get one upload per
// recipient send, each send naming its own upload.
func TestInboundAttachment_ArtifactUploadPerRecipient(t *testing.T) {
	hub := newIASpecHub(t, "artifact", true, "coder", "reviewer", "tester")
	env := newIAEnv(t, hub)

	env.iaSendCaption(t, "@coder @tester please check this with @reviewer")

	var check map[string]any
	checks := hub.requestsTo(iaSpecCheckPath)
	require.Len(t, checks, 1)
	require.NoError(t, json.Unmarshal(checks[0].Body, &check))
	assert.ElementsMatch(t, []any{"coder", "tester", "reviewer"}, check["targets"], "the check names targets and mentioned agents")

	uploads := hub.requestsTo(iaSpecUploadPath)
	stored := hub.storedUploads()
	sends := env.sends(t)
	require.Len(t, uploads, 3, "one upload per recipient send")
	require.Len(t, stored, 3)
	require.Len(t, sends, 3)
	var uploadTargets []string
	for _, u := range uploads {
		require.Len(t, u.Meta["targets"], 1)
		uploadTargets = append(uploadTargets, u.Meta["targets"][0])
	}
	sort.Strings(uploadTargets)
	assert.Equal(t, []string{"coder", "reviewer", "tester"}, uploadTargets)

	seen := map[string]bool{}
	for _, s := range sends {
		require.Len(t, s.AttachmentIDs, 1, "send to %s", s.Message.Recipient)
		assert.False(t, seen[s.AttachmentIDs[0]], "an id is bound to one send only")
		seen[s.AttachmentIDs[0]] = true
		assert.Empty(t, s.Message.Attachments)
	}
	env.assertNoAttachmentWrites(t)
}

// iaSnapshot records name, size and mtime of every entry below root.
func iaSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		out[rel] = fmt.Sprintf("%d %d", info.Size(), info.ModTime().UnixNano())
		return nil
	})
	require.NoError(t, err)
	return out
}

// Pre-existing scratchpad entries and downloads_path
// contents are untouched at start and after a refused legacy send.
func TestInboundAttachment_PreExistingEntriesUntouched(t *testing.T) {
	hub := newIASpecHub(t, "legacy", true, "coder")
	hub.set(func() { hub.inboundStatus = map[string]int{"agent:coder": http.StatusConflict} })
	old := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	var oldEntryRoot string
	env := newIASpecEnvWith(t, hub, true, func(scratchpad, downloads string) {
		oldEntryRoot = filepath.Join(scratchpad, ".attachments", projectfs.AttachmentChannelTelegram)
		for _, f := range []string{
			filepath.Join(oldEntryRoot, "tg_1000", "old.txt"),
			filepath.Join(oldEntryRoot, "tg_2000", "older.pdf"),
			filepath.Join(downloads, "tg_3000_kept.bin"),
		} {
			require.NoError(t, os.MkdirAll(filepath.Dir(f), 0o755))
			require.NoError(t, os.WriteFile(f, []byte("pre-existing "+filepath.Base(f)), 0o644))
			require.NoError(t, os.Chtimes(f, old, old))
			require.NoError(t, os.Chtimes(filepath.Dir(f), old, old))
		}
	})
	env.mapUser(t, "456", "alice@example.com", "uid-alice")
	env.linkGroup(t, -1001, "group")
	beforeEntries := iaSnapshot(t, oldEntryRoot)
	beforeDownloads := iaSnapshot(t, env.downloads)
	// The channel directory itself changes when an entry is added and
	// removed; the entries below it must not.
	delete(beforeEntries, ".")

	env.iaSendCaption(t, "please review this file")

	afterEntries := iaSnapshot(t, oldEntryRoot)
	delete(afterEntries, ".")
	assert.Equal(t, beforeEntries, afterEntries, "pre-existing entries keep names, sizes and mtimes")
	assert.Equal(t, beforeDownloads, iaSnapshot(t, env.downloads), "downloads_path contents are untouched")
	assert.ElementsMatch(t, []string{"tg_1000", "tg_2000"}, env.telegramEntries(t), "only the refused message's entry was removed")
}

// projectfs.WriteAttachment is referenced
// only from legacyStage, whose only caller is stageInboundAttachment, which
// runs only in the "legacy" and older-hub branches of
// prepareInboundAttachment.
func TestInboundAttachment_ScratchpadWriterIsLegacyOnly(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	var parsed []*ast.File
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err)
		parsed = append(parsed, f)
	}
	require.NotEmpty(t, parsed, "non-test files parsed")

	// refsIn returns the names of the functions holding each reference
	// matched by match.
	refsIn := func(match func(ast.Node) bool) []string {
		var out []string
		for _, f := range parsed {
			for _, decl := range f.Decls {
				name := "<package level>"
				if fd, ok := decl.(*ast.FuncDecl); ok {
					name = fd.Name.Name
				}
				ast.Inspect(decl, func(n ast.Node) bool {
					if n != nil && match(n) {
						out = append(out, name)
					}
					return true
				})
			}
		}
		return out
	}
	isSelector := func(pkg, sel string) func(ast.Node) bool {
		return func(n ast.Node) bool {
			s, ok := n.(*ast.SelectorExpr)
			if !ok || s.Sel.Name != sel {
				return false
			}
			id, ok := s.X.(*ast.Ident)
			return ok && (pkg == "" || id.Name == pkg)
		}
	}
	isCallOf := func(name string) func(ast.Node) bool {
		return func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return false
			}
			switch fn := c.Fun.(type) {
			case *ast.Ident:
				return fn.Name == name
			case *ast.SelectorExpr:
				return fn.Sel.Name == name
			}
			return false
		}
	}

	assert.Equal(t, []string{"legacyStage"}, refsIn(isSelector("projectfs", "WriteAttachment")), "WriteAttachment is referenced only from legacyStage")
	assert.Equal(t, []string{"stageInboundAttachment"}, refsIn(isCallOf("legacyStage")), "legacyStage has one caller")
	assert.Equal(t, []string{"prepareInboundAttachment", "prepareInboundAttachment"}, refsIn(isCallOf("stageInboundAttachment")))

	// Each stageInboundAttachment call sits in a case clause for the
	// "legacy" answer or for the older hub without the check.
	var clauses []string
	for _, f := range parsed {
		ast.Inspect(f, func(n ast.Node) bool {
			cc, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			calls := false
			for _, stmt := range cc.Body {
				ast.Inspect(stmt, func(m ast.Node) bool {
					if m != nil && isCallOf("stageInboundAttachment")(m) {
						calls = true
					}
					return true
				})
			}
			if calls {
				var b strings.Builder
				for _, e := range cc.List {
					ast.Inspect(e, func(m ast.Node) bool {
						if id, ok := m.(*ast.Ident); ok {
							b.WriteString(id.Name + " ")
						}
						return true
					})
				}
				clauses = append(clauses, strings.TrimSpace(b.String()))
			}
			return true
		})
	}
	assert.ElementsMatch(t, []string{"errors Is err errAttachmentCheckAbsent", "attachmentUploadsLegacy"}, clauses)
}

// legacyStage runs in legacy mode and on an
// older hub only.
func TestInboundAttachment_LegacyStageOnlyInLegacyModes(t *testing.T) {
	cases := []struct {
		name  string
		hub   func(t *testing.T) *iaSpecHub
		wants int32
	}{
		{"artifact", func(t *testing.T) *iaSpecHub { return newIASpecHub(t, "artifact", true, "coder") }, 0},
		{"unavailable", func(t *testing.T) *iaSpecHub { return newIASpecHub(t, "unavailable", true, "coder") }, 0},
		{"refused", func(t *testing.T) *iaSpecHub { return newIASpecHub(t, "legacy", false, "coder") }, 0},
		{"check error", func(t *testing.T) *iaSpecHub {
			h := newIASpecHub(t, "legacy", true, "coder")
			h.set(func() { h.checkStatus = http.StatusInternalServerError })
			return h
		}, 0},
		{"legacy", func(t *testing.T) *iaSpecHub { return newIASpecHub(t, "legacy", true, "coder") }, 1},
		{"older hub", func(t *testing.T) *iaSpecHub {
			h := newIASpecHub(t, "legacy", true, "coder")
			h.set(func() { h.checkStatus = http.StatusNotFound })
			return h
		}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stages := countLegacyStages(t)
			env := newIAEnv(t, tc.hub(t))
			env.iaSendCaption(t, "please review this file")
			assert.Equal(t, tc.wants, stages.Load())
		})
	}
}

// A hub without the check route (a plain 404) is an older
// hub: an authorized sender's file is staged under the plugin's own checks
// and removed if the send is refused; an unresolved sender causes no write.
func TestInboundAttachment_OlderHub(t *testing.T) {
	t.Run("authorized", func(t *testing.T) {
		hub := newIASpecHub(t, "legacy", true, "coder")
		hub.set(func() { hub.checkStatus = http.StatusNotFound })
		env := newIAEnv(t, hub)

		env.iaSendCaption(t, "please review this file")

		entries := env.telegramEntries(t)
		require.Len(t, entries, 1)
		sends := env.sends(t)
		require.Len(t, sends, 1)
		assert.Equal(t, []string{"/scion-volumes/scratchpad/.attachments/_telegram/" + entries[0] + "/notes.txt"}, sends[0].Message.Attachments)
		assert.Empty(t, hub.requestsTo(iaSpecUploadPath))
		assert.Empty(t, iaSpecFilesUnder(t, env.downloads))
	})
	t.Run("send refused", func(t *testing.T) {
		hub := newIASpecHub(t, "legacy", true, "coder")
		hub.set(func() { hub.checkStatus = http.StatusNotFound })
		hub.set(func() { hub.inboundStatus = map[string]int{"agent:coder": http.StatusForbidden} })
		env := newIAEnv(t, hub)

		env.iaSendCaption(t, "please review this file")

		require.Len(t, env.sends(t), 1)
		env.assertNoAttachmentWrites(t)
	})
	t.Run("unresolved sender", func(t *testing.T) {
		hub := newIASpecHub(t, "legacy", true, "coder")
		hub.set(func() { hub.checkStatus = http.StatusNotFound })
		env := newIASpecEnv(t, hub, true)
		env.linkGroup(t, -1001, "group")

		env.iaSendCaption(t, "please review this file")

		getFile, download := env.tg.fileCalls()
		assert.Zero(t, getFile)
		assert.Zero(t, download)
		assert.Empty(t, env.sends(t))
		env.assertNoAttachmentWrites(t)
	})
}

// A 404 carrying the hub's JSON error envelope, or any
// other failed check, is an error: the text is delivered with a notice and
// nothing is staged, downloaded or written.
func TestInboundAttachment_CheckErrorNeverStages(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		envelope bool
	}{
		{"404 with envelope", http.StatusNotFound, true},
		{"500", http.StatusInternalServerError, false},
		{"503 with envelope", http.StatusServiceUnavailable, true},
		{"400 with envelope", http.StatusBadRequest, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stages := countLegacyStages(t)
			hub := newIASpecHub(t, "legacy", true, "coder")
			hub.set(func() { hub.checkStatus = tc.status })
			hub.set(func() { hub.checkEnvelope = tc.envelope })
			env := newIAEnv(t, hub)

			env.iaSendCaption(t, "please review this file")

			getFile, download := env.tg.fileCalls()
			assert.Zero(t, getFile)
			assert.Zero(t, download)
			assert.Zero(t, stages.Load())
			sends := env.sends(t)
			require.Len(t, sends, 1, "the text is delivered")
			assert.Empty(t, sends[0].Message.Attachments)
			assert.Contains(t, env.chatTexts(), attachmentsWithheldNotice)
			env.assertNoAttachmentWrites(t)
		})
	}
}

// A message with only an attachment the hub cannot take sends nothing and
// says so.
func TestInboundAttachment_UnavailableWithoutText(t *testing.T) {
	hub := newIASpecHub(t, "unavailable", true, "coder")
	env := newIAEnv(t, hub)

	env.iaSendCaption(t, "")

	assert.Empty(t, env.sends(t))
	assert.Contains(t, env.chatTexts(), attachmentsWithheldNothingSent)
	env.assertNoAttachmentWrites(t)
}

// An upload the hub does not accept sends nothing to that recipient and
// shows the failure.
func TestInboundAttachment_ArtifactUploadFailureSendsNothing(t *testing.T) {
	hub := newIASpecHub(t, "artifact", true, "coder")
	env := newIAEnv(t, hub)
	// The check answers artifact, but the upload route refuses: the
	// sender's access changed in between.
	hub.set(func() { hub.authorized = false })
	var answered atomic.Bool
	env.b.mu.Lock()
	hubURL := env.b.hubURL
	env.b.mu.Unlock()
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == iaSpecCheckPath && answered.CompareAndSwap(false, true) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"authorized": true, "uploads": "artifact", "delivery_max_bytes": iaSpecMaxBytes})
			return
		}
		req, _ := http.NewRequestWithContext(r.Context(), r.Method, hubURL+r.URL.Path, r.Body)
		req.Header = r.Header.Clone()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		_, _ = w.Write(buf.Bytes())
	}))
	t.Cleanup(front.Close)
	env.b.mu.Lock()
	env.b.hubURL = front.URL
	env.b.mu.Unlock()

	env.iaSendCaption(t, "please review this file")

	assert.Empty(t, hub.storedUploads())
	assert.Empty(t, env.sends(t), "nothing is sent to the recipient")
	assert.True(t, anyContains(env.chatTexts(), "could not be uploaded"), "the failure is shown: %q", env.chatTexts())
	env.assertNoAttachmentWrites(t)
}

// downloads_path is retired: a configured value is ignored with a warning
// and nothing is created there.
func TestV2_Configure_DownloadsPathRetired(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	var logs bytes.Buffer
	b := NewV2(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { b.Close() })
	dir := filepath.Join(t.TempDir(), "downloads")

	require.NoError(t, b.Configure(map[string]string{
		"bot_token":      "test-token",
		"api_base_url":   tgSrv.srv.URL,
		"db_path":        filepath.Join(t.TempDir(), "test.db"),
		"downloads_path": dir,
	}))

	assert.Contains(t, logs.String(), "downloads_path setting is no longer supported")
	_, err := os.Stat(dir)
	assert.True(t, os.IsNotExist(err), "nothing is created under downloads_path")
}

func TestIsHubErrorEnvelope(t *testing.T) {
	assert.True(t, isHubErrorEnvelope([]byte(`{"error":{"code":"not_found","message":"x"}}`)))
	assert.False(t, isHubErrorEnvelope([]byte("404 page not found\n")))
	assert.False(t, isHubErrorEnvelope([]byte(`{"error":"not found"}`)))
	assert.False(t, isHubErrorEnvelope([]byte(`{"error":{"message":"no code"}}`)))
	assert.False(t, isHubErrorEnvelope(nil))
}

func TestBoundedReadCloser(t *testing.T) {
	r := newBoundedReadCloser(nopCloser{strings.NewReader("12345")}, 5)
	var buf bytes.Buffer
	_, err := buf.ReadFrom(r)
	require.NoError(t, err)
	assert.Equal(t, "12345", buf.String())

	r = newBoundedReadCloser(nopCloser{strings.NewReader("123456")}, 5)
	buf.Reset()
	_, err = buf.ReadFrom(r)
	assert.ErrorIs(t, err, errAttachmentTooLarge)
}

type nopCloser struct{ *strings.Reader }

func (nopCloser) Close() error { return nil }

// Two messages staged in one clock tick get distinct numeric entries, so
// the refusal cleanup of one never removes the other's file.
func TestNextLegacyEntry_UniqueWithinOneTick(t *testing.T) {
	orig := legacyEntryClock
	tick := time.Unix(1_900_000_000, 0)
	legacyEntryClock = func() time.Time { return tick }
	t.Cleanup(func() { legacyEntryClock = orig })

	a, b := nextLegacyEntry(), nextLegacyEntry()
	assert.NotEqual(t, a, b)
	for _, e := range []string{a, b} {
		assert.Regexp(t, `^tg_[0-9]+$`, e)
	}
}

// A document named ".." (after reducing the name to one component) is
// delivered as file_<id>, like ".".
func TestTelegramAttachmentOf_DotDotName(t *testing.T) {
	for _, name := range []string{"..", "a/..", `a\..`, "."} {
		att, ok := telegramAttachmentOf(&TGMessage{Document: &TGDocument{FileID: "doc-1", FileUniqueID: "u1", FileName: name}})
		require.True(t, ok, name)
		assert.Equal(t, "file_doc-1", att.Name, name)
	}
}

// With delivery off (delivery_max_bytes 0) the text is delivered with the
// withheld notice and nothing is downloaded, uploaded or written.
func TestInboundAttachment_DeliveryOffWithheld(t *testing.T) {
	stages := countLegacyStages(t)
	hub := newIASpecHub(t, "artifact", true, "coder")
	hub.set(func() { hub.zeroMaxBytes = true })
	env := newIAEnv(t, hub)

	env.iaSendCaption(t, "please review this file")

	getFile, download := env.tg.fileCalls()
	assert.Zero(t, getFile)
	assert.Zero(t, download)
	sends := env.sends(t)
	require.Len(t, sends, 1, "the text is delivered")
	assert.Empty(t, sends[0].AttachmentIDs)
	assert.Empty(t, hub.requestsTo(iaSpecUploadPath))
	assert.Zero(t, stages.Load())
	assert.Contains(t, env.chatTexts(), attachmentsWithheldNotice)
	env.assertNoAttachmentWrites(t)
}

// A check the hub refuses because it names too many recipients tells the
// sender so; the text is delivered without the file.
func TestInboundAttachment_TooManyRecipientsNotice(t *testing.T) {
	stages := countLegacyStages(t)
	hub := newIASpecHub(t, "artifact", true, "coder")
	hub.set(func() { hub.checkStatus = http.StatusBadRequest })
	hub.set(func() { hub.checkEnvelope = true })
	hub.set(func() { hub.checkCode = hubCodeTooManyTargets })
	env := newIAEnv(t, hub)

	env.iaSendCaption(t, "please review this file")

	_, download := env.tg.fileCalls()
	assert.Zero(t, download)
	sends := env.sends(t)
	require.Len(t, sends, 1, "the text is delivered")
	assert.Empty(t, sends[0].AttachmentIDs)
	assert.Empty(t, hub.requestsTo(iaSpecUploadPath))
	assert.Zero(t, stages.Load())
	assert.Contains(t, env.chatTexts(), attachmentsTooManyRecipientsNotice)
	assert.NotContains(t, env.chatTexts(), attachmentsWithheldNotice)
	env.assertNoAttachmentWrites(t)
}

// The fake hub refuses metadata fields out of the required order, as the
// hub does, so the plugin's own order is what the specs exercise.
func TestIASpecHub_RefusesOutOfOrderFields(t *testing.T) {
	hub := newIASpecHub(t, "artifact", true, "coder")
	post := func(fields [][2]string) int {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		for _, f := range fields {
			require.NoError(t, mw.WriteField(f[0], f[1]))
		}
		fw, err := mw.CreateFormFile("file", "a.txt")
		require.NoError(t, err)
		_, _ = fw.Write([]byte("hello"))
		require.NoError(t, mw.Close())
		resp, err := http.Post(hub.srv.URL+iaSpecUploadPath, mw.FormDataContentType(), &buf)
		require.NoError(t, err)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	inOrder := [][2]string{{"sender", iaSpecSender}, {"project_id", "p"}, {"surface", "telegram"}, {"audience", "group"}, {"targets", "coder"}}
	assert.Equal(t, http.StatusCreated, post(inOrder))
	swapped := [][2]string{{"project_id", "p"}, {"sender", iaSpecSender}, {"surface", "telegram"}, {"audience", "group"}}
	assert.Equal(t, http.StatusBadRequest, post(swapped))
	targetsEarly := [][2]string{{"sender", iaSpecSender}, {"targets", "coder"}, {"project_id", "p"}, {"surface", "telegram"}, {"audience", "group"}}
	assert.Equal(t, http.StatusBadRequest, post(targetsEarly))
}

// shortenHubTimeout makes b give up on a hub request after d.
func shortenHubTimeout(b *TelegramBrokerV2, d time.Duration) {
	b.mu.Lock()
	b.httpClient = &http.Client{Timeout: d}
	b.mu.Unlock()
}

// In legacy mode a send that gets no hub answer (the client times out) is
// an unknown outcome: the hub may still deliver the path, so the entry is
// kept, alone or next to a recipient that refused.
func TestInboundAttachment_LegacyNoAnswerKeepsEntry(t *testing.T) {
	cases := []struct {
		name    string
		caption string
		agents  []string
		refuse  map[string]int
	}{
		{"single recipient", "please review this file", []string{"coder"}, nil},
		{"unknown plus refused", "@coder @reviewer please review this file", []string{"coder", "reviewer"}, map[string]int{"agent:reviewer": http.StatusConflict}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub := newIASpecHub(t, "legacy", true, tc.agents...)
			hub.set(func() { hub.inboundDelay = map[string]time.Duration{"agent:coder": 2 * time.Second} })
			hub.set(func() { hub.inboundStatus = tc.refuse })
			env := newIAEnv(t, hub)
			shortenHubTimeout(env.b, 300*time.Millisecond)

			env.iaSendCaption(t, tc.caption)

			require.Len(t, env.telegramEntries(t), 1, "the entry is kept when a send got no answer")
			assert.NotEmpty(t, iaSpecFilesHolding(t, env.scratchpad, env.tg.content))
		})
	}
}

// An upload the hub refuses per file (an HTML document) shows the hub's
// reason in the chat; nothing is stored and nothing is sent to that agent.
func TestInboundAttachment_PerFileUploadRefusalShown(t *testing.T) {
	stages := countLegacyStages(t)
	hub := newIASpecHub(t, "artifact", true, "coder")
	env := newIAEnv(t, hub)
	env.tg.content = []byte("<html><body>hi</body></html>")

	env.b.handleIncomingMessageV2(env.b.gen, &TGMessage{
		MessageID: 77,
		From:      &TGUser{ID: 456, Username: "alice"},
		Chat:      TGChat{ID: -1001, Type: "group"},
		Date:      time.Now().Unix(),
		Caption:   "please review this page",
		Document:  &TGDocument{FileID: "page", FileUniqueID: "page-u", FileName: "page.html", FileSize: int64(len(env.tg.content))},
	})

	require.Len(t, hub.requestsTo(iaSpecUploadPath), 1, "the upload was tried")
	assert.Empty(t, hub.storedUploads(), "nothing stored")
	assert.Empty(t, env.sends(t), "nothing sent to that agent")
	assert.Zero(t, stages.Load())
	assert.True(t, anyContains(env.chatTexts(), "files with a .html extension are not accepted"), "the hub's reason is shown: %q", env.chatTexts())
	env.assertNoAttachmentWrites(t)
}

// With several recipients in artifact mode, a recipient whose upload the
// hub refuses (403) does not get the message; the others get it with their
// own upload.
func TestInboundAttachment_ArtifactRecipientUploadRefused(t *testing.T) {
	hub := newIASpecHub(t, "artifact", true, "coder", "reviewer")
	hub.set(func() { hub.uploadRefuse = map[string]int{"reviewer": http.StatusForbidden} })
	env := newIAEnv(t, hub)

	env.iaSendCaption(t, "@coder @reviewer please review this file")

	sends := env.sends(t)
	require.Len(t, sends, 1, "only the recipient whose upload was stored gets the message")
	assert.Equal(t, "agent:coder", sends[0].Message.Recipient)
	stored := hub.storedUploads()
	require.Len(t, stored, 1)
	assert.Equal(t, []string{stored[0].ID}, sends[0].AttachmentIDs)
	assert.True(t, anyContains(env.chatTexts(), "reviewer"), "the sender is told which agent missed it: %q", env.chatTexts())
	env.assertNoAttachmentWrites(t)
}

// With an in-process InboundHandler in artifact mode, the file is withheld
// before it is downloaded: the sender gets the usual notice and the
// delivered text names no attachment.
func TestInboundAttachment_InProcessHandlerWithholdsBeforeDownload(t *testing.T) {
	hub := newIASpecHub(t, "artifact", true, "coder")
	env := newIAEnv(t, hub)
	var got []string
	env.b.mu.Lock()
	env.b.InboundHandler = func(_ string, msg *messages.StructuredMessage) { got = append(got, msg.Msg) }
	env.b.mu.Unlock()

	env.iaSendCaption(t, "please review this file")

	getFile, download := env.tg.fileCalls()
	assert.Zero(t, getFile)
	assert.Zero(t, download)
	assert.Empty(t, hub.requestsTo(iaSpecUploadPath))
	require.Len(t, got, 1)
	assert.NotContains(t, got[0], "📎", "no placeholder for a withheld file")
	assert.Contains(t, env.chatTexts(), attachmentsWithheldNotice)
	env.assertNoAttachmentWrites(t)
}

// A legacy stage that fails part way (the download is over the size bound)
// leaves no entry behind.
func TestInboundAttachment_FailedLegacyStageLeavesNoEntry(t *testing.T) {
	hub := newIASpecHub(t, "legacy", true, "coder")
	env := newIAEnv(t, hub)
	env.tg.content = bytes.Repeat([]byte("x"), maxTelegramFileSize+1)
	env.tg.hideSize = true

	env.b.handleIncomingMessageV2(env.b.gen, &TGMessage{
		MessageID: 77,
		From:      &TGUser{ID: 456, Username: "alice"},
		Chat:      TGChat{ID: -1001, Type: "group"},
		Date:      time.Now().Unix(),
		Caption:   "please review this file",
		Document:  &TGDocument{FileID: "big", FileUniqueID: "big-u", FileName: "big.bin"},
	})

	_, download := env.tg.fileCalls()
	assert.Equal(t, 1, download, "the download started")
	assert.Empty(t, env.telegramEntries(t), "the partly staged entry is removed")
	assert.True(t, anyContains(env.chatTexts(), "larger than Telegram lets bots download"), "the notice names Telegram's limit: %q", env.chatTexts())
	env.assertNoAttachmentWrites(t)
}

// An entry name that already exists is not the plugin's: staging picks the
// next name, never writes into the existing entry, and the refusal cleanup
// never removes it.
func TestInboundAttachment_ExistingEntryNeverTouched(t *testing.T) {
	tick := time.Now()
	origClock := legacyEntryClock
	legacyEntryClock = func() time.Time { return tick }
	legacyEntrySeq.Lock()
	origLast := legacyEntrySeq.last
	legacyEntrySeq.last = 0
	legacyEntrySeq.Unlock()
	t.Cleanup(func() {
		legacyEntryClock = origClock
		legacyEntrySeq.Lock()
		if legacyEntrySeq.last < origLast {
			legacyEntrySeq.last = origLast
		}
		legacyEntrySeq.Unlock()
	})

	hub := newIASpecHub(t, "legacy", true, "coder")
	hub.set(func() { hub.inboundStatus = map[string]int{"agent:coder": http.StatusConflict} })
	env := newIAEnv(t, hub)
	taken := fmt.Sprintf("tg_%d", tick.UnixNano())
	takenDir := filepath.Join(env.scratchpad, ".attachments", projectfs.AttachmentChannelTelegram, taken)
	require.NoError(t, os.MkdirAll(takenDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(takenDir, "theirs.txt"), []byte("not the plugin's"), 0o644))

	env.iaSendCaption(t, "please review this file")

	sends := env.sends(t)
	require.Len(t, sends, 1)
	require.Len(t, sends[0].Message.Attachments, 1)
	assert.NotContains(t, sends[0].Message.Attachments[0], "/"+taken+"/", "the staged path is not in the existing entry")
	assert.Equal(t, []string{taken}, env.telegramEntries(t), "only the existing entry remains; the plugin's refused entry is removed")
	got, err := os.ReadDir(takenDir)
	require.NoError(t, err)
	require.Len(t, got, 1, "nothing was written into the existing entry")
	assert.Equal(t, "theirs.txt", got[0].Name())
}

// The size notice names the hub when the hub's limit applied, and
// Telegram's download limit for bots when that was the smaller one.
func TestAttachmentTooLargeText_NamesTheLimit(t *testing.T) {
	hub := attachmentTooLargeText("a.bin", 1000, limitSourceHub, true)
	assert.Contains(t, hub, "larger than agents on this hub accept (max 1000 bytes)")
	assert.Contains(t, hub, "sent without it")
	tg := attachmentTooLargeText("a.bin", maxTelegramFileSize, limitSourceTelegram, false)
	assert.Contains(t, tg, fmt.Sprintf("larger than Telegram lets bots download (max %d bytes)", maxTelegramFileSize))
	assert.Contains(t, tg, "nothing was sent")
	assert.NotContains(t, tg, "hub")
}
