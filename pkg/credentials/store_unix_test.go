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

//go:build unix

package credentials

import (
	"os"
	"testing"
)

// assertPrivateFile checks the credentials file mode is exactly FileMode.
func assertPrivateFile(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("credentials file not found: %v", err)
	}
	if info.Mode().Perm() != FileMode {
		t.Errorf("wrong file permissions: got %o, want %o", info.Mode().Perm(), FileMode)
	}
}

// Unix load makes no privacy check: a
// group-readable credentials file still loads.
func TestLoad_UnixKeepsNoPrivacyCheck(t *testing.T) {
	path := useTempCredentials(t)
	writeRawCredentials(t, path, map[string]string{"https://hub.example.com": "tok"})
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if c, err := Load("https://hub.example.com"); err != nil || c.AccessToken != "tok" {
		t.Fatalf("Load of a 0644 credentials file = %v, %v; want the stored token", c, err)
	}
}
