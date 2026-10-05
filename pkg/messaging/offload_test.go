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
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// U1: threshold edges, invalid UTF-8, StripReservedMetadata contract.
// ---------------------------------------------------------------------------

func TestQualifies_ThresholdEdges(t *testing.T) {
	const threshold = 100
	pol := OffloadPolicy{ThresholdRunes: threshold}

	below := strings.Repeat("a", threshold-1)
	at := strings.Repeat("a", threshold)
	above := strings.Repeat("a", threshold+1)

	assert.False(t, Qualifies(below, false, pol), "threshold-1 must not qualify")
	assert.False(t, Qualifies(at, false, pol), "exactly threshold must not qualify")
	assert.True(t, Qualifies(above, false, pol), "threshold+1 must qualify")

	// Multi-byte and 4-byte runes: rune count, not byte count, drives the edge.
	multiByte := strings.Repeat("é", threshold+1) // 2 bytes/rune
	fourByte := strings.Repeat("🚀", threshold+1)  // 4 bytes/rune
	assert.True(t, Qualifies(multiByte, false, pol))
	assert.True(t, Qualifies(fourByte, false, pol))

	// Byte length (12) exceeds a threshold of 10 while the rune count (6)
	// does not: the byte-length early return in Qualifies must not fire
	// here, and the rune-based check it falls through to must say no.
	shortThreshold := OffloadPolicy{ThresholdRunes: 10}
	assert.False(t, Qualifies(strings.Repeat("é", 6), false, shortThreshold),
		"byte length over threshold but rune count under threshold must not qualify")
}

func TestQualifies_ThresholdZeroOrNegativeNeverOffloads(t *testing.T) {
	long := strings.Repeat("a", 100000)
	assert.False(t, Qualifies(long, false, OffloadPolicy{ThresholdRunes: 0}))
	assert.False(t, Qualifies(long, false, OffloadPolicy{ThresholdRunes: -5}))
}

func TestQualifies_InvalidUTF8NeverQualifies(t *testing.T) {
	pol := OffloadPolicy{ThresholdRunes: 10}
	invalid := strings.Repeat("a", 20) + "\xff\xfe" + strings.Repeat("b", 20)
	assert.False(t, Qualifies(invalid, false, pol))
}

func TestQualifies_PlainNeverQualifies(t *testing.T) {
	pol := OffloadPolicy{ThresholdRunes: 10}
	long := strings.Repeat("a", 1000)
	assert.False(t, Qualifies(long, true, pol), "plain must never qualify")
}

func TestStripReservedMetadata_NoMutationSameMapWhenClean(t *testing.T) {
	md := map[string]string{"foo": "bar"}
	out := StripReservedMetadata(md)
	assert.True(t, sameMap(md, out), "clean map must be returned unchanged (same reference)")

	// nil map stays nil.
	assert.Nil(t, StripReservedMetadata(nil))
}

func TestStripReservedMetadata_NewMapWhenReservedPresent(t *testing.T) {
	md := map[string]string{
		"foo":             "bar",
		MetaBodyOffloaded: "true",
		MetaBodyChars:     "123",
		MetaBodySHA256:    "deadbeef",
	}
	original := map[string]string{}
	for k, v := range md {
		original[k] = v
	}

	out := StripReservedMetadata(md)
	assert.False(t, sameMap(md, out), "map with reserved keys must return a distinct map")
	assert.Equal(t, map[string]string{"foo": "bar"}, out)
	// The input must never be mutated.
	assert.Equal(t, original, md)
}

// sameMap reports whether a and b are backed by the same underlying map by
// writing through a and observing b — a cheap reference-identity probe since
// Go maps aren't otherwise comparable.
func sameMap(a, b map[string]string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	const probeKey = "__sameMap_probe__"
	if _, exists := a[probeKey]; exists {
		return false // shouldn't happen in these tests
	}
	a[probeKey] = "x"
	_, seen := b[probeKey]
	delete(a, probeKey)
	return seen
}

// ---------------------------------------------------------------------------
// U2: guards. Plain, empty body, empty MessageID, unusable conversation
// form all suppress offload; Plain/Urgent are always preserved on the copy.
// ---------------------------------------------------------------------------

