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
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// RenderDeliveryInput carries everything the rendering helper needs from each
// call site. No call site should build a ConversationInfo or call
// FormatNewDelivery directly — the helper owns the envelope's shape.
type RenderDeliveryInput struct {
	// MessageID is the persisted row's ID. Required — the helper will not
	// fabricate an identifier.
	MessageID string

	// ConvResult is the conversation resolution outcome. May be nil when
	// the conversation is absent or unresolvable (e.g. broadcasts). When
	// nil, the envelope omits the conversation key entirely (§4.3).
	ConvResult *ConversationResult

	// Msg is the StructuredMessage being dispatched. The helper reads
	// sender, recipient, type, body, attachments, metadata, visibility,
	// and transport flag (Plain) from it.
	Msg *messages.StructuredMessage

	// CreatedAt is the message timestamp. When zero, time.Now().UTC() is
	// used — but callers should supply the persisted row's CreatedAt.
	CreatedAt time.Time

	// IsMention, when true, overrides the Kind-based type computation:
	// the rendered envelope's "type" is "mention" rather than "message".
	// Set explicitly by the caller that made the routing decision — never
	// inferred from Msg.Type.
	IsMention bool

	// CoAddressees, when non-empty, overrides the single-recipient
	// inference in buildAddressees for the "to" field: it lists every
	// agent engaged in this message (including the recipient this
	// envelope is being rendered for). Used for both mention and
	// group-primary envelopes when multiple agents are engaged.
	CoAddressees []Addressee

	// ReplyToID is the message ID of the parent message being replied to.
	// When non-empty, the envelope's "type" is "reply" and "reply_to"
	// carries this value.
	ReplyToID string

	// Plan is this recipient's attachment plan (ptone/scion#4596): each
	// deliverable entry's inbox path is listed in the envelope's
	// attachments (or, for a plain message, as an "Attachment:" line), and
	// each entry the recipient's runtime cannot receive gets a
	// not-delivered line in the body. Withheld entries are not mentioned.
	Plan []messages.AttachmentPlanEntry
}

// RenderDeliveryText is the single shared rendering entry point for all hub
// send paths (Phase 9b(ii)). It converts a StructuredMessage and its
// conversation context into the fully rendered agent-facing envelope text.
//
// Invariants:
//   - Never fabricates an identifier. PersistedIdentity carries the real
//     message ID into MapLegacyEnvelope so that both the Message and its
//     Addressees share the same genuine identity from the persisted row.
//   - reply_to is omitted when PersistedIdentity.ReplyToID is empty. In
//     this phase no genuine reply target exists yet; absent is correct.
//   - When ConvResult is nil the envelope carries no conversation key
//     (honest absence per §4.3): convInfo is a nil *ConversationInfo and
//     the JSON tag omitempty suppresses the field.
func RenderDeliveryText(in RenderDeliveryInput) string {
	if in.Msg == nil {
		return ""
	}

	// Transport flag: plain messages deliver body text only.
	if in.Msg.Plain {
		return appendPlanLines(AppendArtifactFetchHints(in.Msg.Msg, in.Msg.Metadata), in.Plan, true)
	}

	// Pass the real persisted identity into MapLegacyEnvelope so the
	// message ID reaches both the Message and its Addressees structurally,
	// rather than overriding after the fact (which left addressees carrying
	// the fabricated ID while the message carried the real one).
	msg, addrs, err := MapLegacyEnvelope(in.Msg, PersistedIdentity{
		MessageID: in.MessageID,
		ReplyToID: in.ReplyToID,
	})
	if err != nil {
		// MapLegacyEnvelope only fails on nil input, which we checked.
		return in.Msg.Msg
	}

	// Artifact fetch hints (ptone/scion#3223) go in the delivered body only;
	// in.Msg and the persisted row keep the body as sent.
	msg.Body = AppendArtifactFetchHints(msg.Body, in.Msg.Metadata)
	msg.Body = appendPlanLines(msg.Body, in.Plan, false)
	for i := range msg.Attachments {
		if mount := in.Msg.AttachmentMounts[msg.Attachments[i].Path]; mount != "" {
			msg.Attachments[i].Mount = mount
		}
	}
	for _, e := range in.Plan {
		// The inbox path comes first: it is valid on every runtime. The
		// projection follows, tagged with its shared dir, so a recipient
		// whose runtime does not mount that dir is never left with only a
		// dangling path.
		if e.State == messages.AttachmentDeliverable {
			msg.Attachments = append(msg.Attachments, AttachmentRef{Path: e.InboxPath, Name: e.Name})
		}
		if e.State != messages.AttachmentWithheld && e.ProjectedPath != "" {
			msg.Attachments = append(msg.Attachments, AttachmentRef{Path: e.ProjectedPath, Name: e.Name, Mount: e.ProjectedMount})
		}
	}

	// Override timestamp if we have a real one from the persisted row.
	if !in.CreatedAt.IsZero() {
		msg.CreatedAt = in.CreatedAt.UTC()
	}

	// Build ConversationInfo from the enriched ConversationResult.
	// nil when ConvResult is absent — FormatNewDelivery omits the key.
	var convInfo *ConversationInfo
	if in.ConvResult != nil {
		convInfo = &ConversationInfo{
			ID:      in.ConvResult.ConversationID,
			Kind:    in.ConvResult.Kind,
			Surface: in.ConvResult.Surface,
			Name:    in.ConvResult.DisplayName,
		}
	}

	// If caller supplied explicit CoAddressees (mention routing), use
	// those instead of the legacy-inferred addrs from MapLegacyEnvelope.
	if len(in.CoAddressees) > 0 {
		addrs = in.CoAddressees
	}

	// FormatNewDelivery handles the envelope framing.
	opts := DeliveryOptions{
		Plain: in.Msg.Plain,
	}
	return FormatNewDelivery(msg, addrs, convInfo, opts, in.IsMention, in.ReplyToID != "")
}

