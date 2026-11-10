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

//go:build windows

package relay

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// TestLoadPeerPrivateKey_NotSupportedOnWindows: on Windows every relay-peer
// key file is refused (fail closed), directly and through PeerKeyOptions.
func TestLoadPeerPrivateKey_NotSupportedOnWindows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peer.key")
	// The content is never read: the refusal comes before any parse.
	if err := os.WriteFile(path, []byte("placeholder"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPeerPrivateKey(path); err == nil || !strings.Contains(err.Error(), "not supported on Windows") {
		t.Fatalf("LoadPeerPrivateKey on Windows: %v, want the not-supported refusal", err)
	}
	if _, _, err := PeerKeyOptions(config.HubConduitConfig{InstanceID: "relay-a", PeerKeyFile: path}); err == nil || !strings.Contains(err.Error(), "not supported on Windows") {
		t.Fatalf("PeerKeyOptions with a key file on Windows: %v, want the not-supported refusal", err)
	}
}
