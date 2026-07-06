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

package templateimport

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"gopkg.in/yaml.v3"
)

const scionConfigFile = "scion-agent.yaml"

// IsScionTemplate returns true if the directory contains a scion-agent.yaml file,
// indicating it is a native scion template.
func IsScionTemplate(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, scionConfigFile))
	return err == nil && !info.IsDir()
}

// IsScionTemplatesDir returns true if the directory contains subdirectories
// that are scion templates (i.e., contain scion-agent.yaml).
func IsScionTemplatesDir(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() && IsScionTemplate(filepath.Join(dir, e.Name())) {
			return true
		}
	}
	return false
}

// DiscoverScionTemplates scans a directory for scion-format template subdirectories.
// Each subdirectory containing scion-agent.yaml is treated as a template.
func DiscoverScionTemplates(dir string) ([]*ImportedAgent, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to read directory: %w", err)
	}

	var agents []*ImportedAgent
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		templateDir := filepath.Join(dir, e.Name())
		if !IsScionTemplate(templateDir) {
			continue
		}

		agent, err := parseScionTemplate(templateDir, e.Name())
		if err != nil {
			continue // skip templates that fail to parse
		}
		agents = append(agents, agent)
	}
	return agents, nil
}

// ParseScionTemplate reads a single scion-format template directory and returns
// an ImportedAgent with metadata from scion-agent.yaml.
func ParseScionTemplate(dir string) (*ImportedAgent, error) {
	return parseScionTemplate(dir, filepath.Base(dir))
}

// scionTemplateConfig extends ScionConfig with the description field that is
// present in scion-agent.yaml but not mapped in the API struct.
type scionTemplateConfig struct {
	api.ScionConfig `yaml:",inline"`
	Description     string `yaml:"description"`
}

func parseScionTemplate(dir, name string) (*ImportedAgent, error) {
	configPath := filepath.Join(dir, scionConfigFile)
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", scionConfigFile, err)
	}

	var cfg scionTemplateConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse %s: %w", scionConfigFile, err)
	}

	harness := cfg.DefaultHarnessConfig
	if harness == "" {
		harness = cfg.Harness
	}
	if harness == "" {
		harness = "scion"
	}

	description := cfg.Description
	if description == "" {
		description = "Scion template"
	}

	return &ImportedAgent{
		Name:        name,
		Description: description,
		Harness:     harness,
		Model:       cfg.Model,
		SourcePath:  dir,
		ScionFormat: true,
	}, nil
}

// CopyScionTemplate copies a scion template directory tree to the destination.
func CopyScionTemplate(srcDir, destDir string, force bool) (string, error) {
	if !force {
		if _, err := os.Stat(destDir); err == nil {
			return "", fmt.Errorf("template '%s' already exists at %s (use --force to overwrite)", filepath.Base(destDir), destDir)
		}
	}

	if force {
		_ = os.RemoveAll(destDir)
	}

	if err := copyDir(srcDir, destDir); err != nil {
		return "", fmt.Errorf("failed to copy template: %w", err)
	}

	return destDir, nil
}

// copyDir recursively copies a directory tree.
func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		destPath := filepath.Join(dst, relPath)

		if d.IsDir() {
			return os.MkdirAll(destPath, 0755)
		}

		return copyFile(path, destPath)
	})
}

// copyFile copies a single file, preserving permissions.
func copyFile(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = srcFile.Close() }()

	srcInfo, err := srcFile.Stat()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}

	dstFile, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, srcInfo.Mode())
	if err != nil {
		return err
	}
	defer func() { _ = dstFile.Close() }()

	_, err = io.Copy(dstFile, srcFile)
	return err
}
