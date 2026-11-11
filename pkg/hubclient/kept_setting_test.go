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
	"bytes"
	"strings"
	"testing"
)

func TestKeptSetting_Message(t *testing.T) {
	kept := KeptSetting{Field: "gcpHostServiceAccountEmail", StoredValue: "a@p.iam.gserviceaccount.com", RequestedValue: "b@p.iam.gserviceaccount.com", Reason: "set"}
	msg := kept.Message()
	for _, want := range []string{`kept gcpHostServiceAccountEmail="a@p.iam.gserviceaccount.com"`, `requested "b@p.iam.gserviceaccount.com"`, "update API"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}

	projectOnly := KeptSetting{Field: "gcpHostProjectId", RequestedValue: "p", Reason: "email_required"}
	msg = projectOnly.Message()
	if !strings.Contains(msg, `not applied gcpHostProjectId="p"`) || !strings.Contains(msg, "gcpHostServiceAccountEmail") {
		t.Errorf("email_required message %q", msg)
	}

	var buf bytes.Buffer
	PrintKeptSettings(&buf, []KeptSetting{kept, projectOnly})
	if got := strings.Count(buf.String(), "\n"); got != 2 {
		t.Errorf("PrintKeptSettings wrote %d lines, want 2", got)
	}
}
