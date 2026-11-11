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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

func TestExpectedRuntimeTargetID_JSONKey(t *testing.T) {
	b, err := json.Marshal(CreateAgentRequest{Name: "a", ProjectID: "p", RuntimeBrokerID: "b", ExpectedRuntimeTargetID: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"expectedRuntimeTargetId":"t"`) {
		t.Fatalf("frozen wire key missing: %s", b)
	}
	b, _ = json.Marshal(CreateAgentRequest{Name: "a", ProjectID: "p"})
	if strings.Contains(string(b), "expectedRuntimeTargetId") {
		t.Fatalf("empty field must be omitted: %s", b)
	}
}

func TestRuntimeTargetDescriptor_OnRegistrationTypes(t *testing.T) {
	desc := &api.RuntimeTargetDescriptor{ID: "t", Type: "docker"}
	for name, v := range map[string]interface{}{
		"CreateBrokerRequest":  CreateBrokerRequest{Name: "n", RuntimeTarget: desc},
		"CreateBrokerResponse": CreateBrokerResponse{BrokerID: "b", RuntimeTarget: desc},
		"JoinBrokerRequest":    JoinBrokerRequest{BrokerID: "b", RuntimeTarget: desc},
		"JoinBrokerResponse":   JoinBrokerResponse{BrokerID: "b", RuntimeTarget: desc},
	} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), `"runtimeTarget":{"id":"t","type":"docker"}`) {
			t.Fatalf("%s: runtimeTarget missing: %s", name, b)
		}
	}
	// A response without the acknowledgement (an older Hub) decodes to nil.
	var resp JoinBrokerResponse
	if err := json.Unmarshal([]byte(`{"secretKey":"s","brokerId":"b"}`), &resp); err != nil || resp.RuntimeTarget != nil {
		t.Fatalf("missing acknowledgement must decode as nil: %+v %v", resp, err)
	}
}
