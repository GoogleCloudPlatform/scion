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

func TestCheckHubTransport(t *testing.T) {
	accept := []string{
		"https://hub.example.com",
		"https://10.0.0.5:8443/",
		"HTTPS://hub.example.com",
		"wss://hub.example.com",
		"http://localhost:8080",
		"http://127.0.0.1:9800",
		"http://127.8.9.10",
		"http://[::1]:8080",
		"ws://127.0.0.1:8080",
		"HTTP://LOCALHOST:8080",
	}
	for _, u := range accept {
		if err := CheckHubTransport(u); err != nil {
			t.Errorf("CheckHubTransport(%q) = %v, want nil", u, err)
		}
	}
	refuse := []string{
		"http://10.0.0.5:8080",
		"http://hub.example.com",
		"ws://hub.example.com",
		"http://0.0.0.0:8080",
		"http://[::]:8080",
		"http://localhost.:8080",
		"http://hub.localhost:8080",
		"http://192.0.2.10:9800",
		"ftp://localhost",
		"unix://localhost",
		"localhost:8080",
		"",
		"http://",
		"://bad",
	}
	for _, u := range refuse {
		err := CheckHubTransport(u)
		if err == nil {
			t.Errorf("CheckHubTransport(%q) = nil, want a refusal", u)
			continue
		}
		if !errors.Is(err, ErrPlaintextNonLoopback) {
			t.Errorf("CheckHubTransport(%q) = %v, want it to wrap ErrPlaintextNonLoopback", u, err)
		}
	}
}

func TestCheckHubTransport_MessageNamesFixAndRedacts(t *testing.T) {
	err := CheckHubTransport("http://user:s3cret@hub.example.com:8080")
	if err == nil {
		t.Fatal("want a refusal")
	}
	msg := err.Error()
	if strings.Contains(msg, "s3cret") {
		t.Errorf("message leaks the URL password: %s", msg)
	}
	for _, want := range []string{"hub.example.com", "not a loopback address", "https://", "127.0.0.1"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not contain %q", msg, want)
		}
	}
}

// A URL that does not parse and holds an '@' is not echoed: the masked form
// would show only what follows the '@', which can look like a loopback
// host. A parsed URL names its real host separately.
func TestCheckHubTransport_MessageDoesNotShowMisleadingHost(t *testing.T) {
	err := CheckHubTransport(`http://evil.example\@127.0.0.1`)
	if err == nil {
		t.Fatal("want a refusal")
	}
	if strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("message %q shows the text after '@' as if it were the host", err)
	}
	err = CheckHubTransport("http://127.0.0.1@evil.example:8080")
	if err == nil {
		t.Fatal("want a refusal")
	}
	if !strings.Contains(err.Error(), `host "evil.example"`) {
		t.Errorf("message %q does not name the parsed host", err)
	}
}
