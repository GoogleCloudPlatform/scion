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

//go:build !windows

package relay

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// TestLoadPeerPrivateKey_RefusesGroupOrOtherReadable: a key file readable
// by group or other is refused; an owner-only one loads.
func TestLoadPeerPrivateKey_RefusesGroupOrOtherReadable(t *testing.T) {
	p := newTestPeer(t, "relay-a", "a1", PeerRoleRelay)
	der, err := x509.MarshalPKCS8PrivateKey(p.priv)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "peer.key")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPeerPrivateKey(path); err != nil {
		t.Fatalf("owner-only key: %v", err)
	}
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644, 0o620} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadPeerPrivateKey(path); err == nil || !strings.Contains(err.Error(), "chmod 600") {
			t.Errorf("mode %04o: %v, want refused", mode, err)
		}
	}
}

// TestPeerKeyOptions: the settings are decoded, a key file needs a
// configured instance id, and bad input is refused without key material
// in the error.
func TestPeerKeyOptions(t *testing.T) {
	a := newTestPeer(t, "relay-a", "a1", PeerRoleRelay)
	der, err := x509.MarshalPKCS8PrivateKey(a.priv)
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "peer.key")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	peers := []config.HubConduitPeer{{ID: a.id, Role: "relay", Keys: []config.HubConduitPeerKey{{KeyID: a.kid, PublicKey: base64.StdEncoding.EncodeToString(a.pub)}}}}

	es, key, err := PeerKeyOptions(config.HubConduitConfig{InstanceID: a.id, PeerKeyFile: keyFile, Peers: peers})
	if err != nil || len(es) != 1 || !key.Equal(a.priv) {
		t.Fatalf("PeerKeyOptions = %d peers, key set %v, %v", len(es), key != nil, err)
	}
	if _, _, err := PeerKeyOptions(config.HubConduitConfig{PeerKeyFile: keyFile, Peers: peers}); err == nil {
		t.Error("a key file without a configured instance id was accepted")
	}
	if _, _, err := PeerKeyOptions(config.HubConduitConfig{InstanceID: a.id, PeerKeyFile: keyFile + ".missing", Peers: peers}); err == nil {
		t.Error("a missing key file was accepted")
	}
	bad := []config.HubConduitPeer{{ID: a.id, Role: "relay", Keys: []config.HubConduitPeerKey{{KeyID: a.kid, PublicKey: "not-base64!"}}}}
	if _, _, err := PeerKeyOptions(config.HubConduitConfig{Peers: bad}); err == nil {
		t.Error("a malformed public key was accepted")
	}
	if es, key, err := PeerKeyOptions(config.HubConduitConfig{}); err != nil || es != nil || key != nil {
		t.Errorf("no settings: %v, %v, %v", es, key, err)
	}
}
