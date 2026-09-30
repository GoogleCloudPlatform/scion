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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// ---------------------------------------------------------------------------
// Large-DM offload (ptone/scion#2257, design: auto-offload large agent DM
// bodies §4.1)
//
// OffloadForDelivery replaces the dispatched copy of an over-threshold body
// with a short stub: size, a preview, and one command the recipient can run
// to fetch the full body from the hub. The persisted row and every observer
// (web, SSE, plugin observers) always keep the full body — only the
// dispatched copy is affected.
// ---------------------------------------------------------------------------

// Hub-reserved metadata keys. Clients may never set them; StripReservedMetadata
// and OffloadForDelivery both scrub them so a spoofed client value can never
// reach an agent (§4.2).
const (
	MetaBodyOffloaded = "body_offloaded"
	MetaBodyChars     = "body_chars"
	MetaBodySHA256    = "body_sha256"
)

// reservedMetadataKeys is the set backing StripReservedMetadata.
var reservedMetadataKeys = map[string]bool{
	MetaBodyOffloaded: true,
	MetaBodyChars:     true,
	MetaBodySHA256:    true,
}

// DefaultPreviewBudgetBytes is the compiled default preview byte budget
// (§5). OffloadPolicy.PreviewBudgetBytes falls back to this value when <= 0.
const DefaultPreviewBudgetBytes = 768

// OffloadPolicy configures OffloadForDelivery and Qualifies.
type OffloadPolicy struct {
	// ThresholdRunes is the rune-count threshold above which a body is
	// offloaded. <= 0 disables offload (negative is treated as 0).
	ThresholdRunes int

	// PreviewBudgetBytes is the byte budget for the JSON-escaped preview.
	// <= 0 falls back to DefaultPreviewBudgetBytes.
	PreviewBudgetBytes int
}

// OffloadInput carries everything OffloadForDelivery needs from a call site.
type OffloadInput struct {
	// Msg is the dispatched copy of the StructuredMessage. Raw and Plain are
	// assumed already normalized by the caller.
	Msg *messages.StructuredMessage

	// PersistedBody is store.Message.Msg exactly as written to the row. All
	// size, hash, and preview computation uses this value, never Msg.Msg —
	// the two can legitimately differ (chat v2, §4.1).
	PersistedBody string

	// MessageID is the persisted row's ID. Empty means "not persisted" —
	// never offload.
	MessageID string

	// ConversationID is the conversation stamped on the row, or "" when
	// there is none.
	ConversationID string

	// RecipientCanReadConv is true when the call site has verified the
	// recipient may read ConversationID (recipientCanReadConversation).
	RecipientCanReadConv bool

	// FetchByID selects the by-ID stub form (Phase 3) when true. P1/P2 call
	// sites always pass the literal false — no operational setting can
	// produce true before the Phase 3 CLI and route exist.
	FetchByID bool
}

// OffloadResult reports what OffloadForDelivery did.
type OffloadResult struct {
	Offloaded bool
	Chars     int
	SHA256    string
}

// Qualifies reports whether a body would be offloaded, ignoring the fetch
// form. Call sites use it to skip extra work (a conversation read,
// reordering persistence) on the common small-message path.
//
// The UTF-8 check keeps the fetched bytes equal to the hashed bytes: a body
// with invalid UTF-8 would come back as U+FFFD through JSON transport.
func Qualifies(persistedBody string, raw, plain bool, p OffloadPolicy) bool {
	if raw || plain {
		return false
	}
	if p.ThresholdRunes <= 0 {
		return false
	}
	// A string's rune count never exceeds its byte length (each rune is at
	// least one byte, and RuneCountInString counts one rune per invalid
	// byte too), so a body no longer than the threshold in bytes can never
	// qualify. This lets the common small-message path skip the UTF-8
	// validity and rune-count passes below.
	if len(persistedBody) <= p.ThresholdRunes {
		return false
	}
	if !utf8.ValidString(persistedBody) {
		return false
	}
	return utf8.RuneCountInString(persistedBody) > p.ThresholdRunes
}

// StripReservedMetadata returns md unchanged if it has none of the three
// hub-reserved keys. Otherwise it returns a NEW map without them. It never
// mutates md, so it is safe on aliased maps. Callers assign the result:
// sm.Metadata = messaging.StripReservedMetadata(sm.Metadata).
func StripReservedMetadata(md map[string]string) map[string]string {
	hasReserved := false
	for k := range md {
		if reservedMetadataKeys[k] {
			hasReserved = true
			break
		}
	}
	if !hasReserved {
		return md
	}
	out := make(map[string]string, len(md))
	for k, v := range md {
		if reservedMetadataKeys[k] {
			continue
		}
		out[k] = v
	}
	return out
}