func TestOffloadForDelivery_Guards(t *testing.T) {
	pol := OffloadPolicy{ThresholdRunes: 10}
	long := strings.Repeat("a", 1000)

	cases := []struct {
		name string
		in   OffloadInput
	}{
		{
			name: "plain",
			in: OffloadInput{
				Msg:                  &messages.StructuredMessage{Msg: long, Plain: true},
				PersistedBody:        long,
				MessageID:            "msg-1",
				ConversationID:       "conv-1",
				RecipientCanReadConv: true,
			},
		},
		{
			name: "empty body",
			in: OffloadInput{
				Msg:                  &messages.StructuredMessage{Msg: ""},
				PersistedBody:        "",
				MessageID:            "msg-1",
				ConversationID:       "conv-1",
				RecipientCanReadConv: true,
			},
		},
		{
			name: "empty message id",
			in: OffloadInput{
				Msg:                  &messages.StructuredMessage{Msg: long},
				PersistedBody:        long,
				MessageID:            "",
				ConversationID:       "conv-1",
				RecipientCanReadConv: true,
			},
		},
		{
			name: "empty conversation id, no fetch by id",
			in: OffloadInput{
				Msg:                  &messages.StructuredMessage{Msg: long},
				PersistedBody:        long,
				MessageID:            "msg-1",
				ConversationID:       "",
				RecipientCanReadConv: true,
			},
		},
		{
			name: "recipient cannot read conversation, no fetch by id",
			in: OffloadInput{
				Msg:                  &messages.StructuredMessage{Msg: long},
				PersistedBody:        long,
				MessageID:            "msg-1",
				ConversationID:       "conv-1",
				RecipientCanReadConv: false,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, result := OffloadForDelivery(tc.in, pol)
			assert.False(t, result.Offloaded, "must not offload")
			assert.Equal(t, tc.in.Msg.Plain, out.Plain)
			assert.Equal(t, tc.in.Msg.Urgent, out.Urgent)
		})
	}
}

func TestOffloadForDelivery_UrgentPreservedWhenOffloaded(t *testing.T) {
	pol := OffloadPolicy{ThresholdRunes: 10}
	long := strings.Repeat("a", 1000)
	in := OffloadInput{
		Msg:                  &messages.StructuredMessage{Msg: long, Urgent: true},
		PersistedBody:        long,
		MessageID:            "msg-1",
		ConversationID:       "conv-1",
		RecipientCanReadConv: true,
	}
	out, result := OffloadForDelivery(in, pol)
	require.True(t, result.Offloaded)
	assert.True(t, out.Urgent)
}

func TestOffloadForDelivery_FetchByIDMakesEmptyConversationUsable(t *testing.T) {
	pol := OffloadPolicy{ThresholdRunes: 10}
	long := strings.Repeat("a", 1000)
	in := OffloadInput{
		Msg:           &messages.StructuredMessage{Msg: long},
		PersistedBody: long,
		MessageID:     "msg-1",
		FetchByID:     true,
	}
	out, result := OffloadForDelivery(in, pol)
	require.True(t, result.Offloaded)
	assert.Contains(t, out.Msg, "scion messages get msg-1")
}

// ---------------------------------------------------------------------------
// U3: stub content, bounds, and the escaped-width function.
// ---------------------------------------------------------------------------

func TestOffloadForDelivery_StubContentAndBounds(t *testing.T) {
	pol := OffloadPolicy{ThresholdRunes: 10}
	body := strings.Repeat("hello world ", 1000)
	in := OffloadInput{
		Msg:                  &messages.StructuredMessage{Msg: body},
		PersistedBody:        body,
		MessageID:            "0f9c2d7e-5a41-4c7e-9a0b-2b1f6f0e1c11",
		ConversationID:       "3f0ee221-167d-462a-9eed-ca4a688a039a",
		RecipientCanReadConv: true,
	}
	out, result := OffloadForDelivery(in, pol)
	require.True(t, result.Offloaded)

	wantChars := utf8.RuneCountInString(body)
	assert.Equal(t, wantChars, result.Chars)
	sum := sha256.Sum256([]byte(body))
	assert.Equal(t, hex.EncodeToString(sum[:]), result.SHA256)

	assert.Contains(t, out.Msg, in.MessageID)
	assert.Contains(t, out.Msg, "conv:"+in.ConversationID)
	assert.Contains(t, out.Msg, "scion conversation get-message")
	assert.Contains(t, out.Msg, "--body")

	require.NotNil(t, out.Metadata)
	assert.Equal(t, "true", out.Metadata[MetaBodyOffloaded])
	assert.Equal(t, result.SHA256, out.Metadata[MetaBodySHA256])
}

