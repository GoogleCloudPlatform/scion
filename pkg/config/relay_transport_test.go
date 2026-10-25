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

package config

import (
	"errors"
	"strings"
	"testing"
)

func TestCheckRelayTransport(t *testing.T) {
	for _, u := range []string{
		"https://10.0.0.5:9443",
		"wss://relay.internal:9443/x",
		"http://127.0.0.1:9800",
		"http://[::1]:9800",
		"http://localhost:9800",
		"ws://127.0.0.1:9800",
	} {
		if err := CheckRelayTransport(u); err != nil {
			t.Errorf("CheckRelayTransport(%q) = %v, want nil", u, err)
		}
	}
	for _, u := range []string{
		"http://10.0.0.5:9800",
		"ws://relay:9800",
		"http://0.0.0.0:9800",
		"http://[::]:9800",
		"http://localhost.:9800",
		"http://relay.localhost:9800",
		"ftp://localhost",
		"http://",
		"",
		"http://user:pw@10.0.0.5:9800",
	} {
		err := CheckRelayTransport(u)
		if !errors.Is(err, ErrPlaintextNonLoopback) {
			t.Errorf("CheckRelayTransport(%q) = %v, want ErrPlaintextNonLoopback", u, err)
			continue
		}
		if !strings.Contains(err.Error(), "internal_tls_ca_file") {
			t.Errorf("CheckRelayTransport(%q) error %q does not name internal_tls_ca_file", u, err)
		}
		if strings.Contains(err.Error(), "pw@") {
			t.Errorf("CheckRelayTransport(%q) error leaks the password: %q", u, err)
		}
	}
}

func TestCheckRelayInternalListen(t *testing.T) {
	for _, l := range []string{"127.0.0.1:9800", "[::1]:9800", "localhost:9800", "LOCALHOST:1"} {
		if err := CheckRelayInternalListen("k", l, false); err != nil {
			t.Errorf("CheckRelayInternalListen(%q, no TLS) = %v, want nil", l, err)
		}
	}
	for _, l := range []string{":9800", "0.0.0.0:9800", "[::]:9800", "10.0.0.5:9800", "relay:9800", "garbage"} {
		err := CheckRelayInternalListen("server.hub.conduit.internal_listen", l, false)
		if !errors.Is(err, ErrPlaintextNonLoopback) || !strings.Contains(err.Error(), "server.hub.conduit.internal_listen") {
			t.Errorf("CheckRelayInternalListen(%q, no TLS) = %v, want a refusal naming the setting", l, err)
		}
		if err := CheckRelayInternalListen("k", l, true); err != nil {
			t.Errorf("CheckRelayInternalListen(%q, TLS) = %v, want nil", l, err)
		}
	}
}
