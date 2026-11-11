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

//go:build !no_sqlite

package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Finalize refuses an uploaded config.yaml whose provisioner block cannot
// provision an agent, with 422 harness_config_unusable, and leaves the record
// as it was (ptone/scion#3133).
func TestHandleHarnessConfigFinalize_RejectsUnusableProvisioner(t *testing.T) {
	cases := []struct {
		name       string
		configYAML string
		wantStatus int
		wantReason string
	}{
		{
			name:       "builtin type",
			configYAML: "harness: claude\nprovisioner:\n  type: builtin\n",
			wantStatus: http.StatusUnprocessableEntity,
			wantReason: `provisioner.type "builtin"`,
		},
		{
			name:       "empty command",
			configYAML: "harness: claude\nprovisioner:\n  type: container-script\n  interface_version: 1\n",
			wantStatus: http.StatusUnprocessableEntity,
			wantReason: "provisioner.command is empty",
		},
		{
			name:       "container-script with command",
			configYAML: "harness: claude\nprovisioner:\n  type: container-script\n  interface_version: 1\n  command: [python3, provision.py]\n",
			wantStatus: http.StatusOK,
		},
		{
			name:       "no provisioner block",
			configYAML: "harness: claude\n",
			wantStatus: http.StatusOK,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, _ := testHarnessConfigFileServer(t)
			ctx := context.Background()
			hc := createTestHarnessConfigWithFiles(t, s, nil, nil)
			stor := srv.GetStorage().(*contentMockStorage)

			objectPath := hc.StoragePath + "/config.yaml"
			stor.content[objectPath] = []byte(tc.configYAML)
			stor.objects[objectPath] = &storage.Object{Name: objectPath, Size: int64(len(tc.configYAML))}

			body := map[string]interface{}{
				"manifest": map[string]interface{}{
					"files": []map[string]interface{}{
						{"path": "config.yaml", "size": len(tc.configYAML), "hash": "sha256:placeholder"},
					},
				},
			}
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+hc.ID+"/finalize", body)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.wantStatus, rec.Body.String())
			}

			stored, err := s.GetHarnessConfig(ctx, hc.ID)
			if err != nil {
				t.Fatalf("get harness config: %v", err)
			}
			if tc.wantStatus == http.StatusOK {
				if len(stored.Files) != 1 {
					t.Errorf("expected the manifest to be recorded, got %+v", stored.Files)
				}
				return
			}

			var resp struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode error body: %v (%s)", err, rec.Body.String())
			}
			if resp.Error.Code != harnessConfigUnusableErrorCode {
				t.Errorf("error code = %q, want %q", resp.Error.Code, harnessConfigUnusableErrorCode)
			}
			if !strings.Contains(resp.Error.Message, tc.wantReason) {
				t.Errorf("error message %q does not mention %q", resp.Error.Message, tc.wantReason)
			}
			if len(stored.Files) != 0 || stored.Status != store.HarnessConfigStatusActive {
				t.Errorf("record changed by a refused finalize: files=%+v status=%q", stored.Files, stored.Status)
			}
		})
	}
}
