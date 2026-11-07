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

package rebuildpreflight

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func decodeBody(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return m
}

// An invalid branch parameter on rebuild-server or rebuild-web is a 400
// that states the allowed form.
func TestCheckRunRequest_InvalidBranchRejected(t *testing.T) {
	for _, key := range []string{RebuildServerKey, RebuildWebKey} {
		for _, body := range []string{
			`{"params":{"branch":"-x"}}`,
			`{"params":{"branch":"a..b"}}`,
			`{"params":{"branch":"HEAD"}}`,
			`{"params":{"branch":" --upload-pack=x "}}`,
		} {
			t.Run(key+" "+body, func(t *testing.T) {
				_, rej := CheckRunRequest(key, decodeBody(t, body))
				if rej == nil {
					t.Fatalf("CheckRunRequest(%s, %s) = nil, want a 400", key, body)
				}
				if rej.Status != http.StatusBadRequest {
					t.Errorf("status = %d, want 400", rej.Status)
				}
				if !strings.HasPrefix(rej.Message, "invalid branch name: ") || !strings.HasSuffix(rej.Message, BranchRule) {
					t.Errorf("message = %q, want the reason and the allowed form", rej.Message)
				}
			})
		}
	}
}

// A branch parameter that is not a string is a 400 too (the parameter
// parser would drop it silently).
func TestCheckRunRequest_NonStringBranchRejected(t *testing.T) {
	for _, body := range []string{
		`{"params":{"branch":1}}`,
		`{"params":{"branch":true}}`,
		`{"params":{"branch":null}}`,
		`{"params":{"branch":["main"]}}`,
	} {
		_, rej := CheckRunRequest(RebuildServerKey, decodeBody(t, body))
		if rej == nil || rej.Status != http.StatusBadRequest || !strings.HasSuffix(rej.Message, BranchRule) {
			t.Errorf("CheckRunRequest(%s) = %+v, want a 400 with the allowed form", body, rej)
		}
	}
}

func TestCheckRunRequest_Accepts(t *testing.T) {
	for _, tc := range []struct{ key, body string }{
		{RebuildServerKey, `{"params":{"branch":"main"}}`},
		{RebuildServerKey, `{"params":{"branch":" release/1.2 "}}`},
		{RebuildServerKey, `{"params":{"branch":""}}`},
		{RebuildServerKey, `{"params":{"branch":"   "}}`},
		{RebuildServerKey, `{"params":{}}`},
		{RebuildServerKey, `{}`},
		{RebuildServerKey, `{"params":"x"}`},
		{RebuildWebKey, `{"params":{"branch":"feature_x-y"}}`},
		{RebuildWebKey, `{"params":{"dryRun":true}}`},
		// Other operations do not take a branch to check out.
		{"pull-images", `{"params":{"branch":"-x"}}`},
		{"rebuild-container-binaries", `{"params":{"branch":"-x"}}`},
	} {
		got, rej := CheckRunRequest(tc.key, decodeBody(t, tc.body))
		if rej != nil {
			t.Errorf("CheckRunRequest(%s, %s) = %+v, want nil", tc.key, tc.body, rej)
		}
		if got != (ForceRequest{}) {
			t.Errorf("CheckRunRequest(%s, %s) force request = %+v, want empty", tc.key, tc.body, got)
		}
	}
	if _, rej := CheckRunRequest(RebuildServerKey, nil); rej != nil {
		t.Errorf("CheckRunRequest with no body = %+v, want nil", rej)
	}
}
