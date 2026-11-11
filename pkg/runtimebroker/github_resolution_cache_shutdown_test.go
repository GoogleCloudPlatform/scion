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
	// A save delay far longer than the test, so the delayed write cannot
	// produce the file on its own.
	cache, err := agent.NewGitHubResolutionCache(dir, time.Hour, agent.WithResolutionCacheSaveDelay(time.Hour))
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
	// The cache's delayed write is still pending here, so the file can only
	// exist below if Shutdown wrote it.
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

// TestShutdownWaitsForGitHubResolutionRefresh checks that Shutdown waits
// for a background refresh of a stale cache entry that is still running,
// and that the refreshed entry reaches the file.
func TestShutdownWaitsForGitHubResolutionRefresh(t *testing.T) {
	dir := t.TempDir()
	// A negative TTL makes every entry stale as soon as it is written, so
	// the second resolution below serves it and starts a refresh.
	cache, err := agent.NewGitHubResolutionCache(dir, -time.Minute, agent.WithResolutionCacheSaveDelay(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	const key = "gh://o/r/s@main"
	first := func(context.Context) (agent.ResolvedSkill, error) {
		return agent.ResolvedSkill{Name: "s", URI: key, Version: "v1"}, nil
	}
	if _, err := cache.ResolveWithFetch(context.Background(), key, "flight", "cred", "ref", true, nil, first); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	refresh := func(context.Context) (agent.ResolvedSkill, error) {
		close(started)
		<-release
		return agent.ResolvedSkill{Name: "s", URI: key, Version: "v2"}, nil
	}
	got, err := cache.ResolveWithFetch(context.Background(), key, "flight", "cred", "ref", true, nil, refresh)
	if err != nil || got.Version != "v1" {
		t.Fatalf("ResolveWithFetch = %+v, %v; want the stale v1 entry", got, err)
	}
	<-started

	s := &Server{ghResolutionCache: cache}
	done := make(chan error, 1)
	go func() { done <- s.Shutdown(context.Background()) }()
	// Shutdown must still be waiting while the refresh is held; the short
	// wait only gives a Shutdown that does not wait time to return.
	select {
	case err := <-done:
		t.Fatalf("Shutdown returned (%v) while a refresh was still running", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Shutdown did not return after the refresh finished")
	}

	data, err := os.ReadFile(filepath.Join(dir, "github-resolution-cache.json"))
	if err != nil {
		t.Fatalf("cache file not written on Shutdown: %v", err)
	}
	var f struct {
		Entries map[string]struct {
			Skill agent.ResolvedSkill `json:"skill"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	if e, ok := f.Entries[key]; !ok || e.Skill.Version != "v2" {
		t.Fatalf("file entry = %+v, want the refreshed v2 entry: %s", e, data)
	}
}
