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

package runtimebroker

import (
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

func TestParseExportOwner(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		for _, tc := range []struct {
			in      string
			wantUID int
			wantGID int
		}{
			{"1000:1000\n", 1000, 1000},
			{"0:0", 0, 0},
			{" 2000:3000 \n", 2000, 3000},
		} {
			uid, gid, err := parseExportOwner([]byte(tc.in))
			if err != nil {
				t.Fatalf("parseExportOwner(%q) unexpected error: %v", tc.in, err)
			}
			if uid != tc.wantUID || gid != tc.wantGID {
				t.Errorf("parseExportOwner(%q) = %d:%d, want %d:%d", tc.in, uid, gid, tc.wantUID, tc.wantGID)
			}
		}
	})

	t.Run("invalid", func(t *testing.T) {
		for _, in := range []string{
			"",
			"1000",
			"1000:1000:1000",
			"-1:1000",
			"1000:-1",
			"scion:scion",
			"1000:",
			":1000",
		} {
			if _, _, err := parseExportOwner([]byte(in)); err == nil {
				t.Errorf("parseExportOwner(%q) succeeded, want error", in)
			}
		}
	})
}

func TestOwnerMismatchWarning(t *testing.T) {
	if got := ownerMismatchWarning(1000, 1000, 1000, 1000); got != "" {
		t.Fatalf("matching ids returned %q, want empty", got)
	}

	for _, tc := range []struct {
		name                     string
		agentUID, agentGID       int
		recordedUID, recordedGID int
		wantSubstrings           []string
	}{
		{
			name:        "uid mismatch",
			agentUID:    1000,
			agentGID:    1000,
			recordedUID: 2000,
			recordedGID: 1000,
			wantSubstrings: []string{
				shareHealthOwnerMismatch,
				"1000:1000",
				"2000:1000",
				"server.project_storage.nfs.uid/gid",
				api.ExportOwnerMarkerName,
				"correct or remove",
			},
		},
		{
			name:        "gid mismatch",
			agentUID:    1000,
			agentGID:    2000,
			recordedUID: 1000,
			recordedGID: 1000,
			wantSubstrings: []string{
				shareHealthOwnerMismatch,
				"1000:2000",
				"1000:1000",
				"server.project_storage.nfs.uid/gid",
				api.ExportOwnerMarkerName,
				"correct or remove",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ownerMismatchWarning(tc.agentUID, tc.agentGID, tc.recordedUID, tc.recordedGID)
			for _, sub := range tc.wantSubstrings {
				if !strings.Contains(got, sub) {
					t.Errorf("ownerMismatchWarning = %q, want substring %q", got, sub)
				}
			}
			if strings.Contains(got, "broker runtime uid/gid") {
				t.Errorf("ownerMismatchWarning = %q, must not mention non-existent 'broker runtime uid/gid' setting", got)
			}
		})
	}
}
