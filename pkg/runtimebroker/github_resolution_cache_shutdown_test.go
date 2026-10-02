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

package runtimebroker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
)

// TestShutdownFlushesGitHubResolutionCache checks that an entry still
// waiting for the cache's delayed write reaches the file on Shutdown, also
// when the server never started its HTTP listener.
func TestShutdownFlushesGitHubResolutionCache(t *testing.T) {
	dir := t.TempDir()
	cache, err := agent.NewGitHubResolutionCache(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	const key = "gh://o/r/s@main"
	fetch := func(context.Context) (agent.ResolvedSkill, error) {
		return agent.ResolvedSkill{Name: "s", URI: key}, nil
	}
	if _, err := cache.ResolveWithFetch(context.Background(), key, "flight", "cred", "ref", true, nil, fetch); err != nil {
		t.Fatal(err)
	}
	// The cache's delayed write (2s) is still pending here; Shutdown runs
	// well within it, so the file can only exist below if Shutdown wrote it.
	cacheFile := filepath.Join(dir, "github-resolution-cache.json")

	s := &Server{ghResolutionCache: cache}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	data, err := os.ReadFile(cacheFile)
	if err != nil {
		t.Fatalf("cache file not written on Shutdown: %v", err)
	}
	var f struct {
		Entries map[string]json.RawMessage `json:"entries"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.Entries[key]; !ok {
		t.Fatalf("entry missing from cache file after Shutdown: %s", data)
	}
}
