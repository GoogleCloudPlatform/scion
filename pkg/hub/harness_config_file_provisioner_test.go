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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
)

// uploadRecordingStorage records every Upload call, so a test can assert
// that a refused request wrote nothing at all.
type uploadRecordingStorage struct {
	*contentMockStorage
	uploads []string
}

func (m *uploadRecordingStorage) Upload(ctx context.Context, objectPath string, reader io.Reader, opts storage.UploadOptions) (*storage.Object, error) {
	m.uploads = append(m.uploads, objectPath)
	return m.contentMockStorage.Upload(ctx, objectPath, reader, opts)
}

// provisionerUploadCases are the config.yaml bodies the harness-config
// file-write and multipart upload paths must refuse or accept, matching
// finalize (ptone/scion#3133).
var provisionerUploadCases = []struct {
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

func assertUnusableProvisionerResponse(t *testing.T, rec *httptest.ResponseRecorder, wantReason string) {
	t.Helper()
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
	if !strings.Contains(resp.Error.Message, wantReason) {
		t.Errorf("error message %q does not mention %q", resp.Error.Message, wantReason)
	}
}

// The per-file PUT refuses an unusable provisioner block in config.yaml
// before writing it, leaving storage and the record unchanged.
func TestHandleHarnessConfigFileWrite_RejectsUnusableProvisioner(t *testing.T) {
	for _, tc := range provisionerUploadCases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, _ := testHarnessConfigFileServer(t)
			hc := createTestHarnessConfigWithFiles(t, s, nil, nil)
			stor := srv.GetStorage().(*contentMockStorage)

			req := httptest.NewRequest(http.MethodPut,
				"/api/v1/harness-configs/"+hc.ID+"/files/config.yaml",
				strings.NewReader(tc.configYAML))
			req.Header.Set("Content-Type", "text/plain")
			req.Header.Set("Authorization", "Bearer "+testDevToken)
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.wantStatus, rec.Body.String())
			}

			stored, err := s.GetHarnessConfig(context.Background(), hc.ID)
			if err != nil {
				t.Fatalf("get harness config: %v", err)
			}
			_, written := stor.content[hc.StoragePath+"/config.yaml"]
			if tc.wantStatus == http.StatusOK {
				if !written || len(stored.Files) != 1 {
					t.Errorf("expected config.yaml written and recorded: written=%v files=%+v", written, stored.Files)
				}
				return
			}
			assertUnusableProvisionerResponse(t, rec, tc.wantReason)
			if written {
				t.Error("refused config.yaml was written to storage")
			}
			if len(stored.Files) != 0 || stored.ContentHash != hc.ContentHash {
				t.Errorf("record changed by a refused write: files=%+v hash=%q", stored.Files, stored.ContentHash)
			}
		})
	}
}

// A file path that cleans to config.yaml gets the same check as the exact
// name on the per-file PUT.
func TestHandleHarnessConfigFileWrite_RejectsUnusableProvisionerUncleanPath(t *testing.T) {
	srv, s, stor := testHarnessConfigFileServer(t)
	rs := &uploadRecordingStorage{contentMockStorage: stor}
	srv.SetStorage(rs)
	hc := createTestHarnessConfigWithFiles(t, s, nil, nil)

	refused := provisionerUploadCases[0]
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(refused.configYAML))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	srv.handleHarnessConfigFileWrite(rec, req, hc, "./config.yaml")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusUnprocessableEntity, rec.Body.String())
	}
	assertUnusableProvisionerResponse(t, rec, refused.wantReason)
	if len(rs.uploads) != 0 {
		t.Errorf("refused write uploaded %v", rs.uploads)
	}
}

// The multipart upload refuses an unusable provisioner block in a
// config.yaml part before writing any part. Parts are written in sorted
// order and "aaa-first.txt" sorts before "config.yaml", so a check made
// inside the write loop instead of before it always records an upload.
func TestHandleHarnessConfigFileUpload_RejectsUnusableProvisioner(t *testing.T) {
	for _, configPart := range []string{"config.yaml", "./config.yaml", "sub/../config.yaml"} {
		for _, tc := range provisionerUploadCases {
			t.Run(configPart+"/"+tc.name, func(t *testing.T) {
				testMultipartProvisionerCase(t, configPart, tc.configYAML, tc.wantStatus, tc.wantReason)
			})
		}
	}
}

func testMultipartProvisionerCase(t *testing.T, configPart, configYAML string, wantStatus int, wantReason string) {
	t.Helper()
	srv, s, contentStor := testHarnessConfigFileServer(t)
	stor := &uploadRecordingStorage{contentMockStorage: contentStor}
	srv.SetStorage(stor)
	hc := createTestHarnessConfigWithFiles(t, s, nil, nil)

	req := harnessConfigMultipartRequest(t, hc.ID, map[string][]byte{
		"aaa-first.txt": []byte("first\n"),
		configPart:      []byte(configYAML),
		"provision.py":  []byte("print('ok')\n"),
	})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d: %s", rec.Code, wantStatus, rec.Body.String())
	}

	stored, err := s.GetHarnessConfig(context.Background(), hc.ID)
	if err != nil {
		t.Fatalf("get harness config: %v", err)
	}
	if wantStatus == http.StatusOK {
		if len(stor.uploads) != 3 || len(stored.Files) != 3 {
			t.Errorf("expected all three parts written and recorded: uploads=%v files=%+v", stor.uploads, stored.Files)
		}
		return
	}
	assertUnusableProvisionerResponse(t, rec, wantReason)
	if len(stor.uploads) != 0 {
		t.Errorf("refused upload wrote parts before the check: %v", stor.uploads)
	}
	if len(stored.Files) != 0 || stored.ContentHash != hc.ContentHash {
		t.Errorf("record changed by a refused upload: files=%+v hash=%q", stored.Files, stored.ContentHash)
	}
}
