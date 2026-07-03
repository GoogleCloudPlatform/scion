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

	skipHarnessCreate := false
	if opts.SkipIfAnyExist {
		existing, err := s.store.ListHarnessConfigs(ctx, store.HarnessConfigFilter{
			Status: store.HarnessConfigStatusActive,
		}, store.ListOptions{Limit: 1})
		if err == nil && len(existing.Items) > 0 {
			s.resourceLog.Info("bundled resource bootstrap: active harness configs exist, skipping new harness-config creation but updating existing")
			skipHarnessCreate = true
		}
	}

	var errs []error
	for _, r := range resources.BuiltinResources() {
		callOpts := opts
		if skipHarnessCreate && r.Kind == storage.ResourceKindHarnessConfig {
			callOpts.SkipCreate = true
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

		result, err := rs.BootstrapSource(ctx, src, callOpts)
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
