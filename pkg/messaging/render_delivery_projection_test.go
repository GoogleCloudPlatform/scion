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

package messaging

import (
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// Envelope shape for scratchpad projections (ptone/scion#5827):
// the inbox path is the primary entry, the projection a second,
// self-describing entry tagged with its mount.

const (
	testInboxPath     = "~/.scion/inbox/m1/a.png"
	testProjectedPath = "/scion-volumes/scratchpad/.attachments/_webchat/art1/a.png"
)

func projectionTestMsg(plain bool) *messages.StructuredMessage {
	return &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Sender:    "user:alice",
		Recipient: "agent:bot",
		Msg:       "see attached",
		Type:      messages.TypeInstruction,
		Plain:     plain,
	}
}

func projectedEntry(state string) messages.AttachmentPlanEntry {
	e := messages.AttachmentPlanEntry{
		Delivery:       messages.AttachmentDelivery{ArtifactID: "art1", MessageID: "m1", Name: "a.png", Size: 10},
		Name:           "a.png",
		State:          state,
		ProjectedPath:  testProjectedPath,
		ProjectedMount: "scratchpad",
	}
	if state != messages.AttachmentWithheld {
		e.InboxPath = testInboxPath
	}
	return e
}

func TestRenderDeliveryText_ProjectionAfterInboxWithMount(t *testing.T) {
	env := extractDeliveryEnvelope(t, RenderDeliveryText(RenderDeliveryInput{
		MessageID: "m1", Msg: projectionTestMsg(false), CreatedAt: time.Now().UTC(),
		Plan: []messages.AttachmentPlanEntry{projectedEntry(messages.AttachmentDeliverable)},
	}))
	if len(env.Attachments) != 2 || env.Attachments[0] != testInboxPath || env.Attachments[1] != testProjectedPath {
		t.Fatalf("attachments = %q, want [inbox, projection]", env.Attachments)
	}
	if len(env.AttachmentMounts) != 1 || env.AttachmentMounts[testProjectedPath] != "scratchpad" {
		t.Fatalf("attachment_mounts = %v, want only the projection tagged scratchpad", env.AttachmentMounts)
	}
	if !strings.Contains(env.Msg, "also visible to sibling agents at "+testProjectedPath) {
		t.Fatalf("body %q does not describe the projection", env.Msg)
	}
}

func TestRenderDeliveryText_UndeliverableKeepsProjection(t *testing.T) {
	env := extractDeliveryEnvelope(t, RenderDeliveryText(RenderDeliveryInput{
		MessageID: "m1", Msg: projectionTestMsg(false), CreatedAt: time.Now().UTC(),
		Plan: []messages.AttachmentPlanEntry{projectedEntry(messages.AttachmentUndeliverableRuntime)},
	}))
	if len(env.Attachments) != 1 || env.Attachments[0] != testProjectedPath {
		t.Fatalf("attachments = %q, want only the projection", env.Attachments)
	}
	if env.AttachmentMounts[testProjectedPath] != "scratchpad" {
		t.Fatalf("attachment_mounts = %v", env.AttachmentMounts)
	}
	if !strings.Contains(env.Msg, "Attachment not delivered: a.png") {
		t.Fatalf("body %q lacks the not-delivered line", env.Msg)
	}
}

func TestRenderDeliveryText_WithheldMentionsNoProjection(t *testing.T) {
	text := RenderDeliveryText(RenderDeliveryInput{
		MessageID: "m1", Msg: projectionTestMsg(false), CreatedAt: time.Now().UTC(),
		Plan: []messages.AttachmentPlanEntry{projectedEntry(messages.AttachmentWithheld)},
	})
	if strings.Contains(text, testProjectedPath) || strings.Contains(text, "a.png") {
		t.Fatalf("a withheld attachment must not be mentioned:\n%s", text)
	}
}

func TestRenderDeliveryText_PlainListsProjectionLine(t *testing.T) {
	text := RenderDeliveryText(RenderDeliveryInput{
		MessageID: "m1", Msg: projectionTestMsg(true), CreatedAt: time.Now().UTC(),
		Plan: []messages.AttachmentPlanEntry{projectedEntry(messages.AttachmentDeliverable)},
	})
	inbox := strings.Index(text, "Attachment: "+testInboxPath)
	proj := strings.Index(text, "also visible to sibling agents at "+testProjectedPath)
	if inbox < 0 || proj < 0 || proj < inbox {
		t.Fatalf("plain text must list the inbox path, then the projection:\n%s", text)
	}
}

func TestRenderDeliveryText_LegacyStagedPathTaggedWithMount(t *testing.T) {
	msg := projectionTestMsg(false)
	msg.Attachments = []string{testProjectedPath, "/home/hub/.scion/attachments/p/id/b.txt"}
	msg.AttachmentMounts = map[string]string{testProjectedPath: "scratchpad"}
	env := extractDeliveryEnvelope(t, RenderDeliveryText(RenderDeliveryInput{
		MessageID: "m1", Msg: msg, CreatedAt: time.Now().UTC(),
	}))
	if len(env.Attachments) != 2 {
		t.Fatalf("attachments = %q", env.Attachments)
	}
	if len(env.AttachmentMounts) != 1 || env.AttachmentMounts[testProjectedPath] != "scratchpad" {
		t.Fatalf("attachment_mounts = %v, want the staged path only", env.AttachmentMounts)
	}
}

func TestApplyAttachmentPlanLegacy_ProjectionInBodyOnly(t *testing.T) {
	msg := projectionTestMsg(false)
	ApplyAttachmentPlanLegacy(msg, []messages.AttachmentPlanEntry{projectedEntry(messages.AttachmentDeliverable)})
	if len(msg.Attachments) != 1 || msg.Attachments[0] != testInboxPath {
		t.Fatalf("legacy attachments = %q, want the inbox path only", msg.Attachments)
	}
	if !strings.Contains(msg.Msg, "also visible to sibling agents at "+testProjectedPath) {
		t.Fatalf("legacy body %q does not describe the projection", msg.Msg)
	}
}
