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
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/templatecache"
)

// skillCacheMaxSize is the skill cache's size bound.
const skillCacheMaxSize = int64(500 * 1024 * 1024)

// fileCaches are a Runtime Broker's content-addressed file caches.
type fileCaches struct {
	templates, harnessConfigs, skills *templatecache.Cache
}

// defaultTemplateCacheDir is the template cache directory a Runtime Broker
// uses when none is configured: ~/.scion/cache/templates.
func defaultTemplateCacheDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(home, ".scion", "cache", "templates"), nil
}

// openFileCaches opens the template cache in templateDir and the
// harness-config and skill caches in its sibling directories harness-configs
// and skills. A maxSize of zero or less means templatecache.DefaultMaxSize.
func openFileCaches(templateDir string, maxSize int64) (fileCaches, error) {
	if maxSize <= 0 {
		maxSize = templatecache.DefaultMaxSize
	}
	var fc fileCaches
	var err error
	if fc.templates, err = templatecache.New(templateDir, maxSize); err != nil {
		return fileCaches{}, fmt.Errorf("failed to initialize template cache: %w", err)
	}
	if fc.harnessConfigs, err = templatecache.New(filepath.Join(filepath.Dir(templateDir), "harness-configs"), maxSize); err != nil {
		return fileCaches{}, fmt.Errorf("failed to initialize harness-config cache: %w", err)
	}
	if fc.skills, err = templatecache.New(filepath.Join(filepath.Dir(templateDir), "skills"), skillCacheMaxSize); err != nil {
		return fileCaches{}, fmt.Errorf("failed to initialize skill cache: %w", err)
	}
	return fc, nil
}

// openGitHubResolutionCache opens the GitHub resolution cache, or returns
// nil (resolution then runs uncached) when it cannot be opened.
func openGitHubResolutionCache() *agent.GitHubResolutionCache {
	dir, err := agent.GitHubResolutionCacheDir()
	if err != nil {
		slog.Warn("github resolution cache: cannot determine cache dir", "error", err)
		return nil
	}
	gh, err := agent.NewGitHubResolutionCache(dir, agent.DefaultResolutionCacheTTL)
	if err != nil {
		slog.Warn("github resolution cache: init failed (running uncached)", "error", err)
		return nil
	}
	slog.Info("GitHub resolution cache initialized", "dir", dir, "ttl", agent.DefaultResolutionCacheTTL)
	return gh
}

// SharedCaches are a flat host's broker caches (ptone/scion#3274). Two
// cache objects on one directory would keep separate indexes and evict
// each other's files, so every directory has exactly one object. The choice
// per cache:
//
//   - templates, harness-configs and skills (templatecache): partitioned
//     per instance. Each instance has its own object on its own
//     directories, <cache root>/instances/<broker ID>/{templates,
//     harness-configs,skills}. A path these caches return is read after
//     the call returns (a start copies a template later), so a shared
//     object would let another instance's insert evict a path a start is
//     about to read. Partitioned, eviction in an object only races the
//     same instance's own starts, as for a single Runtime Broker. The cost
//     is that instances do not share downloads and each partition has the
//     full size bound (twice TemplateCacheMaxSize plus 500 MB for skills).
//     A partition is not reclaimed when its instance is removed from the
//     configuration or recreated with a new broker ID; its directory stays
//     on disk until removed by hand.
//   - GitHub resolution (agent.GitHubResolutionCache, metadata only): one
//     object shared by every instance; its own locks serialize reads,
//     refreshes and the delayed write of its file. The host closes it once,
//     after every instance has shut down (SharedCaches is a
//     brokerhost.Service).
type SharedCaches struct {
	root    string // the cache root: the parent of a single broker's template directory
	maxSize int64
	GitHub  *agent.GitHubResolutionCache // nil when it cannot be opened (resolution runs uncached)

	mu        sync.Mutex
	instances map[string]fileCaches // by broker ID
}

// NewSharedCaches prepares a flat host's caches. templateDir is the
// template directory a single Runtime Broker would use (default
// ~/.scion/cache/templates); the per-instance partitions go under its
// parent. It opens the shared GitHub resolution cache.
func NewSharedCaches(templateDir string, maxSize int64) (*SharedCaches, error) {
	if templateDir == "" {
		var err error
		if templateDir, err = defaultTemplateCacheDir(); err != nil {
			return nil, err
		}
	}
	return &SharedCaches{
		root:      filepath.Dir(templateDir),
		maxSize:   maxSize,
		GitHub:    openGitHubResolutionCache(),
		instances: map[string]fileCaches{},
	}, nil
}

// instanceTemplateDir is the template directory of the instance's
// partition.
func (c *SharedCaches) instanceTemplateDir(brokerID string) (string, error) {
	if brokerID == "" || brokerID == "." || brokerID == ".." || filepath.Base(brokerID) != brokerID {
		return "", fmt.Errorf("broker ID %q is not a single path element", brokerID)
	}
	return filepath.Join(c.root, "instances", brokerID, "templates"), nil
}

// forInstance returns the file caches of the instance's partition, opening
// them on first use. Every call for one broker ID returns the same objects.
func (c *SharedCaches) forInstance(brokerID string) (fileCaches, error) {
	dir, err := c.instanceTemplateDir(brokerID)
	if err != nil {
		return fileCaches{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if fc, ok := c.instances[brokerID]; ok {
		return fc, nil
	}
	fc, err := openFileCaches(dir, c.maxSize)
	if err != nil {
		return fileCaches{}, err
	}
	c.instances[brokerID] = fc
	slog.Info("Broker caches initialized", "cache", dir, "broker_id", brokerID)
	return fc, nil
}

// Start implements brokerhost.Service (the shared cache is already open).
func (c *SharedCaches) Start(context.Context) error { return nil }

// Stop closes the GitHub resolution cache once, after every instance has
// shut down (brokerhost.Service): it waits, within a bound, for background
// refreshes and writes entries still waiting for their delayed write.
func (c *SharedCaches) Stop(ctx context.Context) {
	if c.GitHub == nil {
		return
	}
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ghResolutionCacheCloseTimeout)
	defer cancel()
	if err := c.GitHub.Close(closeCtx); err != nil {
		slog.Warn("GitHub resolution cache closed before background refreshes finished", "error", err)
	}
}
