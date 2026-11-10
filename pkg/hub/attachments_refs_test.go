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
// files of pkg/hub (and its guardScanSubdirs subpackages). Exactly one
// composite literal of AttachmentRef sets artifact (keyed, positionally with
// every field, or keyed in a type-elided element of a slice or map literal),
// and it is in newArtifactAttachmentRef; no assignment outside that function
// has a selector .artifact on its left-hand side.
func TestArtifactAttachmentRef_OnlyConstructorSetsFlag(t *testing.T) {
	// pkgmove:scan-covers pkg/hub/apierr
	// scanned through guardScanSubdirs; guards AttachmentRef.artifact writes.
	// pkgmove:scan-covers pkg/hub/internal/principal
	// scanned through guardScanSubdirs; guards AttachmentRef.artifact writes.
	// pkgmove:scan-covers pkg/hub/maintenance
	// scanned through guardScanSubdirs once it exists; guards AttachmentRef.artifact writes.
	paths, err := globGuardScanDirs(".", "*.go")
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

// hubCallerFuncs returns, for every call in the non-test files of pkg/hub
// (and its guardScanSubdirs subpackages) whose callee name is one of names
// (a plain identifier or the selector of a method value), the "file:func" it
// is made from.
func hubCallerFuncs(t *testing.T, names ...string) map[string][]string {
	t.Helper()
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	// pkgmove:scan-covers pkg/hub/apierr
	// scanned through guardScanSubdirs; guards the attachment read-decision and delivery call sites.
	// pkgmove:scan-covers pkg/hub/internal/principal
	// scanned through guardScanSubdirs; guards the attachment read-decision and delivery call sites.
	// pkgmove:scan-covers pkg/hub/maintenance
	// scanned through guardScanSubdirs once it exists; guards the attachment read-decision and delivery call sites.
	paths, err := globGuardScanDirs(".", "*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	out := map[string][]string{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		require.NoError(t, err, path)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				name := ""
				switch f := call.Fun.(type) {
				case *ast.Ident:
					name = f.Name
				case *ast.SelectorExpr:
					name = f.Sel.Name
				}
				if want[name] {
					out[name] = append(out[name], path+":"+fn.Name.Name)
				}
				return true
			})
		}
	}
	return out
}

// TestOpenBoundAttachment_OnlyFromSendPath: OpenBoundAttachment checks no
// caller, so pkg/hub calls it only from stageArtifactAttachment, and that
// only from stageChatAttachments (design section 2).
func TestOpenBoundAttachment_OnlyFromSendPath(t *testing.T) {
	sites := hubCallerFuncs(t, "OpenBoundAttachment", "stageArtifactAttachment")
	require.Equal(t, []string{"attachments_artifacts.go:stageArtifactAttachment"}, sites["OpenBoundAttachment"])
	require.Equal(t, []string{"handlers_chat_v2.go:stageChatAttachments"}, sites["stageArtifactAttachment"])
	// The capability redemption route is the one other caller-less read of
	// a bound attachment (ptone/scion#4596, section 8.3).
	require.Equal(t, []string{"attachment_delivery.go:handleAttachmentDelivery"}, hubCallerFuncs(t, "ServeBoundAttachment")["ServeBoundAttachment"])
}

// attachmentDecisionPaths are the artifact attachment paths of
// TestAttachmentReadDecision_NoSecondRule: a file ("file.go:*") or one
// function of a file ("file.go:func").
var attachmentDecisionPaths = []string{
	"attachments_artifacts.go:*",
	"attachments_agent_upload.go:*",
	"attachment_delivery.go:*",
}

// TestAttachmentReadDecision_NoSecondRule (required by the design, section
// 2): on the artifact attachment paths, pkg/hub makes no call to the
// message read helpers; the read decision is the artifact service's rule
// over artifactHost, and nothing else. The legacy path (canReadAttachment)
// is out of scope.
func TestAttachmentReadDecision_NoSecondRule(t *testing.T) {
	sites := hubCallerFuncs(t, "canReadNativeMessage", "canUserReadMessage", "tokenMayReadNativeMessage", "ListAddressees")
	for helper, list := range sites {
		for _, site := range list {
			file, fn, _ := strings.Cut(site, ":")
			for _, p := range attachmentDecisionPaths {
				pf, pfn, _ := strings.Cut(p, ":")
				if file == pf && (pfn == "*" || pfn == fn) {
					t.Errorf("%s calls %s: the attachment read decision is artifactHost's alone", site, helper)
				}
			}
		}
	}
}

// TestAttachmentDecision_OnlyFromDeliveryComponent: the hub asks the read
// decision for someone other than the request's caller in exactly one
// place: attachmentDecisionForRecipient, built only by the delivery
// component's Plan and Issue (ptone/scion#4596, section 8.2).
func TestAttachmentDecision_OnlyFromDeliveryComponent(t *testing.T) {
	sites := hubCallerFuncs(t, "attachmentDecisionForRecipient")["attachmentDecisionForRecipient"]
	require.ElementsMatch(t, []string{"attachment_delivery.go:Plan", "attachment_delivery.go:Issue"}, sites)
}

