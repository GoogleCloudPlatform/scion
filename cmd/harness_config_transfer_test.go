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

package cmd

import (
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

func TestVerifyHarnessConfigArtifactHash(t *testing.T) {
	content := []byte("hello harness")
	correctHash := transfer.HashBytes(content)

	cases := []struct {
		name     string
		announce string
		body     []byte
		wantErr  string
	}{
		{
			name:     "matching hash passes",
			announce: correctHash,
			body:     content,
		},
		{
			name:     "empty announced hash skipped (legacy artifacts)",
			announce: "",
			body:     content,
		},
		{
			name:     "mismatch fails with diagnostic",
			announce: "sha256:0000000000000000000000000000000000000000000000000000000000000000",
			body:     content,
			wantErr:  "hash mismatch",
		},
		{
			name:     "tampered body fails",
			announce: correctHash,
			body:     []byte("tampered"),
			wantErr:  "hash mismatch",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyHarnessConfigArtifactHash(hubclient.DownloadURLInfo{
				Path: "config.yaml",
				Hash: tc.announce,
			}, tc.body)
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("expected nil error, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
			if !strings.Contains(err.Error(), "config.yaml") {
				t.Errorf("error %q should mention the file path", err.Error())
			}
		})
	}
}