func TestOffloadForDelivery_DivergentBody(t *testing.T) {
	// Chat v2 persists `content` and dispatches `agentContent` — the sha,
	// chars, and preview must all follow PersistedBody, never Msg.Msg.
	pol := OffloadPolicy{ThresholdRunes: 10}
	dispatched := strings.Repeat("dispatched-only ", 1000)
	persisted := strings.Repeat("persisted-only ", 1000)

	in := OffloadInput{
		Msg:                  &messages.StructuredMessage{Msg: dispatched},
		PersistedBody:        persisted,
		MessageID:            "msg-1",
		ConversationID:       "conv-1",
		RecipientCanReadConv: true,
	}
	out, result := OffloadForDelivery(in, pol)
	require.True(t, result.Offloaded)

	assert.Equal(t, utf8.RuneCountInString(persisted), result.Chars)
	sum := sha256.Sum256([]byte(persisted))
	assert.Equal(t, hex.EncodeToString(sum[:]), result.SHA256)
	assert.NotContains(t, out.Msg, "dispatched-only")
	assert.Contains(t, out.Msg, "persisted-only")
}

func TestRuneEscapedWidth_MatchesJSONMarshal(t *testing.T) {
	measure := func(s string) int {
		b, err := json.Marshal(s)
		require.NoError(t, err)
		// Strip the surrounding quotes json.Marshal adds.
		return len(b) - 2
	}

	// All 128 ASCII code points, individually.
	for i := 0; i < 128; i++ {
		r := rune(i)
		s := string(r)
		want := measure(s)
		got := runeEscapedWidth(r, utf8.RuneLen(r))
		assert.Equal(t, want, got, "ASCII code point %d (%q)", i, s)
	}

	// U+2028, U+2029 — HTML-escaping-sensitive line/paragraph separators.
	for _, r := range []rune{' ', ' '} {
		want := measure(string(r))
		got := runeEscapedWidth(r, utf8.RuneLen(r))
		assert.Equal(t, want, got, "rune %U", r)
	}

	// 2-, 3-, 4-byte runes.
	for _, r := range []rune{'é', '€', '中', '🚀'} {
		want := measure(string(r))
		got := runeEscapedWidth(r, utf8.RuneLen(r))
		assert.Equal(t, want, got, "rune %U", r)
	}

	// Invalid UTF-8 byte: json.Marshal replaces each invalid byte with the
	// 6-byte � escape.
	invalid := "\xff"
	want := measure(invalid)
	r, size := utf8.DecodeRuneInString(invalid)
	got := runeEscapedWidth(r, size)
	assert.Equal(t, want, got)
}

func TestOffloadForDelivery_WorstCaseBounds(t *testing.T) {
	pol := OffloadPolicy{ThresholdRunes: 10}

	bodies := map[string]string{
		"all_lt":         strings.Repeat("<", 16000),
		"all_amp":        strings.Repeat("&", 16000),
		"all_emoji":      strings.Repeat("🚀", 16000),
		"all_newline":    strings.Repeat("\n", 16000),
		"crlf":           strings.Repeat("\r\n", 8000),
		"cjk":            strings.Repeat("中", 16000),
		"no_newline_16k": strings.Repeat("x", 16000),
	}

	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			in := OffloadInput{
				Msg:                  &messages.StructuredMessage{Msg: body},
				PersistedBody:        body,
				MessageID:            "0f9c2d7e-5a41-4c7e-9a0b-2b1f6f0e1c11",
				ConversationID:       "3f0ee221-167d-462a-9eed-ca4a688a039a",
				RecipientCanReadConv: true,
			}
			out, result := OffloadForDelivery(in, pol)
			require.True(t, result.Offloaded)

			msgJSON, err := json.Marshal(out.Msg)
			require.NoError(t, err)
			msgBytes := len(msgJSON) - 2 // strip surrounding quotes
			assert.LessOrEqual(t, msgBytes, 1200, "msg field must be <= 1200 bytes escaped")

			slug := strings.Repeat("s", 64)
			senderName := strings.Repeat("d", 64)
			out.Sender = "agent:" + slug
			out.SenderID = "11111111-1111-1111-1111-111111111111"
			out.Recipient = "agent:recipient"
			out.RecipientID = "22222222-2222-2222-2222-222222222222"
			out.Type = messages.TypeInstruction
			out.Timestamp = time.Now().UTC().Format(time.RFC3339)

			convResult := &ConversationResult{
				ConversationID: in.ConversationID,
				Kind:           "direct",
				Surface:        "native",
				DisplayName:    senderName,
			}

			// Direct envelope, both renderers.
			directText := RenderDeliveryText(RenderDeliveryInput{
				MessageID:  in.MessageID,
				ConvResult: convResult,
				Msg:        out,
				CreatedAt:  time.Now(),
			})
			assert.LessOrEqual(t, len(directText), 2048, "direct envelope (new renderer) must be <= 2048 bytes")

			legacyText := messages.FormatForDelivery(out)
			assert.LessOrEqual(t, len(legacyText), 2048, "direct envelope (legacy renderer) must be <= 2048 bytes")

			// nit 8 (impl review r1): a real mention envelope carries
			// mention_source/mention_position metadata (messages.NewMention),
			// which adds bytes beyond the offloaded body_* keys alone — the
			// bound must hold with that extra metadata present, not just in
			// the direct-DM shape.
			mentionOut := *out
			mentionMD := make(map[string]string, len(out.Metadata)+2)
			for k, v := range out.Metadata {
				mentionMD[k] = v
			}
			mentionMD["mention_source"] = "agent:" + slug
			mentionMD["mention_position"] = "body"
			mentionOut.Metadata = mentionMD
			mentionOut.Type = messages.TypeMention

			mentionText := RenderDeliveryText(RenderDeliveryInput{
				MessageID:  in.MessageID,
				ConvResult: convResult,
				Msg:        &mentionOut,
				CreatedAt:  time.Now(),
				IsMention:  true,
			})
			assert.LessOrEqual(t, len(mentionText), 2048, "mention envelope (new renderer) must be <= 2048 bytes")

			// nit 8: the legacy-renderer mention case — FormatForDelivery has
			// no IsMention parameter; a mention is distinguished purely by
			// Type, and deliveryMetadataAllowlist forwards mention_source/
			// mention_position too, so the legacy envelope is checked with
			// the same mention metadata present.
			legacyMentionText := messages.FormatForDelivery(&mentionOut)
			assert.LessOrEqual(t, len(legacyMentionText), 2048, "mention envelope (legacy renderer) must be <= 2048 bytes")
			assert.Contains(t, legacyMentionText, "mention_source")
		})
	}
}

