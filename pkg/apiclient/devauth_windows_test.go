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

package apiclient

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/internal/testutil/wintest"
)

func assertPrivateTokenFile(t *testing.T, path string) {
	t.Helper()
	wintest.AssertPrivateSD(t, path)
}

// A new dev token file gets the owner-only ACL even in a directory
// that hands an inheritable Users ACE to new files; the directory itself
// keeps its default (inherited) protection. Control: a file created there
// without privatefs carries the Users ACE.
func TestInitDevAuth_WindowsTokenFilePrivate(t *testing.T) {
	dir := t.TempDir()
	wintest.Icacls(t, dir, "/grant", "*"+wintest.SIDUsers+":(OI)(CI)R")
	control := filepath.Join(dir, "control")
	if err := os.WriteFile(control, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !wintest.ReadSD(t, control).HasAllowACE(wintest.SIDUsers) {
		t.Fatal("control: a plain new file does not carry the inherited Users ACE")
	}
	sub := filepath.Join(dir, "scion")
	token, err := InitDevAuth(DevAuthConfig{Enabled: true}, sub)
	if err != nil {
		t.Fatalf("InitDevAuth: %v", err)
	}
	tokenFile := filepath.Join(sub, "dev-token")
	wintest.AssertPrivateSD(t, tokenFile)
	data, err := os.ReadFile(tokenFile)
	if err != nil || strings.TrimSpace(string(data)) != token {
		t.Fatalf("token file = %q, %v; want %q", data, err, token)
	}
	if !wintest.ReadSD(t, sub).HasAllowACE(wintest.SIDUsers) {
		t.Error("the token directory lost its inherited protection: only the token file is private, the directory keeps what it inherits")
	}
}
