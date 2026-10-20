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

package hub

import (
	"encoding/json"
	"strings"
	"testing"

	gcplog "cloud.google.com/go/logging"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// TestConvertLogEntry_SanitizedErrorAttrPassesThrough is the read-side
// guard for error attributes: the cloud-logs endpoint returns a stored
// error attribute (already sanitized and truncated when it was written)
// as that same string, adding nothing, for both payload shapes the
// Cloud Logging client can return.
func TestConvertLogEntry_SanitizedErrorAttrPassesThrough(t *testing.T) {
	const credentialShaped = "sk-live-0123456789abcdefABCDEF"
	stored := logging.SanitizeText("request failed: env FOO_API_KEY=" + credentialShaped + " " +
		strings.Repeat("output line\n", 150))
	if !strings.Contains(stored, "FOO_API_KEY=[REDACTED]") || !strings.Contains(stored, "...[truncated, ") {
		t.Fatalf("stored text is not sanitized and truncated: %q", stored)
	}
	fields := map[string]any{
		"message": "op failed",
		"error":   stored,
		"detail":  map[string]any{"inner": stored},
	}
	st, err := structpb.NewStruct(fields)
	if err != nil {
		t.Fatal(err)
	}
	for name, payload := range map[string]any{"map": fields, "struct": st} {
		t.Run(name, func(t *testing.T) {
			e := ConvertLogEntry(&gcplog.Entry{Payload: payload})
			if got := e.JSONPayload["error"]; got != stored {
				t.Errorf("error = %#v, want the stored sanitized text", got)
			}
			detail, _ := e.JSONPayload["detail"].(map[string]any)
			if got := detail["inner"]; got != stored {
				t.Errorf("detail.inner = %#v, want the stored sanitized text", got)
			}
			out, err := json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(out), credentialShaped) {
				t.Errorf("endpoint output holds the credential-shaped value: %s", out)
			}
		})
	}
}
