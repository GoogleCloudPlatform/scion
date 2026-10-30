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

package apierr

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// A write the database encryption policy refuses maps to 409 with the
// stable encryption_policy_mismatch code (ptone/scion#1530 §6).
func TestWriteErrorFromErr_EncryptionPolicyMismatch(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteErrorFromErr(rec, fmt.Errorf("encrypting secret value: %w", store.ErrEncryptionPolicyMismatch), "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body.Error.Code != ErrCodeEncryptionPolicyMismatch {
		t.Fatalf("code = %q, want %q", body.Error.Code, ErrCodeEncryptionPolicyMismatch)
	}
}
