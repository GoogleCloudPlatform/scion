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

package agentcontrol

import (
	"errors"
	"testing"
	"time"
)

func validReq() DeliverRequest {
	text := "hi"
	return DeliverRequest{V: 1, DeliveryID: "d", BrokerID: "b", MessageIDs: []string{"m"}, Op: OpPaste,
		Text: &text, DeadlineMS: 92000, NotAfterUnixMS: time.Now().UnixMilli(), SubmitKeys: []string{"Enter"}}
}

func TestValidateFromBrokerIgnoresHubFields(t *testing.T) {
	r := validReq()
	r.BrokerID, r.NotAfterUnixMS = "", 0
	if err := r.ValidateFromBroker(); err != nil {
		t.Fatalf("ValidateFromBroker: %v", err)
	}
	var fe *FieldError
	if err := r.Validate(); !errors.As(err, &fe) || fe.Field != FieldBrokerID {
		t.Fatalf("Validate = %v, want broker_id", err)
	}
	r.BrokerID = "b"
	if err := r.Validate(); !errors.As(err, &fe) || fe.Field != FieldNotAfter {
		t.Fatalf("Validate = %v, want not_after_unix_ms", err)
	}
}

func TestDecodeDeliverRequestStrict(t *testing.T) {
	if _, err := DecodeDeliverRequest([]byte(`{"v":1,"route":"exec-start"}`)); err == nil {
		t.Fatal("unknown field accepted")
	}
	if _, err := DecodeDeliverRequest([]byte(`{"v":1} {"v":1}`)); err == nil {
		t.Fatal("second value accepted")
	}
	if _, err := DecodeDeliverRequest([]byte(`{"v":1}`)); err != nil {
		t.Fatalf("plain body refused: %v", err)
	}
}

func TestFieldErrorNamesFieldOnly(t *testing.T) {
	r := validReq()
	r.SubmitKeys = []string{"SECRET-KEY-NAME"}
	err := r.Validate()
	if err == nil || err.Error() != "deliver-message: invalid field submit_keys" {
		t.Fatalf("err = %v", err)
	}
}

func TestDecodeDeliverResponseNeverSuccessByDefault(t *testing.T) {
	for _, b := range []string{``, `{}`, `not json`, `{"replayed":true}`} {
		if _, err := DecodeDeliverResponse([]byte(b)); err == nil {
			t.Errorf("%q decoded without error", b)
		}
	}
	r, err := DecodeDeliverResponse([]byte(`{"outcome":"partial","stage":"submit"}`))
	if err != nil || r.Outcome != OutcomePartial || r.Stage != StageSubmit {
		t.Fatalf("= %+v %v", r, err)
	}
}

func TestTimingBudget(t *testing.T) {
	// design v2.22 §2.2: hard stop 92 s + 8 s paste < hub 110 s < broker 115 s < relay 120 s.
	if MaxDeliverDeadline+8*time.Second >= HubDeliverTimeout || HubDeliverTimeout >= BrokerDeliverTimeout || BrokerDeliverTimeout >= 120*time.Second {
		t.Fatal("timing budget out of order")
	}
	if StaleDelivering != 220*time.Second {
		t.Fatalf("StaleDelivering = %s, want 220s", StaleDelivering)
	}
}
