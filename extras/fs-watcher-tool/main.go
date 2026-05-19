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

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/GoogleCloudPlatform/scion/extras/fs-watcher-tool/pkg/fswatcher"
	"github.com/docker/docker/client"
)

type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ", ") }
func (s *stringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func main() {
	var (
		grove      string
		watchDirs  stringSlice
		logFile    string
		labelKey   string
		ignore     stringSlice
		filterFile string
		debounce   time.Duration
		cacheTTL   time.Duration
		debug      bool
	)

	flag.StringVar(&grove, "grove", "", "Grove ID — auto-discover agent directories via Docker labels")
	flag.Var(&watchDirs, "watch", "Directory to watch explicitly (repeatable)")
	flag.StringVar(&logFile, "log", "-", "Output log file path (- for stdout)")
	flag.StringVar(&labelKey, "label-key", "scion.name", "Docker label key to use as agent ID")
	flag.Var(&ignore, "ignore", "Glob patterns to exclude (repeatable)")
	flag.StringVar(&filterFile, "filter-file", "", "Path to .gitignore-style filter file")
	flag.DurationVar(&debounce, "debounce", 300*time.Millisecond, "Duration to collapse rapid edits to the same file")
	flag.DurationVar(&cacheTTL, "cache-ttl", 5*time.Minute, "Duration to cache PID-to-container mappings")
	flag.BoolVar(&debug, "debug", false, "Enable verbose debug logging to stderr")
	flag.Parse()

	if grove == "" && len(watchDirs) == 0 {
		fmt.Fprintln(os.Stderr, "error: at least one of --grove or --watch is required")
		flag.Usage()
		os.Exit(1)
	}

	// Default ignore pattern.
	if len(ignore) == 0 {
		ignore = stringSlice{".git/**"}
	}

	cfg := fswatcher.Config{
		Grove:      grove,
		WatchDirs:  watchDirs,
		LogFile:    logFile,
		LabelKey:   labelKey,
		Ignore:     ignore,
		FilterFile: filterFile,
		Debounce:   debounce,
		CacheTTL:   cacheTTL,
		Debug:      debug,
	}

	if err := run(cfg); err != nil {
		log.Fatalf("fatal: %v", err)
	}
}

func run(cfg fswatcher.Config) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle signals.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	// Set up output.
	var out io.Writer
	if cfg.LogFile == "-" || cfg.LogFile == "" {
		out = os.Stdout
	} else {
		f, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return fmt.Errorf("opening log file: %w", err)
		}
		defer f.Close()
		out = f
	}

	logger := fswatcher.NewLogger(out)

	// Set up filter.
	filter, err := fswatcher.NewFilter(cfg.Ignore, cfg.FilterFile)
	if err != nil {
		return fmt.Errorf("initializing filter: %w", err)
	}

	// Set up Docker client.
	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("creating docker client: %w", err)
	}
	defer dockerClient.Close()

	if cfg.Debug {
		log.Printf("[docker] connected to docker daemon at %s", dockerClient.DaemonHost())
		info, infoErr := dockerClient.Info(ctx)
		if infoErr != nil {
			log.Printf("[docker] warning: could not query docker info: %v", infoErr)
		} else {
			log.Printf("[docker] server version: %s, containers: %d running / %d total",
				info.ServerVersion, info.ContainersRunning, info.Containers)
			log.Printf("[docker] cgroup driver: %s, cgroup version: %s",
				info.CgroupDriver, info.CgroupVersion)
		}
	}

	// Set up resolver.
	resolver := fswatcher.NewResolver(dockerClient, cfg.LabelKey, cfg.CacheTTL, cfg.Debug)
	if err := resolver.Warmup(ctx); err != nil {
		log.Printf("warning: resolver warmup failed: %v", err)
	}

	// Collect watch directories.
	roots := make([]string, len(cfg.WatchDirs))
	copy(roots, cfg.WatchDirs)

	var groveDiscovery *fswatcher.GroveDiscovery
	if cfg.Grove != "" {
		groveDiscovery = fswatcher.NewProjectDiscovery(dockerClient, cfg.Grove, cfg.Debug)
		groveDirs, err := groveDiscovery.Discover(ctx)
		if err != nil {
			return fmt.Errorf("grove discovery: %w", err)
		}
		roots = append(roots, groveDirs...)
	}

	if len(roots) == 0 && groveDiscovery == nil {
		return fmt.Errorf("no directories to watch (no --watch paths specified)")
	}
	if len(roots) == 0 {
		log.Printf("no agent containers running yet for grove %q; waiting for containers to start", cfg.Grove)
	}

	if cfg.Debug {
		log.Printf("[config] grove=%q, label-key=%q, debounce=%s, cache-ttl=%s",
			cfg.Grove, cfg.LabelKey, cfg.Debounce, cfg.CacheTTL)
		log.Printf("[config] ignore patterns: %v", cfg.Ignore)
		if cfg.FilterFile != "" {
			log.Printf("[config] filter file: %s", cfg.FilterFile)
		}
		log.Printf("[config] log output: %s", cfg.LogFile)
		for i, dir := range roots {
			log.Printf("[config] watch root [%d]: %s", i, dir)
		}
	}

	// Build and start watcher.
	watcher := fswatcher.NewWatcher(cfg, roots, filter, resolver, logger)

	// Subscribe to container events for cache updates and dynamic grove discovery.
	var onStart func(string)
	if groveDiscovery != nil {
		onStart = func(containerID string) {
			dir, err := groveDiscovery.DiscoverForContainer(ctx, containerID)
			if err != nil || dir == "" {
				return
			}
			added, err := watcher.AddRoot(dir)
			if err != nil {
				log.Printf("warning: failed to add watch for new container dir %s: %v", dir, err)
			} else if added && cfg.Debug {
				log.Printf("[grove] added watch for new container dir: %s", dir)
			}
		}
	}
	resolver.WatchContainerEvents(ctx, onStart, nil)

	if cfg.Debug {
		log.Printf("[watcher] subscribed to docker container lifecycle events (start/die)")
	}

	// Run event loop in a goroutine.
	errCh := make(chan error, 1)
	go func() {
		errCh <- watcher.Run(ctx)
	}()

	log.Printf("scion-fs-watcher started, watching %d directories", len(roots))

	// Wait for signal or error.
	for {
		select {
		case sig := <-sigCh:
			if sig == syscall.SIGHUP {
				log.Println("received SIGHUP, reloading filter file")
				if err := filter.Reload(cfg.Ignore, cfg.FilterFile); err != nil {
					log.Printf("warning: filter reload failed: %v", err)
				}
				continue
			}
			log.Printf("received %s, shutting down", sig)
			cancel()
			return <-errCh
		case err := <-errCh:
			return err
		}
	}
}
