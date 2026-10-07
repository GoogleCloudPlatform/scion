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

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

const (
	testArtifactA = "5f1c2d3e-0000-4000-8000-00000000000a"
	testArtifactB = "5f1c2d3e-0000-4000-8000-00000000000b"
)

func artifactRefsMD() map[string]string {
	return map[string]string{
		"foo": "bar",
		artifacts.MessageMetadataKey: artifacts.EncodeMessageRefs([]artifacts.MessageRef{
			{ArtifactID: testArtifactA, Seq: 2}, {ArtifactID: testArtifactB},
		}),
	}
}

// The artifact reference key is hub-reserved: StripReservedMetadata removes
// it like the offload keys, so only the hub's admission step can set it.
func TestStripReservedMetadata_StripsArtifactRefs(t *testing.T) {
	md := artifactRefsMD()
	out := StripReservedMetadata(md)
	if _, ok := out[artifacts.MessageMetadataKey]; ok {
		t.Fatalf("artifact refs survived the strip: %v", out)
	}
	if out["foo"] != "bar" {
		t.Errorf("unrelated key lost: %v", out)
	}
	if _, ok := md[artifacts.MessageMetadataKey]; !ok {
		t.Error("input map mutated")
	}
}

// OffloadForDelivery strips only its own keys: admitted artifact refs stay
// on the dispatched copy, offloaded or not.
func TestOffloadForDelivery_KeepsArtifactRefs(t *testing.T) {
	md := artifactRefsMD()
	md[MetaBodyOffloaded] = "spoofed"
	body := strings.Repeat("x", 200)
	out, res := OffloadForDelivery(OffloadInput{
		Msg:                  &messages.StructuredMessage{Msg: body, Metadata: md},
		PersistedBody:        body,
		MessageID:            "m1",
		ConversationID:       "c1",
		RecipientCanReadConv: true,
	}, OffloadPolicy{ThresholdRunes: 100})
	if !res.Offloaded {
		t.Fatal("expected offload")
	}
	if out.Metadata[artifacts.MessageMetadataKey] != md[artifacts.MessageMetadataKey] {
		t.Errorf("artifact refs lost on offloaded copy: %v", out.Metadata)
	}
	if out.Metadata[MetaBodyOffloaded] != "true" {
		t.Errorf("offload key not hub-set: %v", out.Metadata)
	}
}

func TestAppendArtifactFetchHints(t *testing.T) {
	got := AppendArtifactFetchHints("see attached", artifactRefsMD())
	want := "see attached\n" +
		"\nArtifact: v2 - scion artifact get scion://artifact/" + testArtifactA + "@2" +
		"\nArtifact: current - scion artifact get scion://artifact/" + testArtifactB
	if got != want {
		t.Errorf("hints:\n got %q\nwant %q", got, want)
	}
	for _, md := range []map[string]string{nil, {"foo": "bar"}, {artifacts.MessageMetadataKey: "junk"}} {
		if got := AppendArtifactFetchHints("body", md); got != "body" {
			t.Errorf("AppendArtifactFetchHints(%v) = %q, want body unchanged", md, got)
		}
	}
}

// The rendered envelope carries the refs in metadata and the hint lines in
// the body; the input StructuredMessage is not changed.
func TestRenderDeliveryText_ArtifactHints(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	msg := &messages.StructuredMessage{
		Version: messages.Version, Timestamp: now.Format(time.RFC3339),
		Sender: "agent:a", Recipient: "agent:b", Msg: "design ready",
		Type: messages.TypeInstruction, Metadata: artifactRefsMD(),
	}
	env := extractDeliveryEnvelope(t, RenderDeliveryText(RenderDeliveryInput{MessageID: "m1", Msg: msg, CreatedAt: now}))
	if !strings.HasPrefix(env.Msg, "design ready\n\nArtifact: v2 - scion artifact get ") {
		t.Errorf("envelope body = %q", env.Msg)
	}
	if env.Metadata[artifacts.MessageMetadataKey] == "" {
		t.Errorf("envelope metadata lacks refs: %v", env.Metadata)
	}
	if msg.Msg != "design ready" {
		t.Errorf("input body changed: %q", msg.Msg)
	}

	msg.Plain = true
	plain := RenderDeliveryText(RenderDeliveryInput{MessageID: "m1", Msg: msg, CreatedAt: now})
	if !strings.HasPrefix(plain, "design ready\n\nArtifact: v2") {
		t.Errorf("plain delivery = %q", plain)
	}
}
