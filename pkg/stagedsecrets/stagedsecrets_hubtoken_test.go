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

package stagedsecrets

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const hubTokenCanary = "fake-hub-token-canary-7239-not-a-credential"

func TestDecode_HubTokenRoundTrip(t *testing.T) {
	data, err := json.Marshal(Staged{HubToken: hubTokenCanary})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"hub_token"`) {
		t.Fatalf("hub_token key missing from the blob JSON")
	}
	got, err := Decode(base64.StdEncoding.EncodeToString(data))
	if err != nil {
		t.Fatal(err)
	}
	if got.HubToken != hubTokenCanary {
		t.Error("HubToken lost in the round trip")
	}

	empty, err := json.Marshal(Staged{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(empty), "hub_token") {
		t.Error("hub_token emitted for an empty token")
	}
}

// Formatting a Staged never prints a value.
func TestStaged_FormattingWithholdsValues(t *testing.T) {
	s := Staged{
		HubToken:        hubTokenCanary,
		VariableSecrets: map[string]string{"K": hubTokenCanary},
		FileSecrets:     []FileSecret{{Name: "f", Target: "/x", Value: hubTokenCanary}},
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		if out := fmt.Sprintf(verb, s); strings.Contains(out, hubTokenCanary) {
			t.Errorf("%s of Staged prints a value", verb)
		}
		if out := fmt.Sprintf(verb, &s); strings.Contains(out, hubTokenCanary) {
			t.Errorf("%s of *Staged prints a value", verb)
		}
	}
}

// Write ignores HubToken: it writes no file for it.
func TestWrite_IgnoresHubToken(t *testing.T) {
	home := t.TempDir()
	if err := writeAs(home, &Staged{HubToken: hubTokenCanary}, 0, 0); err != nil {
		t.Fatalf("writeAs: %v", err)
	}
	assertNoFileContains(t, home, hubTokenCanary)
}

func assertNoFileContains(t *testing.T, root, needle string) {
	t.Helper()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(b), needle) {
			t.Errorf("file %s holds the hub credential", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
