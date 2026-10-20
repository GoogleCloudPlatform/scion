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
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// One constructor for artifact attachment references (ptone/scion#4596,
// design section 3): only newArtifactAttachmentRef sets
// AttachmentRef.artifact.

// TestArtifactAttachmentRef_OnlyConstructorSetsFlag walks the non-test
// files of pkg/hub. Exactly one composite literal of AttachmentRef sets
// artifact (keyed, positionally with every field, or keyed in a
// type-elided element of a slice or map literal), and it is in
// newArtifactAttachmentRef; no assignment outside that function has a
// selector .artifact on its left-hand side.
func TestArtifactAttachmentRef_OnlyConstructorSetsFlag(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	var literals, assignments []string
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		require.NoError(t, err)
		file, err := parser.ParseFile(fset, path, src, 0)
		require.NoError(t, err, path)
		for _, decl := range file.Decls {
			fn, _ := decl.(*ast.FuncDecl)
			name := ""
			if fn != nil {
				name = fn.Name.Name
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CompositeLit:
					// A literal typed AttachmentRef, or a type-elided one (an
					// element of []AttachmentRef{...} or of a map literal)
					// that keys a field named artifact: AttachmentRef is the
					// only struct in pkg/hub with that field.
					typed := false
					if id, ok := x.Type.(*ast.Ident); ok && id.Name == "AttachmentRef" {
						typed = true
					}
					if !typed && x.Type != nil {
						return true
					}
					sets := false
					for _, elt := range x.Elts {
						kv, ok := elt.(*ast.KeyValueExpr)
						if !ok {
							if !typed {
								continue
							}
							// An unkeyed literal names every field, artifact included.
							sets = true
							break
						}
						if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "artifact" {
							sets = true
						}
					}
					if sets {
						literals = append(literals, name+"@"+fset.Position(x.Pos()).String())
					}
				case *ast.AssignStmt:
					for _, lhs := range x.Lhs {
						if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "artifact" {
							assignments = append(assignments, name+"@"+fset.Position(x.Pos()).String())
						}
					}
				}
				return true
			})
		}
	}
	require.Len(t, literals, 1, "AttachmentRef literals that set artifact: %v", literals)
	require.True(t, strings.HasPrefix(literals[0], "newArtifactAttachmentRef@"),
		"the one literal that sets artifact is outside the constructor: %s", literals[0])
	require.Empty(t, assignments, "assignments to .artifact outside the constructor")
}

// TestArtifactAttachmentRef_ReconstructionNeverFlags: references decoded
// from message metadata naming an artifact id, and a JSON round trip of a
// flagged reference, are never flagged. (The history merge is covered by
// TestArtifactAttachmentRef_HistoryNeverFlags.)
func TestArtifactAttachmentRef_ReconstructionNeverFlags(t *testing.T) {
	id := "5f1c2d3e-0000-4000-8000-0000000000ab"
	flagged := newArtifactAttachmentRef(artifacts.AttachmentInfo{ID: id, Name: "a.txt", MediaType: "text/plain", Size: 3})
	require.True(t, flagged.artifact)

	encoded, ok := attachmentRefsMetadata([]AttachmentRef{flagged})
	require.True(t, ok)
	parsed := parseAttachmentRefs(map[string]string{attachmentsMetadataKey: encoded})
	require.Len(t, parsed, 1)
	require.Equal(t, id, parsed[0].ID)
	require.False(t, parsed[0].artifact, "parseAttachmentRefs never flags a reference")

	// A hand-written metadata value that tries to set the field.
	parsed = parseAttachmentRefs(map[string]string{attachmentsMetadataKey: `[{"id":"` + id + `","artifact":true}]`})
	require.Len(t, parsed, 1)
	require.False(t, parsed[0].artifact, "JSON cannot set the unexported field")

	raw, err := json.Marshal(flagged)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "artifact")
	var back AttachmentRef
	require.NoError(t, json.Unmarshal(raw, &back))
	require.False(t, back.artifact, "a JSON round trip drops the flag")
}

