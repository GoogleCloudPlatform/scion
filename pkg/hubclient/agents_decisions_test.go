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

package hubclient

import (
	"encoding/json"
	"testing"
)

// The decision fields of an outbound send: decisions and decisions_update go
// out only when set, and the result's decisions decode with the replaced
// version's answer and who gave it.
func TestOutboundMessageChoicesWire(t *testing.T) {
	plain, err := json.Marshal(OutboundMessageRequest{Recipient: "user:a@example.com", Msg: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]interface{}
	if err := json.Unmarshal(plain, &fields); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"decisions", "decisions_update"} {
		if _, ok := fields[k]; ok {
			t.Errorf("an ordinary send carries %q", k)
		}
	}

	withDecisions, err := json.Marshal(OutboundMessageRequest{
		Recipient: "user:a@example.com", Decisions: json.RawMessage(`[{"question":"q","choices":["a","b"]}]`), DecisionsUpdate: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	fields = nil
	if err := json.Unmarshal(withDecisions, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["decisions_update"] != true {
		t.Errorf("decisions_update = %v, want true", fields["decisions_update"])
	}
	if _, ok := fields["decisions"].([]interface{}); !ok {
		t.Errorf("decisions = %v, want the decisions array", fields["decisions"])
	}

	var res OutboundMessageResult
	body := `{"message_id":"m1","status":"sent","decisions":[` +
		`{"key":"D1","version":2,"ref":"agent/D1","row_id":"r2","replaced":{"version":1,"status":"decided","answer":"yes","answered_by":"Pat"}},` +
		`{"key":"7","version":1,"ref":"agent/7","row_id":"r3","replaced":null}]}`
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Decisions) != 2 {
		t.Fatalf("decisions = %d, want 2", len(res.Decisions))
	}
	r := res.Decisions[0].Replaced
	if r == nil || r.Version != 1 || r.Status != "decided" || r.Answer != "yes" || r.AnsweredBy != "Pat" {
		t.Errorf("replaced = %+v", r)
	}
	if res.Decisions[1].Replaced != nil {
		t.Errorf("a new decision has no replaced version: %+v", res.Decisions[1].Replaced)
	}
}
