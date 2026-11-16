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

package api

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateHomeFiles(t *testing.T) {
	const secretCanary = "SUPER_SECRET_EVENT_PAYLOAD_CANARY_987654321"
	canaryBytes := []byte(secretCanary)

	exactMax := bytes.Repeat([]byte("a"), MaxHomeFilesTotalBytes)
	halfMax := bytes.Repeat([]byte("b"), MaxHomeFilesTotalBytes/2)

	tests := []struct {
		name    string
		files   []HomeFile
		wantErr string
	}{
		{
			name:  "nil slice ok",
			files: nil,
		},
		{
			name:  "empty slice ok",
			files: []HomeFile{},
		},
		{
			name: "single valid 0600 file ok",
			files: []HomeFile{
				{Path: ".scion/trigger-event.json", Mode: 0o600, Content: []byte(`{"event_id":"e1"}`)},
			},
		},
		{
			name: "single valid 0644 nested file and empty content ok",
			files: []HomeFile{
				{Path: ".scion/triggers/context.json", Mode: 0o644, Content: nil},
			},
		},
		{
			name: "max 4 files ok",
			files: []HomeFile{
				{Path: ".scion/f1.json", Mode: 0o600, Content: []byte("1")},
				{Path: ".scion/f2.json", Mode: 0o644, Content: []byte("2")},
				{Path: ".scion/sub/f3.json", Mode: 0o600, Content: []byte("3")},
				{Path: ".scion/sub/deep/f4.json", Mode: 0o644, Content: []byte("4")},
			},
		},
		{
			name: "5 files refused",
			files: []HomeFile{
				{Path: ".scion/f1.json", Mode: 0o600, Content: canaryBytes},
				{Path: ".scion/f2.json", Mode: 0o600, Content: canaryBytes},
				{Path: ".scion/f3.json", Mode: 0o600, Content: canaryBytes},
				{Path: ".scion/f4.json", Mode: 0o600, Content: canaryBytes},
				{Path: ".scion/f5.json", Mode: 0o600, Content: canaryBytes},
			},
			wantErr: "too many home files",
		},
		{
			name: "total size exact 64 KiB single file ok",
			files: []HomeFile{
				{Path: ".scion/max.bin", Mode: 0o600, Content: exactMax},
			},
		},
		{
			name: "total size exact 64 KiB across two files ok",
			files: []HomeFile{
				{Path: ".scion/half1.bin", Mode: 0o600, Content: halfMax},
				{Path: ".scion/half2.bin", Mode: 0o644, Content: halfMax},
			},
		},
		{
			name: "total size 64 KiB + 1 single file refused",
			files: []HomeFile{
				{Path: ".scion/over.bin", Mode: 0o600, Content: append(append([]byte{}, exactMax...), 'x')},
			},
			wantErr: "exceeds",
		},
		{
			name: "total size 64 KiB + 1 across multiple files refused",
			files: []HomeFile{
				{Path: ".scion/half1.bin", Mode: 0o600, Content: halfMax},
				{Path: ".scion/half2.bin", Mode: 0o600, Content: halfMax},
				{Path: ".scion/extra.bin", Mode: 0o600, Content: []byte(secretCanary)[:1]},
			},
			wantErr: "exceeds",
		},
		{
			name: "empty path refused",
			files: []HomeFile{
				{Path: "", Mode: 0o600, Content: canaryBytes},
			},
			wantErr: "empty",
		},
		{
			name: "missing .scion/ prefix refused",
			files: []HomeFile{
				{Path: "trigger-event.json", Mode: 0o600, Content: canaryBytes},
			},
			wantErr: ".scion/",
		},
		{
			name: "other dotdir prefix refused",
			files: []HomeFile{
				{Path: ".ssh/authorized_keys", Mode: 0o600, Content: canaryBytes},
			},
			wantErr: ".scion/",
		},
		{
			name: ".scion-other prefix confusion refused",
			files: []HomeFile{
				{Path: ".scion-evil/file.json", Mode: 0o600, Content: canaryBytes},
			},
			wantErr: ".scion/",
		},
		{
			name: "bare .scion refused",
			files: []HomeFile{
				{Path: ".scion", Mode: 0o600, Content: canaryBytes},
			},
			wantErr: ".scion/",
		},
		{
			name: "bare .scion/ directory refused",
			files: []HomeFile{
				{Path: ".scion/", Mode: 0o600, Content: canaryBytes},
			},
			wantErr: "clean",
		},
		{
			name: "dot-dot escape from .scion refused",
			files: []HomeFile{
				{Path: ".scion/../.bashrc", Mode: 0o600, Content: canaryBytes},
			},
			wantErr: "..",
		},
		{
			name: "leading dot-dot refused",
			files: []HomeFile{
				{Path: "../.scion/trigger-event.json", Mode: 0o600, Content: canaryBytes},
			},
			wantErr: "..",
		},
		{
			name: "nested dot-dot within .scion refused",
			files: []HomeFile{
				{Path: ".scion/a/../b.json", Mode: 0o600, Content: canaryBytes},
			},
			wantErr: "..",
		},
		{
			name: "absolute unix path refused",
			files: []HomeFile{
				{Path: "/.scion/trigger-event.json", Mode: 0o600, Content: canaryBytes},
			},
			wantErr: "relative",
		},
		{
			name: "absolute home path refused",
			files: []HomeFile{
				{Path: "/home/scion/.scion/trigger-event.json", Mode: 0o600, Content: canaryBytes},
			},
			wantErr: "relative",
		},
		{
			name: "windows drive prefix refused",
			files: []HomeFile{
				{Path: "C:/.scion/trigger-event.json", Mode: 0o600, Content: canaryBytes},
			},
			wantErr: "relative",
		},
		{
			name: "leading ./ unclean path refused",
			files: []HomeFile{
				{Path: "./.scion/trigger-event.json", Mode: 0o600, Content: canaryBytes},
			},
			wantErr: "clean",
		},
		{
			name: "inner /./ unclean path refused",
			files: []HomeFile{
				{Path: ".scion/./trigger-event.json", Mode: 0o600, Content: canaryBytes},
			},
			wantErr: "clean",
		},
		{
			name: "trailing /. unclean path refused",
			files: []HomeFile{
				{Path: ".scion/.", Mode: 0o600, Content: canaryBytes},
			},
			wantErr: "clean",
		},
		{
			name: "double slash unclean path refused",
			files: []HomeFile{
				{Path: ".scion//trigger-event.json", Mode: 0o600, Content: canaryBytes},
			},
			wantErr: "clean",
		},
		{
			name: "trailing slash directory path refused",
			files: []HomeFile{
				{Path: ".scion/sub/", Mode: 0o600, Content: canaryBytes},
			},
			wantErr: "clean",
		},
		{
			name: "backslash in path refused",
			files: []HomeFile{
				{Path: `.scion\trigger-event.json`, Mode: 0o600, Content: canaryBytes},
			},
			wantErr: "backslash",
		},
		{
			name: "nested backslash in path refused",
			files: []HomeFile{
				{Path: `.scion/sub\trigger-event.json`, Mode: 0o600, Content: canaryBytes},
			},
			wantErr: "backslash",
		},
		{
			name: "NUL byte in path refused",
			files: []HomeFile{
				{Path: ".scion/trigger\x00event.json", Mode: 0o600, Content: canaryBytes},
			},
			wantErr: "NUL",
		},
		{
			name: "mode 0 refused",
			files: []HomeFile{
				{Path: ".scion/trigger-event.json", Mode: 0, Content: canaryBytes},
			},
			wantErr: "mode",
		},
		{
			name: "mode 0700 refused",
			files: []HomeFile{
				{Path: ".scion/trigger-event.json", Mode: 0o700, Content: canaryBytes},
			},
			wantErr: "mode",
		},
		{
			name: "mode 0666 refused",
			files: []HomeFile{
				{Path: ".scion/trigger-event.json", Mode: 0o666, Content: canaryBytes},
			},
			wantErr: "mode",
		},
		{
			name: "mode 04600 (setuid) refused",
			files: []HomeFile{
				{Path: ".scion/trigger-event.json", Mode: 0o4600, Content: canaryBytes},
			},
			wantErr: "mode",
		},
		{
			name: "duplicate paths refused",
			files: []HomeFile{
				{Path: ".scion/trigger-event.json", Mode: 0o600, Content: canaryBytes},
				{Path: ".scion/trigger-event.json", Mode: 0o644, Content: canaryBytes},
			},
			wantErr: "duplicate",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateHomeFiles(tc.files)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateHomeFiles() unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateHomeFiles() = nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("ValidateHomeFiles() error = %q, want substring %q", err.Error(), tc.wantErr)
			}
			if strings.Contains(err.Error(), secretCanary) {
				t.Errorf("ValidateHomeFiles() error leaked file content: %q", err.Error())
			}
		})
	}
}

func TestHomeFileJSONWireEncoding(t *testing.T) {
	orig := HomeFile{
		Path:    ".scion/trigger-event.json",
		Mode:    0o600,
		Content: []byte("{\"type\":\"trigger.event\"}\n\x00\xff"),
	}
	raw, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	// []byte must base64-encode on the wire rather than emit a JSON number array.
	if strings.Contains(string(raw), "[123") {
		t.Fatalf("expected base64 string on wire, got %s", raw)
	}
	var got HomeFile
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if got.Path != orig.Path || got.Mode != orig.Mode || !bytes.Equal(got.Content, orig.Content) {
		t.Fatalf("round-trip mismatch: got %+v, want %+v", got, orig)
	}
}