// OffloadForDelivery ALWAYS returns a fresh copy (struct copy plus a deep
// copy of Metadata) with the three hub-reserved keys deleted. It never
// mutates the input: not the caller's metadata map, not the persisted row.
//
// If the body qualifies and a usable fetch form exists — FetchByID, or a
// non-empty ConversationID the recipient can read — and MessageID != "", the
// copy's Msg is replaced by the stub and the reserved keys are set.
// Threshold, body_chars, body_sha256 and the preview are ALL computed from
// PersistedBody, never from Msg.Msg. Raw, Plain and Urgent on the copy
// always equal the input's.
func OffloadForDelivery(in OffloadInput, p OffloadPolicy) (*messages.StructuredMessage, OffloadResult) {
	var out messages.StructuredMessage
	if in.Msg != nil {
		out = *in.Msg
	}

	// Always a fresh map: strip (cheap no-op when clean), then clone
	// unconditionally so the returned copy never aliases the caller's map.
	stripped := StripReservedMetadata(out.Metadata)
	if stripped != nil {
		cloned := make(map[string]string, len(stripped))
		for k, v := range stripped {
			cloned[k] = v
		}
		out.Metadata = cloned
	} else {
		out.Metadata = nil
	}

	result := OffloadResult{}

	if !Qualifies(in.PersistedBody, out.Raw, out.Plain, p) {
		return &out, result
	}
	if in.MessageID == "" {
		return &out, result
	}
	usable := in.FetchByID || (in.ConversationID != "" && in.RecipientCanReadConv)
	if !usable {
		return &out, result
	}

	chars := utf8.RuneCountInString(in.PersistedBody)
	sum := sha256.Sum256([]byte(in.PersistedBody))
	shaHex := hex.EncodeToString(sum[:])

	previewBudget := p.PreviewBudgetBytes
	if previewBudget <= 0 {
		previewBudget = DefaultPreviewBudgetBytes
	}

	var stub string
	if in.FetchByID {
		stub = buildStub(in.PersistedBody, chars, in.MessageID, stubFormByID, "", previewBudget)
	} else {
		stub = buildStub(in.PersistedBody, chars, in.MessageID, stubFormConversation, in.ConversationID, previewBudget)
	}

	out.Msg = stub
	if out.Metadata == nil {
		out.Metadata = make(map[string]string, 3)
	}
	out.Metadata[MetaBodyOffloaded] = "true"
	out.Metadata[MetaBodyChars] = strconv.Itoa(chars)
	out.Metadata[MetaBodySHA256] = shaHex

	result.Offloaded = true
	result.Chars = chars
	result.SHA256 = shaHex
	return &out, result
}

// stubForm selects which fetch command the stub names.
type stubForm int

const (
	// stubFormConversation is the Phase 1/2 form:
	// scion conversation get-message conv:<conv-id> <msg-id> --body
	stubFormConversation stubForm = iota
	// stubFormByID is the Phase 3 form: scion messages get <msg-id>.
	// Unreachable in P1/P2 — no call site ever passes FetchByID: true.
	stubFormByID
)

// buildStub renders the stub text: a header with size, an optional preview,
// and exactly one fetch command.
func buildStub(persistedBody string, chars int, messageID string, form stubForm, conversationID string, previewBudget int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[Large message: %d characters. Not pasted; the full body is stored on the hub.]", chars)

	if preview := buildPreview(persistedBody, previewBudget); preview != "" {
		b.WriteString("\n\nPreview:\n----\n")
		b.WriteString(preview)
		b.WriteString("\n----")
	}

	b.WriteString("\n\nFull body (prints only the body):\n  ")
	switch form {
	case stubFormByID:
		fmt.Fprintf(&b, "scion messages get %s", messageID)
	default:
		fmt.Fprintf(&b, "scion conversation get-message conv:%s %s --body", conversationID, messageID)
	}
	return b.String()
}

// buildPreview takes whole runes from the start of body while the escaped
// width (as Go's json.Marshal with HTML escaping on would write it) stays
// <= budgetBytes. It never splits a rune.
//
// If the cut is mid-line and there is a newline at or after 50% of the taken
// runes, the preview is cut back to it (so a newline at rune position 1
// cannot produce a 1-character preview — 50% of a large taken-rune count is
// far above 1). Otherwise it hard-cuts at the rune boundary.
//
// Returns "" if body contains NUL anywhere, or if nothing could be taken.
func buildPreview(body string, budgetBytes int) string {
	if budgetBytes <= 0 {
		return ""
	}
	if strings.IndexByte(body, 0) != -1 {
		return ""
	}

	var (
		offset       int
		width        int
		runeCount    int
		lastNLRunes  = -1
		lastNLOffset = -1
	)
	for offset < len(body) {
		r, size := utf8.DecodeRuneInString(body[offset:])
		w := runeEscapedWidth(r, size)
		if width+w > budgetBytes {
			break
		}
		width += w
		offset += size
		runeCount++
		if r == '\n' {
			lastNLRunes = runeCount
			lastNLOffset = offset
		}
	}

	if offset == len(body) {
		// The whole body fit under budget — nothing was truncated.
		return body
	}
	if offset == 0 {
		return ""
	}
	if lastNLOffset >= 0 && lastNLRunes*2 >= runeCount {
		return body[:lastNLOffset]
	}
	return body[:offset]
}

// runeEscapedWidth returns the number of bytes Go's json.Marshal (HTML
// escaping on, Go >= 1.22) writes for rune r, decoded with byte length size
// (size == 1 with r == utf8.RuneError signals an invalid input byte, which
// json.Marshal replaces with the 6-byte "�" escape).
func runeEscapedWidth(r rune, size int) int {
	if r == utf8.RuneError && size <= 1 {
		return 6 // invalid UTF-8 byte -> �
	}
	switch r {
	case '"', '\\', '\n', '\r', '\t', '\b', '\f':
		return 2
	case '<', '>', '&', ' ', ' ':
		return 6
	}
	if r < 0x20 {
		return 6 // other C0 control
	}
	if r < 0x80 {
		return 1 // DEL and other ASCII
	}
	return size // other runes: their UTF-8 length
}
