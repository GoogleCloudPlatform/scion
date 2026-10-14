/*
Copyright 2026 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package hub

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestChatMemberCanAttachFalseIsSerialised pins the JSON encoding of a denied
// agent.
//
// The members sidebar hides the terminal control when canAttach is false. With
// `omitempty` on the field a denied agent encodes to nothing at all, the client
// cannot distinguish "not allowed" from "field absent", and the control stays
// visible for agents the viewer cannot attach to — which is the exact case the
// field exists to cover. The gate then looks correct in review and does nothing
// in practice.
//
// false is the value the client most needs to receive, so it must be on the wire.
func TestChatMemberCanAttachFalseIsSerialised(t *testing.T) {
	denied, err := json.Marshal(chatMemberEntry{ID: "a1", Kind: "agent", CanAttach: false})
	if err != nil {
		t.Fatalf("marshal denied agent: %v", err)
	}
	if !strings.Contains(string(denied), `"canAttach":false`) {
		t.Fatalf("denied agent must serialise canAttach:false, got %s", denied)
	}

	allowed, err := json.Marshal(chatMemberEntry{ID: "a2", Kind: "agent", CanAttach: true})
	if err != nil {
		t.Fatalf("marshal allowed agent: %v", err)
	}
	if !strings.Contains(string(allowed), `"canAttach":true`) {
		t.Fatalf("allowed agent must serialise canAttach:true, got %s", allowed)
	}
}