// TestPublishUserMessage_ArtifactAttachmentsOnChatSubjectsOnly extends the
// subject pin of TestPublishUserMessage_ArtifactRefsOnChatSubjectsOnly to
// attachment references: a flagged reference reaches only the chat
// subjects (project.<id>.chat.message, user.<id>.chat.dm, and
// user.<id>.chat.message through the member fan-out), never
// agent.<id>.message, user.<id>.message or project.<id>.user.message; an
// AttachmentStore reference reaches every subject.
func TestPublishUserMessage_ArtifactAttachmentsOnChatSubjectsOnly(t *testing.T) {
	artifactID := "5f1c2d3e-0000-4000-8000-0000000000ac"
	legacyID := "6a1c2d3e-0000-4000-8000-0000000000ad"
	refs := []AttachmentRef{
		{ID: legacyID, Name: "legacy.txt"},
		newArtifactAttachmentRef(artifacts.AttachmentInfo{ID: artifactID, Name: "stored.txt"}),
	}
	userID := "11111111-0000-4000-8000-000000000001"
	agentID := "22222222-0000-4000-8000-000000000002"
	topic := "33333333-0000-4000-8000-000000000003"
	dmKey := "dm:agent:" + agentID + ":user:" + userID
	message := func(id, thread string) *store.Message {
		return &store.Message{
			ID: id, ProjectID: "p1", Sender: "agent:coder", SenderID: agentID,
			Recipient: "user:alice", RecipientID: userID, AgentID: agentID, Msg: "see this",
			Channel: "web", ThreadID: thread, CreatedAt: time.Now(),
		}
	}
	others := []string{"user." + userID + ".message", "project.p1.user.message", "agent." + agentID + ".message"}
	for _, tc := range []struct {
		name    string
		publish func(pub *ChannelEventPublisher)
		carry   []string
		omit    []string
	}{
		{
			name: "topic thread",
			publish: func(pub *ChannelEventPublisher) {
				pub.PublishUserMessage(context.Background(), message("m-topic", topic), refs, nil)
			},
			carry: []string{"project.p1.chat.message"},
			omit:  others,
		},
		{
			name: "dm",
			publish: func(pub *ChannelEventPublisher) {
				pub.PublishUserMessage(context.Background(), message("m-dm", dmKey), refs, nil)
			},
			carry: []string{"user." + userID + ".chat.dm", "user." + agentID + ".chat.dm"},
			omit:  others,
		},
		{
			name: "member fan-out",
			publish: func(pub *ChannelEventPublisher) {
				pub.PublishChatMemberMessage(context.Background(), message("m-member", topic), refs, nil, []string{"u2"})
			},
			carry: []string{"user.u2.chat.message"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pub := NewChannelEventPublisher()
			defer pub.Close()
			chans := map[string]<-chan Event{}
			for _, subj := range append(append([]string{}, tc.carry...), tc.omit...) {
				ch, unsub := pub.Subscribe(subj)
				t.Cleanup(unsub)
				chans[subj] = ch
			}
			tc.publish(pub)
			for _, subj := range tc.carry {
				evt := receiveEventWithin(t, chans[subj], subj)
				require.Contains(t, string(evt.Data), artifactID, subj)
				require.Contains(t, string(evt.Data), legacyID, subj)
			}
			for _, subj := range tc.omit {
				evt := receiveEventWithin(t, chans[subj], subj)
				require.NotContains(t, string(evt.Data), artifactID, "%s carries no artifact attachment", subj)
				require.Contains(t, string(evt.Data), legacyID, subj)
			}
		})
	}
}

// receiveEventWithin returns the next event on ch, failing after a second.
func receiveEventWithin(t *testing.T, ch <-chan Event, subject string) Event {
	t.Helper()
	select {
	case evt := <-ch:
		return evt
	case <-time.After(time.Second):
		t.Fatalf("no event on %s", subject)
		return Event{}
	}
}
