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

package hub

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/resources"
)

// BootstrapBundledResources iterates all built-in bundled resources and
// bootstraps each into the Hub's database and storage backend. Templates are
// routed to templateStore and harness-configs to harnessConfigStore. The
// operation is idempotent: unchanged resources are skipped, and updated
// built-in-managed resources are re-uploaded.
func (s *Server) BootstrapBundledResources(ctx context.Context, opts BootstrapOptions) error {
	stor := s.GetStorage()
	if stor == nil {
		s.resourceLog.Warn("bundled resource bootstrap: no storage backend configured, skipping")
		return nil
	}

	skipHarnessConfigs := false
	if opts.SkipIfAnyExist {
		existing, err := s.store.ListHarnessConfigs(ctx, store.HarnessConfigFilter{
			Status: store.HarnessConfigStatusActive,
		}, store.ListOptions{Limit: 1})
		if err == nil && len(existing.Items) > 0 {
			s.resourceLog.Info("bundled resource bootstrap: active harness configs exist, skipping harness-config seeding")
			skipHarnessConfigs = true
		}
	}

	var errs []error
	for _, r := range resources.BuiltinResources() {
		if skipHarnessConfigs && r.Kind == storage.ResourceKindHarnessConfig {
			continue
		}

		src := NewFSResourceSource(r)

		var rs *ResourceStore
		switch r.Kind {
		case storage.ResourceKindTemplate:
			rs = s.templateStore()
		case storage.ResourceKindHarnessConfig:
			harness, err := resolveHarnessType(r)
			if err != nil {
				errs = append(errs, fmt.Errorf("bootstrap %s %q: %w", r.Kind, r.Name, err))
				continue
			}
			rs = s.harnessConfigStore(harness)
		default:
			errs = append(errs, fmt.Errorf("bootstrap: unsupported resource kind %q", r.Kind))
			continue
		}

		result, err := rs.BootstrapSource(ctx, src, opts)
		if err != nil {
			errs = append(errs, fmt.Errorf("bootstrap %s %q: %w", r.Kind, r.Name, err))
			continue
		}

		s.resourceLog.Info("bootstrapped bundled resource",
			"kind", r.Kind, "name", r.Name,
			"created", result.Created, "updated", result.Updated,
			"repaired", result.Repaired, "skipped", result.Skipped,
			"failed", result.Failed)
	}

	if err := s.ArchiveObsoleteBundledHarnessConfigs(ctx); err != nil {
		errs = append(errs, fmt.Errorf("archive obsolete bundled harness-configs: %w", err))
	}

	return errors.Join(errs...)
}

// resolveHarnessType reads config.yaml from a bundled harness-config resource's
// embedded FS and returns the harness type field.
func resolveHarnessType(r resources.BundledResource) (string, error) {
	configPath := filepath.ToSlash(filepath.Join(r.Root, "config.yaml"))
	if r.Root == "." || r.Root == "" {
		configPath = "config.yaml"
	}

	data, err := fs.ReadFile(r.FS, configPath)
	if err != nil {
		return "", fmt.Errorf("read config.yaml: %w", err)
	}

	entry, err := config.ParseHarnessConfigYAML(data)
	if err != nil {
		return "", fmt.Errorf("parse config.yaml: %w", err)
	}

	if entry.Harness == "" {
		return "", fmt.Errorf("config.yaml missing harness field")
	}
	return entry.Harness, nil
}

// ArchiveObsoleteBundledHarnessConfigs archives global harness-configs that
// were originally bootstrapped from bundled resources but whose bundled source
// no longer exists in the current binary. This prevents stale configs (e.g. a
// removed harness like "gemini") from persisting indefinitely via local disk
// copies at ~/.scion/harness-configs/.
func (s *Server) ArchiveObsoleteBundledHarnessConfigs(ctx context.Context) error {
	bundledNames := make(map[string]struct{})
	for _, name := range resources.BuiltinHarnessConfigNames() {
		bundledNames[name] = struct{}{}
	}

	existing, err := s.store.ListHarnessConfigs(ctx, store.HarnessConfigFilter{
		Scope:  store.HarnessConfigScopeGlobal,
		Status: store.HarnessConfigStatusActive,
	}, store.ListOptions{Limit: 1000})
	if err != nil {
		return fmt.Errorf("list global harness configs: %w", err)
	}

	archived := 0
	for _, hc := range existing.Items {
		if !IsBuiltinManaged(hc.SourceURL) {
			continue
		}
		if _, ok := bundledNames[hc.Name]; ok {
			continue
		}

		hc.Status = store.HarnessConfigStatusArchived
		if err := s.store.UpdateHarnessConfig(ctx, &hc); err != nil {
			s.resourceLog.Warn("failed to archive obsolete bundled harness-config",
				"name", hc.Name, "id", hc.ID, "error", err)
			continue
		}
		s.resourceLog.Info("archived obsolete bundled harness-config",
			"name", hc.Name, "id", hc.ID, "sourceUrl", hc.SourceURL)
		archived++
	}

	if archived > 0 {
		s.resourceLog.Info("archived obsolete bundled harness-configs", "count", archived)
	}
	return nil
}

// checkAndUpdateImageStatus resolves the image registry, checks image
// availability, and persists the result.
func (s *Server) checkAndUpdateImageStatus(ctx context.Context, hcID, image string) {
	registry := s.resolveImageRegistry()
	resolvedImage := config.RewriteImageRegistry(image, registry)

	result := s.imageChecker.CheckRemoteOnly(ctx, resolvedImage)
	if result.Error != "" {
		slog.Warn("image status check returned error", "id", hcID, "image", image, "resolved", resolvedImage, "status", result.Status, "error", result.Error)
	}

	if err := s.store.UpdateHarnessConfigImageStatus(ctx, hcID, result.Status, result.CheckedAt); err != nil {
		slog.Error("failed to update image status", "id", hcID, "error", err)
	}
}

// resolveImageRegistry returns the configured image registry from the
// server's in-memory config, which includes environment variable overrides
// applied at startup. Falls back to SCION_IMAGE_REGISTRY env var if the
// maintenance config value is empty.
//
// TODO: use an internal settings API that returns fully-resolved settings
// (env var overrides + DB values) once available — needed for HA mode where
// settings will live in the database.
func (s *Server) resolveImageRegistry() string {
	s.mu.RLock()
	r := s.config.MaintenanceConfig.ImageRegistry
	s.mu.RUnlock()
	if r != "" {
		return r
	}
	return os.Getenv("SCION_IMAGE_REGISTRY")
}

// RecheckAllImageStatuses is a no-op. Remote registry checks are now
// on-demand only (via the image-status endpoint). Retained as a stub
// for callers that invoke it at startup.
func (s *Server) RecheckAllImageStatuses(_ context.Context) {}