// renderNeverCarries are the RenderDeliveryText call sites (file:function)
// whose messages never carry attachments stored as artifacts, each with
// the reason (ptone/scion#4596, section 8.2). Every other call site passes
// the recipient's attachment plan.
var renderNeverCarries = map[string]string{
	"handlers_agent_messaging.go:handleGroupMessage":            "hub group sends refuse attachment_ids",
	"handlers_agent_messaging.go:broadcastDirect":               "project broadcasts refuse attachment_ids",
	"handlers_agent_messaging.go:processMentions":               "human and broker mention fan-out is out of phase 2",
	"messagebroker.go:deliverToAgent":                           "artifact attachments never travel on the bus",
	"handlers_broker_inbound_routed.go:dispatchRoutedRecipient": "chat plugins move attachments in a later phase",
	"notifications.go:dispatchToAgent":                          "notifications carry no attachments",
	"server.go:messageEventHandler":                             "event notices carry no attachments",
	"artifacts_review.go:deliverArtifactReview":                 "artifact review notices carry no attachments",
}

// renderSite is one RenderDeliveryText call: its file:function and whether
// it passes a plan.
type renderSite struct {
	where string
	plan  bool
}

// renderSites returns every RenderDeliveryText call in the non-test files
// of pkg/hub (and its guardScanSubdirs subpackages). A call passes a plan
// when its argument is a RenderDeliveryInput literal keying Plan, or a
// variable the function assigns such a literal.
func renderSites(t *testing.T) []renderSite {
	t.Helper()
	// pkgmove:scan-covers pkg/hub/apierr
	// scanned through guardScanSubdirs; guards RenderDeliveryText call sites.
	// pkgmove:scan-covers pkg/hub/internal/principal
	// scanned through guardScanSubdirs; guards RenderDeliveryText call sites.
	// pkgmove:scan-covers pkg/hub/maintenance
	// scanned through guardScanSubdirs once it exists; guards RenderDeliveryText call sites.
	paths, err := globGuardScanDirs(".", "*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	var out []renderSite
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		require.NoError(t, err, path)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			hasPlanLit := func(lit *ast.CompositeLit) bool {
				for _, elt := range lit.Elts {
					if kv, ok := elt.(*ast.KeyValueExpr); ok {
						if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Plan" {
							return true
						}
					}
				}
				return false
			}
			// varHasPlan records, for each variable assigned a
			// RenderDeliveryInput literal in this function, whether that
			// literal keys Plan.
			varHasPlan := map[string]bool{}
			ast.Inspect(fn, func(n ast.Node) bool {
				as, ok := n.(*ast.AssignStmt)
				if !ok || len(as.Lhs) != len(as.Rhs) {
					return true
				}
				for i, rhs := range as.Rhs {
					lit, ok := rhs.(*ast.CompositeLit)
					if !ok {
						continue
					}
					sel, ok := lit.Type.(*ast.SelectorExpr)
					id, isIdent := as.Lhs[i].(*ast.Ident)
					if ok && isIdent && sel.Sel.Name == "RenderDeliveryInput" {
						varHasPlan[id.Name] = hasPlanLit(lit)
					}
				}
				return true
			})
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "RenderDeliveryText" || len(call.Args) != 1 {
					return true
				}
				site := renderSite{where: path + ":" + fn.Name.Name}
				switch arg := call.Args[0].(type) {
				case *ast.CompositeLit:
					site.plan = hasPlanLit(arg)
				case *ast.Ident:
					site.plan = varHasPlan[arg.Name]
				}
				out = append(out, site)
				return true
			})
		}
	}
	return out
}

// TestRenderSites_ApplyAttachmentPlan (closed list): every
// RenderDeliveryText call in pkg/hub passes the recipient's attachment plan
// or is on renderNeverCarries; a new call site fails until it is
// classified.
func TestRenderSites_ApplyAttachmentPlan(t *testing.T) {
	sites := renderSites(t)
	require.NotEmpty(t, sites)
	planned := 0
	for _, s := range sites {
		if _, never := renderNeverCarries[s.where]; never {
			require.False(t, s.plan, "%s is on the never-carries list but passes a plan", s.where)
			continue
		}
		require.True(t, s.plan, "%s renders a delivery without the attachment plan; pass Plan or classify it in renderNeverCarries", s.where)
		planned++
	}
	require.Equal(t, 7, planned, "the seven delivery render sites pass the plan")
}

// TestNeverCarriesSites_NoDeliveries: the functions on the never-carries
// list, in pkg/hub and its guardScanSubdirs subpackages, never set
// AttachmentDeliveries.
func TestNeverCarriesSites_NoDeliveries(t *testing.T) {
	// pkgmove:scan-covers pkg/hub/apierr
	// scanned through guardScanSubdirs; guards never-carries render sites.
	// pkgmove:scan-covers pkg/hub/internal/principal
	// scanned through guardScanSubdirs; guards never-carries render sites.
	// pkgmove:scan-covers pkg/hub/maintenance
	// scanned through guardScanSubdirs once it exists; guards never-carries render sites.
	paths, err := globGuardScanDirs(".", "*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		require.NoError(t, err, path)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			where := path + ":" + fn.Name.Name
			if _, never := renderNeverCarries[where]; !never {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.AssignStmt:
					for _, lhs := range x.Lhs {
						if sel, ok := lhs.(*ast.SelectorExpr); ok && (sel.Sel.Name == "AttachmentDeliveries" || sel.Sel.Name == "AttachmentPlan") {
							t.Errorf("%s sets %s", where, sel.Sel.Name)
						}
					}
				case *ast.KeyValueExpr:
					if k, ok := x.Key.(*ast.Ident); ok && (k.Name == "AttachmentDeliveries" || k.Name == "AttachmentPlan" || k.Name == "Deliveries" || k.Name == "AttachRefs") {
						t.Errorf("%s sets %s", where, k.Name)
					}
				}
				return true
			})
		}
	}
}
