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
	"encoding/json"
	"strings"
	"testing"
)

// The token remote travels on the hub-to-broker wire as gitClone.tokenRemote
// and is omitted when empty, so an older broker or hub sees the same payload
// as before.
func TestGitCloneConfig_TokenRemoteWire(t *testing.T) {
	data, err := json.Marshal(GitCloneConfig{URL: "https://github.com/org/repo.git", TokenRemote: "github.com/org/repo"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"tokenRemote":"github.com/org/repo"`) {
		t.Errorf("wire form = %s, want tokenRemote", data)
	}
	var back GitCloneConfig
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.TokenRemote != "github.com/org/repo" {
		t.Errorf("round trip TokenRemote = %q", back.TokenRemote)
	}
	data, err = json.Marshal(GitCloneConfig{URL: "https://github.com/org/repo.git"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "tokenRemote") {
		t.Errorf("an empty token remote must be omitted: %s", data)
	}
}