// AppendArtifactFetchHints appends one line per artifact reference in md to
// body, so the recipient's path to the bytes is one command:
//
//	Artifact: v2 - scion artifact get scion://artifact/<id>@2
//	Artifact: current - scion artifact get scion://artifact/<id>
//
// The lines are built from the reference alone. They carry no title and
// involve no lookup: whether the recipient may read the artifact is decided
// when it runs the command. body is returned unchanged when md has no
// well-formed references.
func AppendArtifactFetchHints(body string, md map[string]string) string {
	refs, _ := artifacts.ParseMessageRefs(md[artifacts.MessageMetadataKey])
	if len(refs) == 0 {
		return body
	}
	var b strings.Builder
	b.WriteString(body)
	b.WriteString("\n")
	for _, r := range refs {
		b.WriteString("\nArtifact: ")
		if r.Seq > 0 {
			b.WriteString("v" + strconv.Itoa(r.Seq))
		} else {
			b.WriteString("current")
		}
		b.WriteString(" - scion artifact get ")
		b.WriteString(r.String())
	}
	return b.String()
}

// ConversationGetter is the minimal interface for looking up a conversation
// by ID. Used by RenderDeliveryTextWithLookup when the call site does not
// have a ConversationResult (e.g. paths from the off-limits file).
type ConversationGetter interface {
	GetConversation(ctx context.Context, id string) (*store.Conversation, error)
}

// RenderDeliveryTextWithLookup renders the delivery envelope, looking up the
// conversation by ID from the StructuredMessage when no ConversationResult is
// provided. This is the fallback path for call sites where the conversation
// resolution happens in a file that cannot be modified (e.g.
// handlers_agent_messaging.go).
//
// When the lookup fails or the ConversationID is empty, the envelope omits
// the conversation key (honest absence). The lookup failure is logged at
// DEBUG — it is not an error because broadcasts and pre-migration messages
// legitimately have no conversation.
func RenderDeliveryTextWithLookup(
	ctx context.Context,
	cg ConversationGetter,
	log *slog.Logger,
	in RenderDeliveryInput,
) string {
	if in.Msg == nil {
		return ""
	}

	// If we already have a ConvResult, use it directly.
	if in.ConvResult != nil {
		return RenderDeliveryText(in)
	}

	// Attempt to look up the conversation from the ID on the StructuredMessage.
	convID := in.Msg.ConversationID
	if convID == "" {
		// No conversation — render without it. This is the broadcast path
		// and any message where conversation resolution was skipped.
		return RenderDeliveryText(in)
	}

	conv, err := cg.GetConversation(ctx, convID)
	if err != nil || conv == nil {
		if log != nil {
			log.Debug("delivery render: conversation lookup failed, omitting conversation",
				"conversation_id", convID,
				"message_id", in.MessageID,
				"error", err)
		}
		return RenderDeliveryText(in)
	}

	in.ConvResult = &ConversationResult{
		ConversationID: conv.ID,
		ExternalRef:    conv.ExternalRef,
		Kind:           conv.Kind,
		Surface:        conv.Surface,
		DisplayName:    conv.DisplayName,
	}
	return RenderDeliveryText(in)
}

