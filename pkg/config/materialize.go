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
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/resources"
)

// MaterializeOptions controls the behavior of MaterializeBundledResources.
type MaterializeOptions struct {
	Force bool
}

// MaterializeBundledTemplates writes only the bundled Templates to the local
// filesystem. Use this when harness-config materialization is handled
// separately (e.g. selective materialization in handleSystemInit).
func MaterializeBundledTemplates(globalDir string, opts MaterializeOptions) error {
	for _, res := range resources.BuiltinTemplates() {
		targetDir := filepath.Join(globalDir, "templates", res.Name)
		if err := materializeTemplateFromFS(targetDir, res.FS, res.Root, opts.Force); err != nil {
			return fmt.Errorf("failed to materialize template %q: %w", res.Name, err)
		}
	}
	return nil
}

// MaterializeBundledResources writes bundled Templates and Harness-configs to
// the local filesystem for workstation compatibility. It uses the same
// resources.BuiltinResources() catalog as the hosted bootstrap path.
func MaterializeBundledResources(globalDir string, opts MaterializeOptions) error {
	for _, res := range resources.BuiltinResources() {
		switch res.Kind {
		case storage.ResourceKindTemplate:
			targetDir := filepath.Join(globalDir, "templates", res.Name)
			if err := materializeTemplateFromFS(targetDir, res.FS, res.Root, opts.Force); err != nil {
				return fmt.Errorf("failed to materialize template %q: %w", res.Name, err)
			}
		case storage.ResourceKindHarnessConfig:
			targetDir := filepath.Join(globalDir, harnessConfigsDirName, res.Name)
			if err := SeedHarnessConfigFromDir(targetDir, res.FS, res.Root, opts.Force); err != nil {
				return fmt.Errorf("failed to materialize harness-config %q: %w", res.Name, err)
			}
		}
	}
	return nil
}

// MaterializeSelectedHarnessConfigs materializes only the named harness-configs
// from the bundled catalog. Returns an error if a requested name is not found
// in the catalog.
func MaterializeSelectedHarnessConfigs(globalDir string, names []string, opts MaterializeOptions) error {
	catalog := make(map[string]resources.BundledResource)
	for _, res := range resources.BuiltinHarnessConfigs() {
		catalog[res.Name] = res
	}

	for _, name := range names {
		res, ok := catalog[name]
		if !ok {
			return fmt.Errorf("harness-config %q is not in the bundled catalog", name)
		}
		targetDir := filepath.Join(globalDir, harnessConfigsDirName, res.Name)
		if err := SeedHarnessConfigFromDir(targetDir, res.FS, res.Root, opts.Force); err != nil {
			return fmt.Errorf("failed to materialize harness-config %q: %w", name, err)
		}
	}
	return nil
}

// materializeTemplateFromFS writes a template from an fs.FS to the target
// directory. It walks all files and directories, preserving existing files
// unless force is true.
func materializeTemplateFromFS(targetDir string, srcFS fs.FS, root string, force bool) error {
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return fmt.Errorf("failed to create template directory %s: %w", targetDir, err)
	}

	return fs.WalkDir(srcFS, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(root, filepath.ToSlash(path))
		if err != nil {
			return fmt.Errorf("failed to compute relative path for %s: %w", path, err)
		}

		if relPath == "." {
			return nil
		}

		targetPath := filepath.Join(targetDir, relPath)

		if d.IsDir() {
			if err := os.MkdirAll(targetPath, 0755); err != nil {
				return fmt.Errorf("failed to create directory %s: %w", targetPath, err)
			}
			return nil
		}

		data, err := fs.ReadFile(srcFS, path)
		if err != nil {
			return fmt.Errorf("failed to read file %s: %w", path, err)
		}

		if !force {
			if _, err := os.Stat(targetPath); err == nil {
				return nil
			}
		}

		if err := os.WriteFile(targetPath, data, 0644); err != nil {
			return fmt.Errorf("failed to write file %s: %w", targetPath, err)
		}

		return nil
	})
}
