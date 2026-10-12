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

package credentials

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// useTempCredentials points the credentials file at a temp directory and
// returns its path.
func useTempCredentials(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), CredentialsFile)
	orig := credentialsPath
	credentialsPath = func() string { return path }
	t.Cleanup(func() { credentialsPath = orig })
	return path
}

// writeRawCredentials writes a credentials file whose hub keys are exactly
// the given strings, as an older version that did not normalise would.
func writeRawCredentials(t *testing.T, path string, tokens map[string]string) {
	t.Helper()
	creds := Credentials{Version: 1, Hubs: map[string]*HubCredentials{}}
	for k, tok := range tokens {
		creds.Hubs[k] = &HubCredentials{AccessToken: tok, ExpiresAt: time.Now().Add(time.Hour)}
	}
	data, err := json.Marshal(creds)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, FileMode); err != nil {
		t.Fatal(err)
	}
}

func storedKeys(t *testing.T, path string) []string {
	t.Helper()
	creds, err := loadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range creds.Hubs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// A login stored by an older version under an un-normalised URL is found
// with any spelling of the same hub: trailing slash, letter case in the
// scheme and host, and the default port.
func TestLoad_NormalisedFallbackFindsExistingLogin(t *testing.T) {
	path := useTempCredentials(t)
	writeRawCredentials(t, path, map[string]string{"https://Hub.Example.com/": "old-token"})

	for _, url := range []string{
		"https://hub.example.com",
		"https://hub.example.com/",
		"HTTPS://HUB.EXAMPLE.COM",
		"https://hub.example.com:443",
		" https://Hub.Example.com/ ",
	} {
		creds, err := Load(url)
		if err != nil {
			t.Fatalf("Load(%q): %v", url, err)
		}
		if creds.AccessToken != "old-token" {
			t.Errorf("Load(%q) = %q, want old-token", url, creds.AccessToken)
		}
		if !IsAuthenticated(url) || GetAccessToken(url) != "old-token" {
			t.Errorf("IsAuthenticated/GetAccessToken(%q) did not find the login", url)
		}
	}

	// A different hub (another port, path or host) is not matched.
	for _, url := range []string{"https://hub.example.com:8443", "https://hub.example.com/other", "https://hub2.example.com"} {
		if _, err := Load(url); err != ErrNotAuthenticated {
			t.Errorf("Load(%q) = %v, want ErrNotAuthenticated", url, err)
		}
	}
}

// Store keys the login by the normalised endpoint and replaces another
// spelling of the same hub, so one hub has one login.
func TestStore_NormalisesKeyAndReplacesOtherSpellings(t *testing.T) {
	path := useTempCredentials(t)
	writeRawCredentials(t, path, map[string]string{
		"https://Hub.Example.com/":  "old-token",
		"https://other.example.com": "other-token",
	})

	if err := Store("https://hub.example.com:443/", &TokenResponse{AccessToken: "new-token", ExpiresIn: time.Hour}); err != nil {
		t.Fatal(err)
	}
	keys := storedKeys(t, path)
	want := []string{"https://hub.example.com", "https://other.example.com"}
	if len(keys) != len(want) || keys[0] != want[0] || keys[1] != want[1] {
		t.Fatalf("stored keys = %v, want %v", keys, want)
	}
	if got := GetAccessToken("https://HUB.example.com"); got != "new-token" {
		t.Errorf("token = %q, want new-token", got)
	}
}

// Remove deletes the login under every spelling of the hub URL and keeps
// other hubs.
func TestRemove_AllSpellings(t *testing.T) {
	path := useTempCredentials(t)
	writeRawCredentials(t, path, map[string]string{
		"https://Hub.Example.com/":  "a",
		"https://hub.example.com":   "b",
		"https://other.example.com": "c",
	})

	if err := Remove("https://hub.example.com:443"); err != nil {
		t.Fatal(err)
	}
	keys := storedKeys(t, path)
	if len(keys) != 1 || keys[0] != "https://other.example.com" {
		t.Fatalf("stored keys = %v, want only the other hub", keys)
	}
	if IsAuthenticated("https://hub.example.com") {
		t.Error("the removed hub is still authenticated")
	}
}