func TestBuildPreview_NUL(t *testing.T) {
	body := "hello\x00world" + strings.Repeat("x", 1000)
	assert.Equal(t, "", buildPreview(body, DefaultPreviewBudgetBytes))
}

// ---------------------------------------------------------------------------
// U4: no mutation.
// ---------------------------------------------------------------------------

func TestOffloadForDelivery_NoMutation(t *testing.T) {
	pol := OffloadPolicy{ThresholdRunes: 10}
	long := strings.Repeat("a", 1000)
	inputMD := map[string]string{"foo": "bar"}
	msg := &messages.StructuredMessage{Msg: long, Metadata: inputMD}
	in := OffloadInput{
		Msg:                  msg,
		PersistedBody:        long,
		MessageID:            "msg-1",
		ConversationID:       "conv-1",
		RecipientCanReadConv: true,
	}

	out, result := OffloadForDelivery(in, pol)
	require.True(t, result.Offloaded)

	assert.Equal(t, long, msg.Msg, "input Msg field must be unchanged")
	assert.Equal(t, map[string]string{"foo": "bar"}, inputMD, "input metadata map must be unchanged")
	assert.False(t, sameMap(inputMD, out.Metadata), "output metadata must be a distinct allocation")
}

// ---------------------------------------------------------------------------
// U9: preview cut rules.
// ---------------------------------------------------------------------------

func TestBuildPreview_HardCutWhenNoNewline(t *testing.T) {
	body := strings.Repeat("x", 10000)
	preview := buildPreview(body, 100)
	assert.NotEmpty(t, preview)
	assert.NotContains(t, preview, "\n")
	assert.Less(t, len(preview), len(body))
}

func TestBuildPreview_NewlineAtPosition1NeverProducesOneCharPreview(t *testing.T) {
	body := "\n" + strings.Repeat("x", 10000)
	preview := buildPreview(body, 100)
	assert.Greater(t, len(preview), 1, "a lone leading newline must not force a 1-character preview")
}

func TestBuildPreview_CutsBackToNewlineAfter50Percent(t *testing.T) {
	// Construct a body where a newline sits well past the 50% mark of the
	// runes that fit in budget, so the cut must land on it.
	budget := 200
	// Roughly 190 plain-ASCII runes fit in a 200-byte budget (1 byte each).
	line1 := strings.Repeat("a", 150)
	body := line1 + "\n" + strings.Repeat("b", 10000)
	preview := buildPreview(body, budget)
	assert.True(t, strings.HasSuffix(preview, "\n") || preview == line1,
		"expected cut back to the newline, got %q", preview)
	assert.NotContains(t, strings.TrimSuffix(preview, "\n"), "b")
}

func TestBuildPreview_WholeBodyFitsUnderBudget(t *testing.T) {
	body := "short body"
	preview := buildPreview(body, DefaultPreviewBudgetBytes)
	assert.Equal(t, body, preview)
}