// appendPlanLines appends to body, after a blank line, one line per
// attachment of plan the recipient's runtime cannot receive and, with
// paths, one "Attachment: <path>" line per deliverable attachment (the
// plain form, which has no envelope to list them in), plus one line per
// non-withheld attachment that is also projected into a shared dir
// (AttachmentProjectedLine). Withheld entries are not mentioned. body is returned unchanged when there is nothing to add.
func appendPlanLines(body string, plan []messages.AttachmentPlanEntry, paths bool) string {
	var lines []string
	for _, e := range plan {
		switch e.State {
		case messages.AttachmentDeliverable:
			if paths {
				lines = append(lines, "Attachment: "+e.InboxPath)
			}
		case messages.AttachmentUndeliverableRuntime:
			lines = append(lines, AttachmentNotDeliveredLine(e.Name, e.Delivery.Size,
				"the recipient's runtime cannot receive attachments"))
		}
		if e.State != messages.AttachmentWithheld && e.ProjectedPath != "" {
			lines = append(lines, AttachmentProjectedLine(e.Name, e.ProjectedPath, e.ProjectedMount))
		}
	}
	if len(lines) == 0 {
		return body
	}
	return body + "\n\n" + strings.Join(lines, "\n")
}

// AttachmentProjectedLine is the line a recipient reads for an attachment
// that is also projected into a shared dir (ptone/scion#5827): the
// inbox path stays the one to use; the projection is where sibling agents
// that mount the shared dir find the same file.
func AttachmentProjectedLine(name, path, mount string) string {
	return "Attachment " + name + " is also visible to sibling agents at " + path +
		" (shared dir " + mount + ", only in containers that mount it)."
}

// Reasons a broker gives for an attachment that did not land in the
// recipient's home (ptone/scion#4596): in the not-delivered line the agent
// reads, and in the per-file outcomes the hub counts.
const (
	AttachmentReasonNotDownloaded = "could not be downloaded"
	AttachmentReasonVerification  = "failed verification"
	AttachmentReasonNotWritten    = "could not be written to your home"
)

// AttachmentNotDeliveredLine is the line a recipient reads for an
// attachment that did not reach its home (ptone/scion#4596): what it is
// (its name or inbox path, and size), why, and what to do. It names no
// command and no id.
func AttachmentNotDeliveredLine(what string, size int64, reason string) string {
	return "Attachment not delivered: " + what + " (" + FormatAttachmentSize(size) + "): " + reason + ". Ask the sender to resend it."
}

// FormatAttachmentSize renders a file size for a delivery line.
func FormatAttachmentSize(n int64) string {
	switch {
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MiB"
	case n >= 1<<10:
		return strconv.FormatFloat(float64(n)/(1<<10), 'f', 1, 64) + " KiB"
	default:
		return strconv.FormatInt(n, 10) + " bytes"
	}
}

// ApplyAttachmentPlanLegacy applies plan to msg for the legacy envelope,
// which the broker formats itself when the hub renders no delivery text:
// deliverable inbox paths are appended to msg.Attachments, and not-delivered
// and projection lines to msg.Msg (the legacy attachment list carries no
// mount, so a projection is described in the body only). msg must be the
// per-recipient message being dispatched.
func ApplyAttachmentPlanLegacy(msg *messages.StructuredMessage, plan []messages.AttachmentPlanEntry) {
	if msg == nil {
		return
	}
	for _, e := range plan {
		if e.State == messages.AttachmentDeliverable {
			msg.Attachments = append(msg.Attachments, e.InboxPath)
		}
	}
	msg.Msg = appendPlanLines(msg.Msg, plan, false)
}
