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

// Envelope shape for artifact-backed and legacy attachments (ptone/scion#9694):
// artifact-backed attachments are listed once at their inbox path with no
// scratchpad projection, no attachment_mounts, and no sibling-agents note;
// legacy staged paths still carry attachment_mounts.

const (
	testInboxPath    = "~/.scion/inbox/m1/a.png"
	testLegacyStaged = "/scion-volumes/scratchpad/.attachments/_webchat/art1/a.png"
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

func planEntry(state string) messages.AttachmentPlanEntry {
	e := messages.AttachmentPlanEntry{
		Delivery: messages.AttachmentDelivery{ArtifactID: "art1", MessageID: "m1", Name: "a.png", Size: 10},
		Name:     "a.png",
		State:    state,
	}
	if state != messages.AttachmentWithheld {
		e.InboxPath = testInboxPath
	}
	return e
}

func TestRenderDeliveryText_DeliverableSingleInboxEntry(t *testing.T) {
	env := extractDeliveryEnvelope(t, RenderDeliveryText(RenderDeliveryInput{
		MessageID: "m1", Msg: projectionTestMsg(false), CreatedAt: time.Now().UTC(),
		Plan: []messages.AttachmentPlanEntry{planEntry(messages.AttachmentDeliverable)},
	}))
	if len(env.Attachments) != 1 || env.Attachments[0] != testInboxPath {
		t.Fatalf("attachments = %q, want [inbox] only", env.Attachments)
	}
	if len(env.AttachmentMounts) != 0 {
		t.Fatalf("attachment_mounts = %v, want empty", env.AttachmentMounts)
	}
	if strings.Contains(env.Msg, "also visible to sibling agents") {
		t.Fatalf("body %q must not mention sibling-agent scratchpad projection", env.Msg)
	}
}

func TestRenderDeliveryText_UndeliverableRuntimeHasNoScratchpadFallback(t *testing.T) {
	env := extractDeliveryEnvelope(t, RenderDeliveryText(RenderDeliveryInput{
		MessageID: "m1", Msg: projectionTestMsg(false), CreatedAt: time.Now().UTC(),
		Plan: []messages.AttachmentPlanEntry{planEntry(messages.AttachmentUndeliverableRuntime)},
	}))
	if len(env.Attachments) != 0 {
		t.Fatalf("attachments = %q, want empty", env.Attachments)
	}
	if len(env.AttachmentMounts) != 0 {
		t.Fatalf("attachment_mounts = %v, want empty", env.AttachmentMounts)
	}
	if !strings.Contains(env.Msg, "Attachment not delivered: a.png") {
		t.Fatalf("body %q lacks the not-delivered line", env.Msg)
	}
	if strings.Contains(env.Msg, "also visible to sibling agents") {
		t.Fatalf("body %q must not mention sibling-agent scratchpad projection", env.Msg)
	}
}

func TestRenderDeliveryText_WithheldMentionsNothing(t *testing.T) {
	text := RenderDeliveryText(RenderDeliveryInput{
		MessageID: "m1", Msg: projectionTestMsg(false), CreatedAt: time.Now().UTC(),
		Plan: []messages.AttachmentPlanEntry{planEntry(messages.AttachmentWithheld)},
	})
	if strings.Contains(text, testInboxPath) || strings.Contains(text, "a.png") {
		t.Fatalf("a withheld attachment must not be mentioned:\n%s", text)
	}
}

func TestRenderDeliveryText_PlainListsInboxOnly(t *testing.T) {
	text := RenderDeliveryText(RenderDeliveryInput{
		MessageID: "m1", Msg: projectionTestMsg(true), CreatedAt: time.Now().UTC(),
		Plan: []messages.AttachmentPlanEntry{planEntry(messages.AttachmentDeliverable)},
	})
	if !strings.Contains(text, "Attachment: "+testInboxPath) {
		t.Fatalf("plain text must list the inbox path:\n%s", text)
	}
	if strings.Contains(text, "also visible to sibling agents") {
		t.Fatalf("plain text must not include sibling-agent projection line:\n%s", text)
	}
}

func TestRenderDeliveryText_LegacyStagedPathTaggedWithMount(t *testing.T) {
	msg := projectionTestMsg(false)
	msg.Attachments = []string{testLegacyStaged, "/home/hub/.scion/attachments/p/id/b.txt"}
	msg.AttachmentMounts = map[string]string{testLegacyStaged: "scratchpad"}
	env := extractDeliveryEnvelope(t, RenderDeliveryText(RenderDeliveryInput{
		MessageID: "m1", Msg: msg, CreatedAt: time.Now().UTC(),
	}))
	if len(env.Attachments) != 2 {
		t.Fatalf("attachments = %q", env.Attachments)
	}
	if len(env.AttachmentMounts) != 1 || env.AttachmentMounts[testLegacyStaged] != "scratchpad" {
		t.Fatalf("attachment_mounts = %v, want the staged path only", env.AttachmentMounts)
	}
}

func TestApplyAttachmentPlanLegacy_InboxOnly(t *testing.T) {
	msg := projectionTestMsg(false)
	ApplyAttachmentPlanLegacy(msg, []messages.AttachmentPlanEntry{planEntry(messages.AttachmentDeliverable)})
	if len(msg.Attachments) != 1 || msg.Attachments[0] != testInboxPath {
		t.Fatalf("legacy attachments = %q, want the inbox path only", msg.Attachments)
	}
	if strings.Contains(msg.Msg, "also visible to sibling agents") {
		t.Fatalf("legacy body %q must not mention sibling-agent scratchpad projection", msg.Msg)
	}
}
